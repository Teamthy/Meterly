package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/authz"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const (
	resourceOrganisation = "organisation"
	resourceProperty     = "property"
	resourceUnit         = "unit"
	resourceUnitMeter    = "unit_meter"
	resourceBulkTopup    = "bulk_topup"
	opCreateBulkTopup    = "org_bulk_topup"
)

var (
	ErrUserNotFound       = errors.New("user not found")
	ErrMemberNotFound     = errors.New("organisation member not found")
	ErrUnitMeterNotFound  = errors.New("unit meter not found")
	ErrUnitHasNoMeter     = errors.New("unit has no active meter attached")
	ErrInvalidCountryCode = errors.New("country must be a 2-letter ISO code")
	ErrInvalidDelegation  = errors.New("delegated permission must be VIEW or PURCHASE")
)

// WithPowerDeskCaller binds userID and the store-backed organisation role
// lookup to ctx so every Power Desk operation enforces RBAC via authz.Require.
func (o *Orchestrator) WithPowerDeskCaller(ctx context.Context, userID string) context.Context {
	return authz.WithCaller(ctx, userID, o.lookupOrgRole)
}

func (o *Orchestrator) lookupOrgRole(ctx context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error) {
	r, ok := store.ReposFromContext(ctx)
	if !ok {
		r = o.st.Repos()
	}
	role, err := r.PowerDesk().GetActiveMemberRole(ctx, orgID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return "", authz.ErrNotFound
	}
	return role, err
}

func mustCallerUID(ctx context.Context) (string, error) {
	uid, ok := authz.CallerUserID(ctx)
	if !ok {
		return "", authz.ErrUnauthenticated
	}
	return uid, nil
}

// ---- Organisations & Members -----------------------------------------------

type CreateOrgParams struct {
	Name                          string
	LegalName                     string
	RegistrationNumber            string
	OrganisationType              powerdesk.OrganisationType
	ManagerApprovalThresholdMinor *int64
}

type UpdateOrgParams struct {
	Name                          *string
	LegalName                     *string
	RegistrationNumber            *string
	ManagerApprovalThresholdMinor *int64
	ClearManagerApprovalThreshold bool
}

type UnitView struct {
	powerdesk.Unit
	UnitMeter         *powerdesk.UnitMeter `json:"unit_meter,omitempty"`
	MaskedMeterNumber string               `json:"masked_meter_number,omitempty"`
	Disco             string               `json:"disco,omitempty"`
}

type OrgDetailView struct {
	powerdesk.Organisation
	CallerRole powerdesk.Role        `json:"caller_role"`
	Members    []*powerdesk.Member   `json:"members"`
	Properties []*powerdesk.Property `json:"properties"`
	Units      []UnitView            `json:"units"`
}

func (o *Orchestrator) CreateOrg(ctx context.Context, p CreateOrgParams) (*OrgDetailView, error) {
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	name, err := powerdesk.CleanName(p.Name, powerdesk.ErrInvalidOrgName)
	if err != nil {
		return nil, err
	}
	orgType := p.OrganisationType
	if orgType == "" {
		orgType = powerdesk.OrgLandlord
	}
	if !orgType.Valid() {
		return nil, powerdesk.ErrInvalidOrgType
	}
	var threshold *int64
	if p.ManagerApprovalThresholdMinor != nil {
		if *p.ManagerApprovalThresholdMinor < 0 {
			return nil, powerdesk.ErrInvalidThreshold
		}
		v := *p.ManagerApprovalThresholdMinor
		threshold = &v
	}

	now := o.Now().UTC()
	org := &powerdesk.Organisation{
		ID:                            uuid.New(),
		Name:                          name,
		LegalName:                     strings.TrimSpace(p.LegalName),
		RegistrationNumber:            strings.TrimSpace(p.RegistrationNumber),
		OrganisationType:              orgType,
		VerificationStatus:            powerdesk.VerificationPending,
		ManagerApprovalThresholdMinor: threshold,
		CreatedAt:                     now,
		UpdatedAt:                     now,
	}

	err = o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.PowerDesk().CreateOrg(ctx, org); err != nil {
			return err
		}
		if err := r.PowerDesk().UpsertMember(ctx, &powerdesk.Member{
			OrganisationID: org.ID,
			UserID:         callerUID,
			Role:           powerdesk.RoleOwner,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("organisation.created", resourceOrganisation, org.ID, map[string]any{
			"organisation_type": org.OrganisationType,
		}))
	})
	if err != nil {
		return nil, err
	}
	return o.GetOrg(ctx, org.ID)
}

func (o *Orchestrator) GetOrg(ctx context.Context, orgID uuid.UUID) (*OrgDetailView, error) {
	role, err := authz.RequireRole(ctx, orgID, powerdesk.RoleViewer)
	if err != nil {
		return nil, err
	}
	org, err := o.st.Repos().PowerDesk().GetOrg(ctx, orgID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	members, err := o.st.Repos().PowerDesk().ListActiveMembers(ctx, orgID)
	if err != nil {
		return nil, err
	}
	props, err := o.st.Repos().PowerDesk().ListPropertiesForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	units, err := o.st.Repos().PowerDesk().ListUnitsForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	ums, err := o.st.Repos().PowerDesk().ListUnitMetersForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	umByUnit := make(map[uuid.UUID]*powerdesk.UnitMeter, len(ums))
	for _, um := range ums {
		umByUnit[um.UnitID] = um
	}
	unitViews := make([]UnitView, 0, len(units))
	for _, u := range units {
		uv := UnitView{Unit: *u}
		if um, ok := umByUnit[u.ID]; ok {
			uv.UnitMeter = um
			if m, mErr := o.st.Repos().Meters().Get(ctx, um.MeterID); mErr == nil {
				uv.MaskedMeterNumber = m.Masked
				uv.Disco = m.DiscoCode
			}
		}
		unitViews = append(unitViews, uv)
	}
	return &OrgDetailView{
		Organisation: *org,
		CallerRole:   role,
		Members:      members,
		Properties:   props,
		Units:        unitViews,
	}, nil
}

func (o *Orchestrator) UpdateOrg(ctx context.Context, orgID uuid.UUID, p UpdateOrgParams) (*OrgDetailView, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleOwner); err != nil {
		return nil, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	now := o.Now().UTC()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleOwner); err != nil {
			return err
		}
		org, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Name != nil {
			clean, err := powerdesk.CleanName(*p.Name, powerdesk.ErrInvalidOrgName)
			if err != nil {
				return err
			}
			org.Name = clean
		}
		if p.LegalName != nil {
			org.LegalName = strings.TrimSpace(*p.LegalName)
		}
		if p.RegistrationNumber != nil {
			org.RegistrationNumber = strings.TrimSpace(*p.RegistrationNumber)
		}
		if p.ClearManagerApprovalThreshold {
			org.ManagerApprovalThresholdMinor = nil
		} else if p.ManagerApprovalThresholdMinor != nil {
			if *p.ManagerApprovalThresholdMinor < 0 {
				return powerdesk.ErrInvalidThreshold
			}
			v := *p.ManagerApprovalThresholdMinor
			org.ManagerApprovalThresholdMinor = &v
		}
		org.UpdatedAt = now
		if err := r.PowerDesk().UpdateOrg(ctx, org); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("organisation.updated", resourceOrganisation, org.ID, nil))
	})
	if err != nil {
		return nil, err
	}
	return o.GetOrg(ctx, orgID)
}

