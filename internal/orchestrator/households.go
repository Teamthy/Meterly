package orchestrator

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
)

func emitHH(ctx context.Context, r store.Repos, typ string, id uuid.UUID, payload map[string]any) error {
	return r.Outbox().Add(ctx, events.NewForAggregate("household", typ, id, payload))
}

func (o *Orchestrator) VerifyOwnershipByName(ctx context.Context, userID string, meterID uuid.UUID) (households.Ownership, error) {
	var out households.Ownership
	if !o.ownershipNameMatch {
		return out, households.ErrSelfVerificationDisabled
	}
	now := o.Now().UTC()
	err := o.st.InTx(ctx, func(r store.Repos) error {
		m, err := r.Meters().GetForUpdate(ctx, meterID)
		if err != nil {
			return err
		}
		gr, err := r.Meters().ActiveGrant(ctx, meterID, userID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !gr.Permission.Allows(meters.PermPurchase)) {
			return households.ErrForbidden
		}
		if err != nil {
			return err
		}
		contacts, err := r.Users().Contacts(ctx, userID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		name := notifications.SanitizeDisplayName(contacts.DisplayName)
		if name == "" {
			return households.ErrNoDisplayName
		}
		if !households.NamesMatch(m.CustomerName, name) {
			return households.ErrNameMismatch
		}
		if _, err := r.Households().ActiveOwnership(ctx, meterID, userID); err == nil {
			return households.ErrAlreadyOwner
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		out = households.Ownership{
			ID: uuid.New(), MeterID: meterID, UserID: userID,
			Method: households.MethodAccountNameMatch, Reason: "verified display name matches disco customer_name",
			VerifiedBy: userID, VerifiedAt: now,
		}
		if err := r.Households().UpsertOwnership(ctx, out); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("meter.ownership.verified", "meter", meterID, map[string]any{"method": string(households.MethodAccountNameMatch)})); err != nil {
			return err
		}
		return emitHH(ctx, r, "meter.ownership.verified", meterID, map[string]any{"method": string(households.MethodAccountNameMatch)})
	})
	return out, err
}

func (o *Orchestrator) AttestOwnership(ctx context.Context, actor audit.Actor, meterID uuid.UUID, userID string) (households.Ownership, error) {
	var out households.Ownership
	now := o.Now().UTC()
	userID = strings.TrimSpace(userID)
	if userID == "" || actor.UserID == "" {
		return out, households.ErrForbidden
	}
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if _, err := r.Meters().GetForUpdate(ctx, meterID); err != nil {
			return err
		}
		out = households.Ownership{
			ID: uuid.New(), MeterID: meterID, UserID: userID,
			Method: households.MethodOperatorAttestation, Reason: actor.Reason,
			VerifiedBy: actor.UserID, VerifiedAt: now,
		}
		if err := r.Households().UpsertOwnership(ctx, out); err != nil {
			return err
		}
		if err := r.Audit().Add(ctx, actor.Event("meter.ownership.attested", "meter", meterID, map[string]any{"method": string(households.MethodOperatorAttestation)})); err != nil {
			return err
		}
		return emitHH(ctx, r, "meter.ownership.verified", meterID, map[string]any{"method": string(households.MethodOperatorAttestation)})
	})
	return out, err
}

func (o *Orchestrator) ListOwnership(ctx context.Context, userID string, meterID uuid.UUID) ([]households.Ownership, error) {
	if _, err := o.st.Repos().Meters().ActiveGrant(ctx, meterID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if _, herr := o.st.Repos().Households().PermissionForMeter(ctx, userID, meterID); herr != nil {
				return nil, households.ErrForbidden
			}
		} else {
			return nil, err
		}
	}
	return o.st.Repos().Households().ListOwnership(ctx, meterID)
}

func (o *Orchestrator) CreateHousehold(ctx context.Context, userID, name string) (households.Household, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return households.Household{}, errors.New("households: name required (1–80 characters)")
	}
	now := o.Now().UTC()
	h := households.Household{ID: uuid.New(), OwnerID: userID, Name: name, CreatedAt: now, UpdatedAt: now}
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Households().Create(ctx, h); err != nil {
			return err
		}
		acc := now
		mem := households.Member{
			ID: uuid.New(), HouseholdID: h.ID, UserID: userID, Role: households.RoleOwner,
			Status: households.StatusActive, InvitedBy: userID, InvitedAt: now, AcceptedAt: &acc,
		}
		if err := r.Households().UpsertMember(ctx, mem); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("household.created", "household", h.ID, map[string]any{"name_len": len(name)})); err != nil {
			return err
		}
		return emitHH(ctx, r, "household.created", h.ID, nil)
	})
	return h, err
}

func (o *Orchestrator) GetHousehold(ctx context.Context, userID string, id uuid.UUID) (households.View, error) {
	h, err := o.st.Repos().Households().Get(ctx, id)
	if err != nil {
		return households.View{}, err
	}
	mem, err := o.st.Repos().Households().Member(ctx, id, userID)
	if err != nil {
		return households.View{}, households.ErrForbidden
	}
	if mem.Status != households.StatusActive && mem.Status != households.StatusInvited {
		return households.View{}, households.ErrForbidden
	}
	members, err := o.st.Repos().Households().ListMembers(ctx, id)
	if err != nil {
		return households.View{}, err
	}
	links, err := o.st.Repos().Households().ListMeters(ctx, id)
	if err != nil {
		return households.View{}, err
	}
	return households.View{Household: h, Members: members, Meters: links, MyRole: mem.Role, MyStatus: mem.Status}, nil
}