func (o *Orchestrator) AddOrgMember(ctx context.Context, orgID uuid.UUID, targetUserID string, role powerdesk.Role) (*powerdesk.Member, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleOwner); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, powerdesk.ErrInvalidRole
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	now := o.Now().UTC()
	var m *powerdesk.Member
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleOwner); err != nil {
			return err
		}
		if _, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return authz.ErrNotFound
			}
			return err
		}
		// Prevent demoting the last active OWNER via upsert.
		if existing, err := r.PowerDesk().GetMemberForUpdate(ctx, orgID, targetUserID); err == nil {
			if existing.Role == powerdesk.RoleOwner && role != powerdesk.RoleOwner {
				owners, cErr := r.PowerDesk().CountActiveOwners(ctx, orgID)
				if cErr != nil {
					return cErr
				}
				if owners <= 1 {
					return powerdesk.ErrLastOwner
				}
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		m = &powerdesk.Member{
			OrganisationID: orgID,
			UserID:         strings.TrimSpace(targetUserID),
			Role:           role,
			CreatedAt:      now,
		}
		if err := r.PowerDesk().UpsertMember(ctx, m); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("organisation.member_upserted", resourceOrganisation, orgID, map[string]any{
			"member_user_id": m.UserID,
			"role":           m.Role,
		}))
	})
	return m, err
}

func (o *Orchestrator) UpdateOrgMemberRole(ctx context.Context, orgID uuid.UUID, targetUserID string, role powerdesk.Role) (*powerdesk.Member, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleOwner); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, powerdesk.ErrInvalidRole
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	var updated *powerdesk.Member
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleOwner); err != nil {
			return err
		}
		if _, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return authz.ErrNotFound
			}
			return err
		}
		existing, err := r.PowerDesk().GetMemberForUpdate(ctx, orgID, targetUserID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		if existing.Role == powerdesk.RoleOwner && role != powerdesk.RoleOwner {
			owners, err := r.PowerDesk().CountActiveOwners(ctx, orgID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return powerdesk.ErrLastOwner
			}
		}
		existing.Role = role
		if err := r.PowerDesk().UpsertMember(ctx, existing); err != nil {
			return err
		}
		updated = existing
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("organisation.member_role_updated", resourceOrganisation, orgID, map[string]any{
			"member_user_id": targetUserID,
			"role":           role,
		}))
	})
	return updated, err
}

func (o *Orchestrator) RemoveOrgMember(ctx context.Context, orgID uuid.UUID, targetUserID string) error {
	if err := authz.Require(ctx, orgID, powerdesk.RoleOwner); err != nil {
		return err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return err
	}
	return o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleOwner); err != nil {
			return err
		}
		if _, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return authz.ErrNotFound
			}
			return err
		}
		existing, err := r.PowerDesk().GetMemberForUpdate(ctx, orgID, targetUserID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		if existing.Role == powerdesk.RoleOwner {
			owners, err := r.PowerDesk().CountActiveOwners(ctx, orgID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return powerdesk.ErrLastOwner
			}
		}
		if err := r.PowerDesk().RemoveMember(ctx, orgID, targetUserID); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("organisation.member_removed", resourceOrganisation, orgID, map[string]any{
			"member_user_id": targetUserID,
		}))
	})
}

// ---- Properties, Units & Unit-Meters ---------------------------------------

type CreatePropertyParams struct {
	Name    string
	Address string
	City    string
	State   string
	Country string
}

func (o *Orchestrator) CreateProperty(ctx context.Context, orgID uuid.UUID, p CreatePropertyParams) (*powerdesk.Property, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleManager); err != nil {
		return nil, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	name, err := powerdesk.CleanName(p.Name, powerdesk.ErrInvalidPropertyName)
	if err != nil {
		return nil, err
	}
	country := strings.ToUpper(strings.TrimSpace(p.Country))
	if country == "" {
		country = "NG"
	}
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return nil, ErrInvalidCountryCode
	}
	now := o.Now().UTC()
	prop := &powerdesk.Property{
		ID:             uuid.New(),
		OrganisationID: orgID,
		OwnerUserID:    callerUID,
		Name:           name,
		Address:        strings.TrimSpace(p.Address),
		City:           strings.TrimSpace(p.City),
		State:          strings.TrimSpace(p.State),
		Country:        country,
		CreatedAt:      now,
	}
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleManager); err != nil {
			return err
		}
		if err := r.PowerDesk().CreateProperty(ctx, prop); err != nil {
			return err
		}
		if _, err := r.PowerDesk().EnsureDefaultBuilding(ctx, prop.ID, powerdesk.DefaultBuildingName, now); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("property.created", resourceProperty, prop.ID, map[string]any{
			"organisation_id": orgID,
		}))
	})
	return prop, err
}

type CreateUnitParams struct {
	BuildingName    string
	UnitName        string
	OccupancyStatus string
}

func (o *Orchestrator) CreateUnit(ctx context.Context, propertyID uuid.UUID, p CreateUnitParams) (*powerdesk.Unit, error) {
	prop, err := o.st.Repos().PowerDesk().GetProperty(ctx, propertyID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, prop.OrganisationID, powerdesk.RoleManager); err != nil {
		return nil, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	unitName, err := powerdesk.CleanName(p.UnitName, powerdesk.ErrInvalidUnitName)
	if err != nil {
		return nil, err
	}
	now := o.Now().UTC()
	var unit *powerdesk.Unit
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, prop.OrganisationID, powerdesk.RoleManager); err != nil {
			return err
		}
		bld, err := r.PowerDesk().EnsureDefaultBuilding(ctx, prop.ID, p.BuildingName, now)
		if err != nil {
			return err
		}
		unit = &powerdesk.Unit{
			ID:              uuid.New(),
			BuildingID:      bld.ID,
			UnitName:        unitName,
			OccupancyStatus: strings.TrimSpace(p.OccupancyStatus),
			CreatedAt:       now,
		}
		if err := r.PowerDesk().CreateUnit(ctx, unit); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("unit.created", resourceUnit, unit.ID, map[string]any{
			"organisation_id": prop.OrganisationID,
			"property_id":     prop.ID,
		}))
	})
	return unit, err
}

type AttachUnitMeterParams struct {
	MeterID           uuid.UUID
	MeterNumber       string
	DiscoCode         string
	Label             string
	EvidenceReference string
}

type UnitMeterView struct {
	powerdesk.UnitMeter
	MaskedMeterNumber string `json:"masked_meter_number"`
	Disco             string `json:"disco"`
}

func (o *Orchestrator) AttachUnitMeter(ctx context.Context, unitID uuid.UUID, p AttachUnitMeterParams) (*UnitMeterView, error) {
	unit, err := o.st.Repos().PowerDesk().GetUnit(ctx, unitID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, unit.OrganisationID, powerdesk.RoleManager); err != nil {
		return nil, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	evRef, err := powerdesk.ValidateEvidenceRef(p.EvidenceReference)
	if err != nil {
		return nil, err
	}

	var m *meters.Meter
	switch {
	case strings.TrimSpace(p.MeterNumber) != "" && strings.TrimSpace(p.DiscoCode) != "":
		lbl := strings.TrimSpace(p.Label)
		if lbl == "" {
			lbl = unit.UnitName
		}
		m, err = o.Meters.Register(ctx, callerUID, strings.TrimSpace(p.MeterNumber), strings.TrimSpace(p.DiscoCode), lbl)
		if err != nil {
			return nil, err
		}
	case p.MeterID != uuid.Nil:
		m, err = o.Meters.Authorize(ctx, callerUID, p.MeterID, meters.PermPurchase)
		if err != nil {
			return nil, err
		}
	default:
		return nil, meters.ErrInvalidMeter
	}

	now := o.Now().UTC()
	var um *powerdesk.UnitMeter
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, unit.OrganisationID, powerdesk.RoleManager); err != nil {
			return err
		}
		if _, err := registryAuthorizePurchaseIn(ctx, r, callerUID, m.ID); err != nil {
			return err
		}
		um = &powerdesk.UnitMeter{
			ID:                uuid.New(),
			OrganisationID:    unit.OrganisationID,
			PropertyID:        unit.PropertyID,
			UnitID:            unit.ID,
			MeterID:           m.ID,
			Status:            powerdesk.UnitMeterActive,
			Verified:          false,
			EvidenceReference: evRef,
			DiscoAccountName:  m.CustomerName,
			CreatedByUserID:   callerUID,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := r.PowerDesk().AttachUnitMeter(ctx, um); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("unit_meter.attached", resourceUnitMeter, um.ID, map[string]any{
			"organisation_id": unit.OrganisationID,
			"unit_id":         unit.ID,
			"meter_id":        m.ID,
			"verified":        false,
		}))
	})
	if err != nil {
		return nil, err
	}
	return &UnitMeterView{
		UnitMeter:         *um,
		MaskedMeterNumber: m.Masked,
		Disco:             m.DiscoCode,
	}, nil
}