func (o *Orchestrator) ListHouseholds(ctx context.Context, userID string) ([]households.View, error) {
	ids, err := o.st.Repos().Households().HouseholdIDsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]households.View, 0, len(ids))
	for _, id := range ids {
		v, err := o.GetHousehold(ctx, userID, id)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func (o *Orchestrator) AddHouseholdMeter(ctx context.Context, userID string, householdID, meterID uuid.UUID) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		if _, err := r.Meters().GetForUpdate(ctx, meterID); err != nil {
			return err
		}
		if err := requireHouseholdAdmin(ctx, r, householdID, userID); err != nil {
			return err
		}
		if _, err := r.Households().ActiveOwnership(ctx, meterID, userID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return households.ErrNotVerifiedOwner
			}
			return err
		}
		if err := r.Households().AddMeter(ctx, households.MeterLink{HouseholdID: householdID, MeterID: meterID, AddedBy: userID, CreatedAt: now}); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("household.meter.added", "household", householdID, nil)); err != nil {
			return err
		}
		return emitHH(ctx, r, "household.meter.added", householdID, nil)
	})
}

func (o *Orchestrator) InviteHouseholdMember(ctx context.Context, userID string, householdID uuid.UUID, inviteeID string, role households.Role) (households.Member, error) {
	var mem households.Member
	inviteeID = strings.TrimSpace(inviteeID)
	if inviteeID == "" || inviteeID == userID {
		return mem, households.ErrInvalidInvite
	}
	if role == households.RoleOwner || role == "" {
		return mem, households.ErrInvalidRole
	}
	if _, err := households.ParseRole(string(role)); err != nil {
		return mem, err
	}
	now := o.Now().UTC()
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := requireHouseholdAdmin(ctx, r, householdID, userID); err != nil {
			return err
		}
		if existing, err := r.Households().Member(ctx, householdID, inviteeID); err == nil {
			if existing.Status == households.StatusActive || existing.Status == households.StatusInvited {
				return households.ErrAlreadyMember
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		mem = households.Member{
			ID: uuid.New(), HouseholdID: householdID, UserID: inviteeID, Role: role,
			Status: households.StatusInvited, InvitedBy: userID, InvitedAt: now,
		}
		if err := r.Households().UpsertMember(ctx, mem); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("household.member.invited", "household", householdID, map[string]any{"role": string(role)})); err != nil {
			return err
		}
		return emitHH(ctx, r, "household.member.invited", householdID, map[string]any{"role": string(role)})
	})
	return mem, err
}

func (o *Orchestrator) AcceptHouseholdInvite(ctx context.Context, userID string, householdID uuid.UUID) (households.Member, error) {
	var mem households.Member
	now := o.Now().UTC()
	err := o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Households().Member(ctx, householdID, userID)
		if err != nil {
			return err
		}
		if cur.Status != households.StatusInvited {
			return households.ErrInvalidInvite
		}
		cur.Status = households.StatusActive
		cur.AcceptedAt = &now
		if err := r.Households().UpsertMember(ctx, cur); err != nil {
			return err
		}
		mem = cur
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("household.member.accepted", "household", householdID, map[string]any{"role": string(cur.Role)})); err != nil {
			return err
		}
		return emitHH(ctx, r, "household.member.accepted", householdID, map[string]any{"role": string(cur.Role)})
	})
	return mem, err
}

func (o *Orchestrator) DeclineHouseholdInvite(ctx context.Context, userID string, householdID uuid.UUID) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Households().Member(ctx, householdID, userID)
		if err != nil {
			return err
		}
		if cur.Status != households.StatusInvited {
			return households.ErrInvalidInvite
		}
		cur.Status = households.StatusDeclined
		cur.RevokedAt = &now
		return r.Households().UpsertMember(ctx, cur)
	})
}

func (o *Orchestrator) RevokeHouseholdMember(ctx context.Context, userID string, householdID uuid.UUID, memberUserID string) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		if err := requireHouseholdAdmin(ctx, r, householdID, userID); err != nil {
			return err
		}
		h, err := r.Households().Get(ctx, householdID)
		if err != nil {
			return err
		}
		if memberUserID == h.OwnerID {
			return households.ErrCannotModifyOwner
		}
		cur, err := r.Households().Member(ctx, householdID, memberUserID)
		if err != nil {
			return err
		}
		cur.Status = households.StatusRevoked
		cur.RevokedAt = &now
		if err := r.Households().UpsertMember(ctx, cur); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("household.member.revoked", "household", householdID, nil)); err != nil {
			return err
		}
		return emitHH(ctx, r, "household.member.revoked", householdID, nil)
	})
}

func requireHouseholdAdmin(ctx context.Context, r store.Repos, householdID uuid.UUID, userID string) error {
	mem, err := r.Households().Member(ctx, householdID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return households.ErrForbidden
		}
		return err
	}
	if mem.Status != households.StatusActive || !households.CanAdminister(mem.Role) {
		return households.ErrForbidden
	}
	return nil
}