func registryAuthorizePurchaseIn(ctx context.Context, r store.Repos, userID string, meterID uuid.UUID) (*meters.Meter, error) {
	m, err := r.Meters().GetForUpdate(ctx, meterID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, meters.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	gr, err := r.Meters().ActiveGrant(ctx, meterID, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !gr.Permission.Allows(meters.PermPurchase)) {
		return nil, meters.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

type VerifyUnitMeterParams struct {
	Method            powerdesk.VerificationMethod
	EvidenceReference string
}

func (o *Orchestrator) VerifyUnitMeter(ctx context.Context, unitMeterID uuid.UUID, p VerifyUnitMeterParams, actor audit.Actor) (*UnitMeterView, error) {
	if !p.Method.ValidForApproval() {
		return nil, powerdesk.ErrVerificationMethodInvalid
	}
	cleanEvidence, err := powerdesk.ValidateEvidenceRef(p.EvidenceReference)
	if err != nil {
		return nil, err
	}

	now := o.Now().UTC()
	var (
		um *powerdesk.UnitMeter
		m  *meters.Meter
	)
	err = o.st.InTx(ctx, func(r store.Repos) error {
		var err error
		um, err = r.PowerDesk().GetUnitMeterForUpdate(ctx, unitMeterID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnitMeterNotFound
		}
		if err != nil {
			return err
		}
		org, err := r.PowerDesk().GetOrgForUpdate(ctx, um.OrganisationID)
		if err != nil {
			return err
		}
		m, err = r.Meters().Get(ctx, um.MeterID)
		if err != nil {
			return err
		}

		switch p.Method {
		case powerdesk.VerifyAccountNameMatch:
			candidate := m.CustomerName
			if strings.TrimSpace(candidate) == "" {
				candidate = um.DiscoAccountName
			}
			if !powerdesk.AccountNamesMatch(candidate, org) {
				return powerdesk.ErrAccountNameMismatch
			}
			if org.VerificationStatus != powerdesk.VerificationVerified {
				org.VerificationStatus = powerdesk.VerificationVerified
				org.UpdatedAt = now
				if err := r.PowerDesk().UpdateOrg(ctx, org); err != nil {
					return err
				}
			}
		case powerdesk.VerifyEvidenceApproved:
			evRef := cleanEvidence
			if evRef == "" {
				evRef = um.EvidenceReference
			}
			if evRef == "" {
				return powerdesk.ErrEvidenceRequired
			}
			um.EvidenceReference = evRef
		}

		um.Verified = true
		um.VerificationMethod = p.Method
		um.VerifiedByUserID = actor.UserID
		um.VerifiedAt = &now
		um.VerificationReason = actor.Reason
		um.UpdatedAt = now
		if err := r.PowerDesk().UpdateUnitMeter(ctx, um); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event("unit_meter.verified", resourceUnitMeter, um.ID, map[string]any{
			"organisation_id":    um.OrganisationID,
			"unit_id":            um.UnitID,
			"meter_id":           um.MeterID,
			"method":             um.VerificationMethod,
			"evidence_reference": um.EvidenceReference,
		}))
	})
	if err != nil {
		return nil, err
	}
	return &UnitMeterView{
		UnitMeter:         *um,
		MaskedMeterNumber: m.Masked,
		Disco:             m.DiscoCode,
	}, nil
}

type UnitMeterHistoryItem struct {
	TransactionID  uuid.UUID         `json:"transaction_id"`
	AmountMinor    int64             `json:"amount_minor"`
	FeeMinor       int64             `json:"fee_minor"`
	TotalMinor     int64             `json:"total_minor"`
	Currency       money.Currency    `json:"currency"`
	Status         tx.Overall        `json:"status"`
	DeliveryMethod tx.DeliveryMethod `json:"delivery_method"`
	CreatedAt      time.Time         `json:"created_at"`
}

func (o *Orchestrator) GetUnitMeterHistory(ctx context.Context, unitID, unitMeterID uuid.UUID) ([]UnitMeterHistoryItem, error) {
	unit, err := o.st.Repos().PowerDesk().GetUnit(ctx, unitID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := authz.Require(ctx, unit.OrganisationID, powerdesk.RoleViewer); err != nil {
		return nil, err
	}
	um, err := o.st.Repos().PowerDesk().GetUnitMeter(ctx, unitMeterID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if um.UnitID != unit.ID || um.OrganisationID != unit.OrganisationID {
		return nil, authz.ErrNotFound
	}
	if !um.Verified {
		return nil, powerdesk.ErrUnitMeterUnverified
	}
	txns, err := o.st.Repos().Transactions().Search(ctx, store.TxnFilter{
		MeterID: um.MeterID,
		Limit:   100,
	})
	if err != nil {
		return nil, err
	}
	out := make([]UnitMeterHistoryItem, 0, len(txns))
	for _, t := range txns {
		out = append(out, UnitMeterHistoryItem{
			TransactionID:  t.ID,
			AmountMinor:    t.Amount.Minor,
			FeeMinor:       t.Fee.Minor,
			TotalMinor:     t.Total.Minor,
			Currency:       t.Total.Currency,
			Status:         t.Status(),
			DeliveryMethod: t.DeliveryMethod,
			CreatedAt:      t.CreatedAt.UTC(),
		})
	}
	return out, nil
}

func (o *Orchestrator) DelegateUnitMeter(ctx context.Context, unitID, unitMeterID uuid.UUID, targetUserID string, perm meters.Permission, label string) error {
	unit, err := o.st.Repos().PowerDesk().GetUnit(ctx, unitID)
	if errors.Is(err, store.ErrNotFound) {
		return authz.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := authz.Require(ctx, unit.OrganisationID, powerdesk.RoleManager); err != nil {
		return err
	}
	if perm != meters.PermView && perm != meters.PermPurchase {
		return ErrInvalidDelegation
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return err
	}
	return o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, unit.OrganisationID, powerdesk.RoleManager); err != nil {
			return err
		}
		um, err := r.PowerDesk().GetUnitMeterForUpdate(ctx, unitMeterID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}
		if um.UnitID != unit.ID || um.OrganisationID != unit.OrganisationID || um.Status != powerdesk.UnitMeterActive {
			return authz.ErrNotFound
		}
		if !um.Verified {
			return powerdesk.ErrUnitMeterUnverified
		}
		if _, err := r.Meters().GetForUpdate(ctx, um.MeterID); err != nil {
			return err
		}
		lbl := strings.TrimSpace(label)
		if lbl == "" {
			lbl = unit.UnitName
		}
		if err := r.Meters().UpsertGrant(ctx, meters.Grant{
			MeterID:    um.MeterID,
			UserID:     strings.TrimSpace(targetUserID),
			Permission: perm,
			Label:      lbl,
			GrantedBy:  callerUID,
		}); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("unit_meter.delegated", resourceUnitMeter, um.ID, map[string]any{
			"organisation_id": um.OrganisationID,
			"unit_id":         um.UnitID,
			"meter_id":        um.MeterID,
			"target_user_id":  strings.TrimSpace(targetUserID),
			"permission":      perm,
		}))
	})
}

// ---- Bulk Top-Ups (Funding Pot Kind = LANDLORD_BULK) -----------------------

type BulkTopupItemInput struct {
	UnitID      uuid.UUID `json:"unit_id"`
	AmountMinor int64     `json:"amount_minor"`
}

type CreateBulkTopupRequest struct {
	Items          []BulkTopupItemInput
	Email          string
	IdempotencyKey string
}

type BulkTopupItemStatus string

const (
	BulkItemPending        BulkTopupItemStatus = "PENDING"
	BulkItemDelivered      BulkTopupItemStatus = "DELIVERED"
	BulkItemFailedRefunded BulkTopupItemStatus = "FAILED_REFUNDED"
)

type BulkTopupItemView struct {
	ID                uuid.UUID           `json:"id"`
	PropertyID        uuid.UUID           `json:"property_id"`
	PropertyName      string              `json:"property_name"`
	UnitID            uuid.UUID           `json:"unit_id"`
	UnitName          string              `json:"unit_name"`
	MaskedMeterNumber string              `json:"masked_meter_number"`
	Disco             string              `json:"disco"`
	AmountMinor       int64               `json:"amount_minor"`
	FeeMinor          int64               `json:"fee_minor"`
	TotalMinor        int64               `json:"total_minor"`
	Currency          money.Currency      `json:"currency"`
	Status            BulkTopupItemStatus `json:"status"`
	TransactionID     *uuid.UUID          `json:"transaction_id,omitempty"`
	OverallStatus     tx.Overall          `json:"overall_status,omitempty"`
	DeliveryMethod    tx.DeliveryMethod   `json:"delivery_method,omitempty"`
	TokenAvailable    bool                `json:"token_available"`
	FailureReason     string              `json:"failure_reason,omitempty"`
}

type BulkTopupDetailView struct {
	PotID                 uuid.UUID                `json:"pot_id"`
	Kind                  pots.Kind                `json:"kind"`
	OrganisationID        uuid.UUID                `json:"organisation_id"`
	PotStatus             pots.Status              `json:"pot_status"`
	ApprovalStatus        powerdesk.ApprovalStatus `json:"approval_status"`
	CreatedByUserID       string                   `json:"created_by_user_id"`
	CreatorRole           powerdesk.Role           `json:"creator_role"`
	ApprovedByUserID      string                   `json:"approved_by_user_id,omitempty"`
	ApprovedAt            *time.Time               `json:"approved_at,omitempty"`
	ElectricityTotalMinor int64                    `json:"electricity_total_minor"`
	FeeTotalMinor         int64                    `json:"fee_total_minor"`
	TotalMinor            int64                    `json:"total_minor"`
	FundedMinor           int64                    `json:"funded_minor"`
	RefundedMinor         int64                    `json:"refunded_minor"`
	Currency              money.Currency           `json:"currency"`
	CheckoutURL           string                   `json:"checkout_url,omitempty"`
	PaymentReference      string                   `json:"payment_reference,omitempty"`
	DeliveredCount        int                      `json:"delivered_count"`
	FailedRefundedCount   int                      `json:"failed_refunded_count"`
	PendingCount          int                      `json:"pending_count"`
	Summary               string                   `json:"summary"`
	Items                 []BulkTopupItemView      `json:"items"`
	CreatedAt             time.Time                `json:"created_at"`
	UpdatedAt             time.Time                `json:"updated_at"`
}

func (o *Orchestrator) resolveUserChargeEmail(ctx context.Context, userID, explicitEmail string) (string, error) {
	raw := strings.TrimSpace(explicitEmail)
	if raw == "" {
		if c, err := o.st.Repos().Users().Contacts(ctx, userID); err == nil && len(c.EmailCiphertext) > 0 {
			if dec, dErr := notifications.OpenUserEmail(o.box, userID, c.EmailCiphertext); dErr == nil {
				raw = dec
			}
		}
	}
	if raw == "" {
		raw = "billing+" + userID + "@meterly.local"
	}
	return notifications.NormalizeEmail(raw)
}

func (o *Orchestrator) CreateBulkTopup(ctx context.Context, orgID uuid.UUID, req CreateBulkTopupRequest) (*BulkTopupDetailView, bool, error) {
	callerRole, err := authz.RequireRole(ctx, orgID, powerdesk.RoleManager)
	if err != nil {
		return nil, false, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(req.Items) == 0 || len(req.Items) > powerdesk.MaxBulkItems {
		return nil, false, powerdesk.ErrEmptyBulkItems
	}

	seenUnits := make(map[uuid.UUID]bool, len(req.Items))
	var hashParts []string
	var electricityTotal int64
	for _, in := range req.Items {
		if in.UnitID == uuid.Nil {
			return nil, false, powerdesk.ErrEmptyBulkItems
		}
		if seenUnits[in.UnitID] {
			return nil, false, powerdesk.ErrDuplicateUnitInBulk
		}
		seenUnits[in.UnitID] = true
		if in.AmountMinor < o.cfg.MinAmountMinor || in.AmountMinor > o.cfg.MaxAmountMinor {
			return nil, false, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
		}
		electricityTotal += in.AmountMinor
		hashParts = append(hashParts, fmt.Sprintf("%s:%d", in.UnitID, in.AmountMinor))
	}
	sort.Strings(hashParts)
	normEmail, err := o.resolveUserChargeEmail(ctx, callerUID, req.Email)
	if err != nil {
		return nil, false, err
	}

	code, err := pots.GenerateCode()
	if err != nil {
		return nil, false, err
	}
	codeHMAC, ok := pots.BulkCodeHMAC(o.box, code)
	if !ok {
		return nil, false, errors.New("failed to compute bulk pot HMAC")
	}

	idemScope := "org-bulk:" + orgID.String()
	reqHash := o.box.Hash("org-bulk:" + orgID.String() + ":" + strings.Join(hashParts, ","))
	now := o.Now().UTC()

	var (
		potID     uuid.UUID
		contribID *uuid.UUID
		replayed  bool
	)

	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		var rErr error
		callerRole, rErr = authz.RequireRole(txCtx, orgID, powerdesk.RoleManager)
		if rErr != nil {
			return rErr
		}
		if req.IdempotencyKey != "" {
			existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
				Scope:       idemScope,
				Operation:   opCreateBulkTopup,
				Key:         req.IdempotencyKey,
				RequestHash: reqHash,
				CreatedAt:   now,
				ExpiresAt:   now.Add(o.cfg.IdempotencyTTL),
			})
			if err != nil {
				return err
			}
			if existing != nil {
				if existing.RequestHash != reqHash {
					return idempotency.ErrConflict
				}
				if existing.ResultRef == "" {
					return idempotency.ErrInProgress
				}
				parsedID, err := uuid.Parse(existing.ResultRef)
				if err != nil {
					return err
				}
				bt, err := r.PowerDesk().GetBulkTopup(ctx, parsedID)
				if err != nil {
					return err
				}
				potID = bt.PotID
				contribID = bt.ContributionID
				replayed = true
				return nil
			}
		}

		org, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}

		feeTotal := int64(len(req.Items)) * o.cfg.FeeMinor
		totalMinor := electricityTotal + feeTotal
		needsApproval := org.NeedsManagerApproval(callerRole, totalMinor)

		potStatus := pots.StatusOpen
		approvalStatus := powerdesk.ApprovalNotRequired
		if needsApproval {
			potStatus = pots.StatusPendingApproval
			approvalStatus = powerdesk.ApprovalPending
		}

		pot := &pots.Pot{
			ID:          uuid.New(),
			Kind:        pots.KindLandlordBulk,
			OwnerID:     callerUID,
			TargetMinor: totalMinor,
			FeeMinor:    feeTotal,
			Currency:    money.NGN,
			Status:      potStatus,
			CodeHMAC:    codeHMAC,
			Deadline:    now.Add(pots.DefaultMaxHold),
			NextCheckAt: now.Add(o.cfg.PaymentExpiry),
			CreatedAt:   now,
			UpdatedAt:   now,
		}

		items := make([]*powerdesk.BulkTopupItem, 0, len(req.Items))
		for _, in := range req.Items {
			u, err := r.PowerDesk().GetUnit(ctx, in.UnitID)
			if errors.Is(err, store.ErrNotFound) {
				return authz.ErrNotFound
			}
			if err != nil {
				return err
			}
			// Zero-IDOR: any unit belonging to a different organisation returns 404.
			if u.OrganisationID != orgID {
				return authz.ErrNotFound
			}
			um, err := r.PowerDesk().GetActiveUnitMeterByUnit(ctx, u.ID)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: %s", ErrUnitHasNoMeter, u.ID)
			}
			if err != nil {
				return err
			}
			items = append(items, &powerdesk.BulkTopupItem{
				ID:             uuid.New(),
				PotID:          pot.ID,
				OrganisationID: orgID,
				PropertyID:     u.PropertyID,
				UnitID:         u.ID,
				UnitMeterID:    um.ID,
				MeterID:        um.MeterID,
				AmountMinor:    in.AmountMinor,
				FeeMinor:       o.cfg.FeeMinor,
				TotalMinor:     in.AmountMinor + o.cfg.FeeMinor,
				Currency:       money.NGN,
				CreatedAt:      now,
				UpdatedAt:      now,
			})
		}

		if err := r.Pots().Create(ctx, pot); err != nil {
			return err
		}

		if !needsApproval {
			c, err := o.createBulkContributionInTx(ctx, r, pot, callerUID, org.Name, normEmail, now)
			if err != nil {
				return err
			}
			contribID = &c.ID
		}

		bt := &powerdesk.BulkTopup{
			PotID:                 pot.ID,
			OrganisationID:        orgID,
			CreatedByUserID:       callerUID,
			CreatorRole:           callerRole,
			ApprovalStatus:        approvalStatus,
			ContributionID:        contribID,
			ElectricityTotalMinor: electricityTotal,
			FeeTotalMinor:         feeTotal,
			TotalMinor:            totalMinor,
			Currency:              money.NGN,
			IdempotencyKey:        req.IdempotencyKey,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
		if err := r.PowerDesk().CreateBulkTopup(ctx, bt, items); err != nil {
			return err
		}
		if req.IdempotencyKey != "" {
			if err := r.Idempotency().Complete(ctx, idemScope, opCreateBulkTopup, req.IdempotencyKey, pot.ID.String()); err != nil {
				return err
			}
		}
		potID = pot.ID
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("bulk_topup.created", resourceBulkTopup, pot.ID, map[string]any{
			"organisation_id": orgID,
			"item_count":      len(items),
			"total_minor":     totalMinor,
			"approval_status": approvalStatus,
		}))
	})
	if err != nil {
		return nil, false, err
	}

	if contribID != nil {
		if _, err := o.EnsureContributionPaymentInitialised(ctx, *contribID, normEmail); err != nil {
			return nil, replayed, err
		}
	}
	view, err := o.GetBulkTopup(ctx, orgID, potID)
	return view, replayed, err
}

func (o *Orchestrator) createBulkContributionInTx(
	ctx context.Context,
	r store.Repos,
	pot *pots.Pot,
	payerUserID, displayName, normEmail string,
	now time.Time,
) (*pots.Contribution, error) {
	cid := uuid.New()
	emailCT, emailKV, err := pots.SealContributorEmail(o.box, cid, normEmail)
	if err != nil {
		return nil, err
	}
	nameCT, nameKV, err := pots.SealContributorName(o.box, cid, displayName)
	if err != nil {
		return nil, err
	}
	c := &pots.Contribution{
		ID:               cid,
		PotID:            pot.ID,
		PayerUserID:      payerUserID,
		EmailCiphertext:  emailCT,
		EmailKeyVersion:  emailKV,
		NameCiphertext:   nameCT,
		NameKeyVersion:   nameKV,
		Initials:         pots.FormatInitials(displayName),
		AmountMinor:      pot.TargetMinor,
		Currency:         pot.Currency,
		Status:           pots.ContribPending,
		RefundClaimUntil: time.Unix(0, 0).UTC(),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := r.Pots().CreateContribution(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (o *Orchestrator) ApproveBulkTopup(ctx context.Context, orgID, potID uuid.UUID, explicitEmail string) (*BulkTopupDetailView, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleOwner); err != nil {
		return nil, err
	}
	callerUID, err := mustCallerUID(ctx)
	if err != nil {
		return nil, err
	}
	normEmail, err := o.resolveUserChargeEmail(ctx, callerUID, explicitEmail)
	if err != nil {
		return nil, err
	}

	now := o.Now().UTC()
	var contribID uuid.UUID
	err = o.st.InTx(ctx, func(r store.Repos) error {
		txCtx := store.WithRepos(ctx, r)
		if err := authz.Require(txCtx, orgID, powerdesk.RoleOwner); err != nil {
			return err
		}
		org, err := r.PowerDesk().GetOrgForUpdate(ctx, orgID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}
		pot, err := r.Pots().GetForUpdate(ctx, potID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}
		bt, err := r.PowerDesk().GetBulkTopupForUpdate(ctx, potID)
		if errors.Is(err, store.ErrNotFound) {
			return authz.ErrNotFound
		}
		if err != nil {
			return err
		}
		if bt.OrganisationID != orgID || pot.Kind != pots.KindLandlordBulk {
			return authz.ErrNotFound
		}
		if bt.ApprovalStatus == powerdesk.ApprovalApproved && bt.ContributionID != nil {
			contribID = *bt.ContributionID
			return nil
		}
		if bt.ApprovalStatus != powerdesk.ApprovalPending || pot.Status != pots.StatusPendingApproval {
			return powerdesk.ErrNotPendingApproval
		}
		if err := pot.Transition(pots.StatusOpen, now); err != nil {
			return err
		}
		pot.Deadline = now.Add(pots.DefaultMaxHold)
		pot.NextCheckAt = now.Add(o.cfg.PaymentExpiry)
		if err := r.Pots().Update(ctx, pot); err != nil {
			return err
		}

		c, err := o.createBulkContributionInTx(ctx, r, pot, callerUID, org.Name, normEmail, now)
		if err != nil {
			return err
		}
		contribID = c.ID
		bt.ApprovalStatus = powerdesk.ApprovalApproved
		bt.ApprovedByUserID = callerUID
		bt.ApprovedAt = &now
		bt.ContributionID = &c.ID
		bt.UpdatedAt = now
		if err := r.PowerDesk().UpdateBulkTopup(ctx, bt); err != nil {
			return err
		}
		actor := audit.Actor{UserID: callerUID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("bulk_topup.approved", resourceBulkTopup, pot.ID, map[string]any{
			"organisation_id": orgID,
			"total_minor":     bt.TotalMinor,
		}))
	})
	if err != nil {
		return nil, err
	}
	if contribID != uuid.Nil {
		if _, err := o.EnsureContributionPaymentInitialised(ctx, contribID, normEmail); err != nil {
			return nil, err
		}
	}
	return o.GetBulkTopup(ctx, orgID, potID)
}

func (o *Orchestrator) GetBulkTopup(ctx context.Context, orgID, potID uuid.UUID) (*BulkTopupDetailView, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleViewer); err != nil {
		return nil, err
	}
	bt, err := o.st.Repos().PowerDesk().GetBulkTopup(ctx, potID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if bt.OrganisationID != orgID {
		return nil, authz.ErrNotFound
	}
	pot, err := o.st.Repos().Pots().Get(ctx, potID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, potID)
	if err != nil {
		return nil, err
	}
	sum := pots.SummarizeContributions(contribs)
	var checkoutURL, payRef string
	if bt.ContributionID != nil {
		for _, c := range contribs {
			if c.ID == *bt.ContributionID {
				checkoutURL = c.CheckoutURL
				payRef = c.PaymentProviderRef
				break
			}
		}
	}

	items, err := o.st.Repos().PowerDesk().ListBulkTopupItems(ctx, potID)
	if err != nil {
		return nil, err
	}
	allocs, err := o.st.Repos().Pots().ListAllocations(ctx, potID)
	if err != nil {
		return nil, err
	}
	allocByID := make(map[uuid.UUID]*pots.Allocation, len(allocs))
	for _, al := range allocs {
		allocByID[al.ID] = al
	}

	props, err := o.st.Repos().PowerDesk().ListPropertiesForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	propName := make(map[uuid.UUID]string, len(props))
	for _, p := range props {
		propName[p.ID] = p.Name
	}
	units, err := o.st.Repos().PowerDesk().ListUnitsForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	unitName := make(map[uuid.UUID]string, len(units))
	for _, u := range units {
		unitName[u.ID] = u.UnitName
	}

	var delivered, failedRefunded, pending int
	itemViews := make([]BulkTopupItemView, 0, len(items))
	for _, it := range items {
		iv := BulkTopupItemView{
			ID:            it.ID,
			PropertyID:    it.PropertyID,
			PropertyName:  propName[it.PropertyID],
			UnitID:        it.UnitID,
			UnitName:      unitName[it.UnitID],
			AmountMinor:   it.AmountMinor,
			FeeMinor:      it.FeeMinor,
			TotalMinor:    it.TotalMinor,
			Currency:      it.Currency,
			Status:        BulkItemPending,
			TransactionID: it.TransactionID,
			FailureReason: it.FailureReason,
		}
		if m, mErr := o.st.Repos().Meters().Get(ctx, it.MeterID); mErr == nil {
			iv.MaskedMeterNumber = m.Masked
			iv.Disco = m.DiscoCode
		}

		switch {
		case it.FailureReason != "" && it.TransactionID == nil:
			iv.Status = BulkItemFailedRefunded
			failedRefunded++
		case it.AllocationID != nil && allocByID[*it.AllocationID] != nil && allocByID[*it.AllocationID].Status == pots.AllocReturned:
			iv.Status = BulkItemFailedRefunded
			failedRefunded++
			if it.TransactionID != nil {
				if t, tErr := o.Get(ctx, *it.TransactionID); tErr == nil {
					iv.OverallStatus = t.Status()
					iv.DeliveryMethod = t.DeliveryMethod
					iv.FailureReason = t.RefundReason
				}
			}
		case it.TransactionID != nil:
			if t, tErr := o.Get(ctx, *it.TransactionID); tErr == nil {
				iv.OverallStatus = t.Status()
				iv.DeliveryMethod = t.DeliveryMethod
				iv.TokenAvailable = len(t.TokenCiphertext) > 0
				if t.Delivery == tx.DelConfirmed {
					iv.Status = BulkItemDelivered
					delivered++
				} else if t.Payment == tx.PayRefunded {
					iv.Status = BulkItemFailedRefunded
					failedRefunded++
				} else {
					iv.Status = BulkItemPending
					pending++
				}
			} else {
				pending++
			}
		case pot.Status == pots.StatusCancelled || pot.Status == pots.StatusExpired || pot.Status == pots.StatusRefunded:
			iv.Status = BulkItemFailedRefunded
			failedRefunded++
		default:
			iv.Status = BulkItemPending
			pending++
		}
		itemViews = append(itemViews, iv)
	}

	return &BulkTopupDetailView{
		PotID:                 bt.PotID,
		Kind:                  pot.Kind,
		OrganisationID:        bt.OrganisationID,
		PotStatus:             pot.Status,
		ApprovalStatus:        bt.ApprovalStatus,
		CreatedByUserID:       bt.CreatedByUserID,
		CreatorRole:           bt.CreatorRole,
		ApprovedByUserID:      bt.ApprovedByUserID,
		ApprovedAt:            bt.ApprovedAt,
		ElectricityTotalMinor: bt.ElectricityTotalMinor,
		FeeTotalMinor:         bt.FeeTotalMinor,
		TotalMinor:            bt.TotalMinor,
		FundedMinor:           sum.FundedMinor,
		RefundedMinor:         sum.RefundedMinor,
		Currency:              bt.Currency,
		CheckoutURL:           checkoutURL,
		PaymentReference:      payRef,
		DeliveredCount:        delivered,
		FailedRefundedCount:   failedRefunded,
		PendingCount:          pending,
		Summary:               powerdesk.FormatBulkSummary(delivered, failedRefunded, pending),
		Items:                 itemViews,
		CreatedAt:             bt.CreatedAt,
		UpdatedAt:             pot.UpdatedAt,
	}, nil
}

// allocateAndVendLandlordBulkPot atomically creates 1 POT-funded transaction and
// allocation per active unit in a LANDLORD_BULK pot, queues immediate partial
// refunds for any unit whose meter was detached prior to allocation, and vends
// each allocated unit independently outside InTx.
func (o *Orchestrator) allocateAndVendLandlordBulkPot(ctx context.Context, potID uuid.UUID, now time.Time) error {
	var allocatedTxnIDs []uuid.UUID
	err := o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Pots().GetForUpdate(ctx, potID)
		if err != nil || cur.Status != pots.StatusFunded || cur.ReviewReason != "" {
			return err
		}
		bt, err := r.PowerDesk().GetBulkTopupForUpdate(ctx, potID)
		if err != nil {
			return err
		}
		cs, err := r.Pots().ListContributionsForUpdate(ctx, cur.ID)
		if err != nil {
			return err
		}
		items, err := r.PowerDesk().ListBulkTopupItems(ctx, potID)
		if err != nil {
			return err
		}

		// Lock distinct meters in canonical ascending UUID order to avoid deadlocks.
		meterIDStrs := make([]string, 0, len(items))
		seenMeters := make(map[uuid.UUID]bool, len(items))
		for _, it := range items {
			if !seenMeters[it.MeterID] {
				seenMeters[it.MeterID] = true
				meterIDStrs = append(meterIDStrs, it.MeterID.String())
			}
		}
		sort.Strings(meterIDStrs)
		lockedMeters := make(map[uuid.UUID]*meters.Meter, len(meterIDStrs))
		for _, s := range meterIDStrs {
			mid, _ := uuid.Parse(s)
			if m, mErr := r.Meters().GetForUpdate(ctx, mid); mErr == nil {
				lockedMeters[mid] = m
			}
		}

		remoteEnabled := false
		if o.remote != nil {
			if ok, eErr := r.Providers().Enabled(ctx, o.remote.Name()); eErr == nil && ok {
				remoteEnabled = true
			}
		}

		var hasUnallocRefund bool
		for _, it := range items {
			um, umErr := r.PowerDesk().GetUnitMeterForUpdate(ctx, it.UnitMeterID)
			m := lockedMeters[it.MeterID]
			if umErr != nil || um.Status != powerdesk.UnitMeterActive || um.MeterID != it.MeterID || m == nil {
				it.FailureReason = "unit meter detached before allocation"
				it.UpdatedAt = now
				if err := r.PowerDesk().UpdateBulkTopupItem(ctx, it); err != nil {
					return err
				}
				hp, err := o.queuePotPartialRefundInTx(
					ctx, r, cs, it.TotalMinor,
					"pot-unalloc-refund-due:"+it.ID.String(),
					it.ID,
					"Bulk top-up unallocated unit refund owed",
					now,
				)
				if err != nil {
					return err
				}
				if hp {
					hasUnallocRefund = true
				}
				continue
			}

			options := []tx.DeliveryMethod{tx.MethodToken}
			method := tx.MethodToken
			if o.remote != nil && m.Capabilities.RemoteCreditUsable() {
				options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
				if remoteEnabled {
					method = tx.MethodRemote
				}
			}

			elecAmount := money.New(it.AmountMinor, it.Currency)
			feeAmount := money.New(it.FeeMinor, it.Currency)
			totalAmount := money.New(it.TotalMinor, it.Currency)
			q := &tx.Quote{
				ID:              uuid.New(),
				CustomerID:      bt.CreatedByUserID,
				MeterID:         it.MeterID,
				Amount:          elecAmount,
				Fee:             feeAmount,
				Total:           totalAmount,
				DeliveryMethod:  method,
				DeliveryOptions: options,
				ExpiresAt:       now.Add(o.cfg.QuoteTTL),
				Used:            true,
				CreatedAt:       now,
			}
			if err := r.Quotes().Create(ctx, q); err != nil {
				return err
			}
			potTxn := tx.NewPotFunded(bt.CreatedByUserID, q, now)
			if err := r.Transactions().Create(ctx, potTxn); err != nil {
				return err
			}
			alloc := &pots.Allocation{
				ID:            uuid.New(),
				PotID:         cur.ID,
				TransactionID: potTxn.ID,
				MeterID:       it.MeterID,
				AmountMinor:   it.TotalMinor,
				Currency:      it.Currency,
				Status:        pots.AllocActive,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := r.Pots().CreateAllocation(ctx, alloc); err != nil {
				return err
			}
			it.TransactionID = &potTxn.ID
			it.AllocationID = &alloc.ID
			it.UpdatedAt = now
			if err := r.PowerDesk().UpdateBulkTopupItem(ctx, it); err != nil {
				return err
			}
			if err := o.post(ctx, r, "pot-alloc:"+alloc.ID.String(), potTxn.ID, "Allocated bulk pot funds to unit electricity purchase",
				ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: totalAmount},
				ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: totalAmount},
			); err != nil {
				return err
			}
			if err := emit(ctx, r, "electricity.transaction.created", potTxn.ID, map[string]any{
				"funding_source":  string(tx.FundingPot),
				"pot_id":          cur.ID,
				"organisation_id": bt.OrganisationID,
				"unit_id":         it.UnitID,
			}); err != nil {
				return err
			}
			if err := emit(ctx, r, "payment.confirmed", potTxn.ID, map[string]any{
				"funding_source":  string(tx.FundingPot),
				"pot_id":          cur.ID,
				"organisation_id": bt.OrganisationID,
				"unit_id":         it.UnitID,
			}); err != nil {
				return err
			}
			allocatedTxnIDs = append(allocatedTxnIDs, potTxn.ID)
		}

		if len(allocatedTxnIDs) == 0 {
			if err := cur.Transition(pots.StatusRefunding, now); err != nil {
				return err
			}
			cur.NextCheckAt = now
			cur.CheckAttempts = 0
			if !hasUnallocRefund {
				if err := cur.Transition(pots.StatusRefunded, now); err != nil {
					return err
				}
			}
			return r.Pots().Update(ctx, cur)
		}

		if err := cur.Transition(pots.StatusAllocating, now); err != nil {
			return err
		}
		cur.NextCheckAt = now
		cur.CheckAttempts = 0
		return r.Pots().Update(ctx, cur)
	})
	if err != nil {
		return err
	}

	var firstAdvErr error
	for _, txnID := range allocatedTxnIDs {
		if _, advErr := o.advance(ctx, txnID); advErr != nil && firstAdvErr == nil {
			firstAdvErr = advErr
		}
	}
	return firstAdvErr
}

func (o *Orchestrator) queuePotPartialRefundInTx(
	ctx context.Context,
	r store.Repos,
	contribs []*pots.Contribution,
	amountMinor int64,
	refPrefix string,
	sourceID uuid.UUID,
	desc string,
	now time.Time,
) (bool, error) {
	rem := amountMinor
	hasPendingWork := false
	for _, c := range contribs {
		if rem > 0 && c.RetainedMinor > 0 {
			toQueue := rem
			if toQueue > c.RetainedMinor {
				toQueue = c.RetainedMinor
			}
			c.RetainedMinor -= toQueue
			c.RefundAmountMinor += toQueue
			rem -= toQueue
			if c.Status != pots.ContribRefundPending {
				c.RefundStep++
				if err := c.Transition(pots.ContribRefundPending, now); err != nil {
					return false, err
				}
			}
			if err := o.post(ctx, r, fmt.Sprintf("%s:%s", refPrefix, c.ID), sourceID, desc,
				ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: money.New(toQueue, c.Currency)},
				ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: money.New(toQueue, c.Currency)},
			); err != nil {
				return false, err
			}
			if err := r.Pots().UpdateContribution(ctx, c); err != nil {
				return false, err
			}
		}
		if c.Status == pots.ContribRefundPending && c.PendingRefundMinor() > 0 {
			hasPendingWork = true
		}
	}
	return hasPendingWork, nil
}

// ---- Ledger-Derived Monthly Statements -------------------------------------

func (o *Orchestrator) GetMonthlyStatement(ctx context.Context, orgID uuid.UUID, propertyID *uuid.UUID, month string) (*powerdesk.MonthlyStatement, error) {
	if err := authz.Require(ctx, orgID, powerdesk.RoleViewer); err != nil {
		return nil, err
	}
	startUTC, endUTC, canonicalMonth, err := powerdesk.ParseStatementMonth(month)
	if err != nil {
		return nil, err
	}
	org, err := o.st.Repos().PowerDesk().GetOrg(ctx, orgID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, authz.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var props []*powerdesk.Property
	if propertyID != nil {
		p, err := o.st.Repos().PowerDesk().GetProperty(ctx, *propertyID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, authz.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if p.OrganisationID != orgID {
			return nil, authz.ErrNotFound
		}
		props = []*powerdesk.Property{p}
	} else {
		props, err = o.st.Repos().PowerDesk().ListPropertiesForOrg(ctx, orgID)
		if err != nil {
			return nil, err
		}
	}

	ums, err := o.st.Repos().PowerDesk().ListUnitMetersForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	umByUnit := make(map[uuid.UUID]*powerdesk.UnitMeter, len(ums))
	for _, um := range ums {
		umByUnit[um.UnitID] = um
	}

	items, err := o.st.Repos().PowerDesk().ListBulkTopupItemsForOrg(ctx, orgID, propertyID)
	if err != nil {
		return nil, err
	}

	type unitAgg struct {
		elecMinor     int64
		feesMinor     int64
		refundsMinor  int64
		vendedCount   int
		refundedCount int
		lastMeterID   uuid.UUID
	}
	aggByUnit := make(map[uuid.UUID]*unitAgg)
	getAgg := func(uid uuid.UUID) *unitAgg {
		a, ok := aggByUnit[uid]
		if !ok {
			a = &unitAgg{}
			aggByUnit[uid] = a
		}
		return a
	}

	for _, it := range items {
		a := getAgg(it.UnitID)
		a.lastMeterID = it.MeterID

		if it.TransactionID != nil {
			lts, err := o.st.Repos().Ledger().BySource(ctx, *it.TransactionID)
			if err != nil {
				return nil, err
			}
			for _, lt := range lts {
				if lt.PostedAt.Before(startUTC) || !lt.PostedAt.Before(endUTC) {
					continue
				}
				switch {
				case strings.HasPrefix(lt.Reference, "vend:"):
					a.vendedCount++
					for _, e := range lt.Entries {
						if e.Direction == ledger.Credit && e.Account == ledger.ProviderPayable {
							a.elecMinor += e.Amount.Minor
						}
						if e.Direction == ledger.Credit && e.Account == ledger.PlatformFeeRevenue {
							a.feesMinor += e.Amount.Minor
						}
					}
				case strings.HasPrefix(lt.Reference, "refund-pot:") || strings.HasPrefix(lt.Reference, "refund:"):
					a.refundedCount++
					for _, e := range lt.Entries {
						if e.Direction == ledger.Debit && e.Account == ledger.CustomerFundsClearing {
							a.refundsMinor += e.Amount.Minor
						}
					}
				}
			}
		}

		if it.FailureReason != "" && it.TransactionID == nil {
			lts, err := o.st.Repos().Ledger().BySource(ctx, it.ID)
			if err != nil {
				return nil, err
			}
			for _, lt := range lts {
				if lt.PostedAt.Before(startUTC) || !lt.PostedAt.Before(endUTC) {
					continue
				}
				if strings.HasPrefix(lt.Reference, "pot-unalloc-refund-due:") {
					a.refundedCount++
					for _, e := range lt.Entries {
						if e.Direction == ledger.Credit && e.Account == ledger.RefundPayable {
							a.refundsMinor += e.Amount.Minor
						}
					}
				}
			}
		}
	}

	st := &powerdesk.MonthlyStatement{
		OrganisationID:   org.ID,
		OrganisationName: org.Name,
		Month:            canonicalMonth,
		Timezone:         "Africa/Lagos",
		Description:      powerdesk.StatementDescription,
		Currency:         money.NGN,
		Properties:       make([]powerdesk.PropertyStatement, 0, len(props)),
	}

	for _, p := range props {
		units, err := o.st.Repos().PowerDesk().ListUnitsForProperty(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		ps := powerdesk.PropertyStatement{
			PropertyID:   p.ID,
			PropertyName: p.Name,
			Description:  powerdesk.StatementDescription,
			Currency:     money.NGN,
			Units:        make([]powerdesk.UnitStatementRow, 0, len(units)),
		}
		for _, u := range units {
			a := aggByUnit[u.ID]
			var elec, fees, refunds int64
			var vCnt, rCnt int
			meterID := uuid.Nil
			if um, ok := umByUnit[u.ID]; ok {
				meterID = um.MeterID
			}
			if a != nil {
				elec = a.elecMinor
				fees = a.feesMinor
				refunds = a.refundsMinor
				vCnt = a.vendedCount
				rCnt = a.refundedCount
				if meterID == uuid.Nil {
					meterID = a.lastMeterID
				}
			}
			var masked, disco string
			if meterID != uuid.Nil {
				if m, mErr := o.st.Repos().Meters().Get(ctx, meterID); mErr == nil {
					masked = m.Masked
					disco = m.DiscoCode
				}
			}
			netSettled := elec + fees
			row := powerdesk.UnitStatementRow{
				PropertyID:                p.ID,
				PropertyName:              p.Name,
				UnitID:                    u.ID,
				UnitName:                  u.UnitName,
				MaskedMeterNumber:         masked,
				Disco:                     disco,
				Description:               powerdesk.StatementDescription,
				ElectricityPurchasedMinor: elec,
				FeesMinor:                 fees,
				RefundsMinor:              refunds,
				NetChargedMinor:           netSettled,
				Currency:                  money.NGN,
				VendedCount:               vCnt,
				RefundedCount:             rCnt,
			}
			ps.Units = append(ps.Units, row)
			ps.ElectricityPurchasedMinor += elec
			ps.FeesMinor += fees
			ps.RefundsMinor += refunds
			ps.NetChargedMinor += netSettled
			ps.VendedCount += vCnt
			ps.RefundedCount += rCnt
		}
		st.Properties = append(st.Properties, ps)
		st.ElectricityPurchasedMinor += ps.ElectricityPurchasedMinor
		st.FeesMinor += ps.FeesMinor
		st.RefundsMinor += ps.RefundsMinor
		st.NetChargedMinor += ps.NetChargedMinor
		st.VendedCount += ps.VendedCount
		st.RefundedCount += ps.RefundedCount
	}

	return st, nil
}
