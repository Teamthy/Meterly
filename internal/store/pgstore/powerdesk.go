package pgstore

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/store"
)

type powerDeskRepo struct{ *repos }

func (r *repos) orgRoleID(ctx context.Context, role powerdesk.Role) (uuid.UUID, error) {
	if !role.Valid() {
		return uuid.Nil, powerdesk.ErrInvalidRole
	}
	return r.refID(ctx, "orgrole:"+string(role), `
		INSERT INTO roles (name, scope, description) VALUES ($1, 'ORGANISATION', $1)
		ON CONFLICT (scope, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, string(role))
}

// ---- organisations ---------------------------------------------------------

const orgSelectSQL = `
	SELECT id, name, coalesce(legal_name, ''), coalesce(registration_number, ''),
	       organisation_type, verification_status, manager_approval_threshold_minor,
	       created_at, updated_at
	FROM organisations`

func scanOrg(row pgx.Row) (*powerdesk.Organisation, error) {
	var (
		o             powerdesk.Organisation
		orgType, vSts string
	)
	if err := row.Scan(
		&o.ID, &o.Name, &o.LegalName, &o.RegistrationNumber,
		&orgType, &vSts, &o.ManagerApprovalThresholdMinor,
		&o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	o.OrganisationType = powerdesk.OrganisationType(orgType)
	o.VerificationStatus = powerdesk.VerificationStatus(vSts)
	o.CreatedAt = o.CreatedAt.UTC()
	o.UpdatedAt = o.UpdatedAt.UTC()
	return &o, nil
}

func (r powerDeskRepo) CreateOrg(ctx context.Context, org *powerdesk.Organisation) error {
	if org.ID == uuid.Nil {
		org.ID = uuid.New()
	}
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	} else {
		org.CreatedAt = org.CreatedAt.UTC()
	}
	if org.UpdatedAt.IsZero() {
		org.UpdatedAt = org.CreatedAt
	} else {
		org.UpdatedAt = org.UpdatedAt.UTC()
	}
	if org.VerificationStatus == "" {
		org.VerificationStatus = powerdesk.VerificationPending
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO organisations (
			id, name, legal_name, registration_number,
			organisation_type, verification_status,
			manager_approval_threshold_minor, created_at, updated_at
		) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8,$9)`,
		org.ID, org.Name, org.LegalName, org.RegistrationNumber,
		string(org.OrganisationType), string(org.VerificationStatus),
		org.ManagerApprovalThresholdMinor, org.CreatedAt, org.UpdatedAt,
	)
	return mapErr(err)
}

func (r powerDeskRepo) GetOrg(ctx context.Context, id uuid.UUID) (*powerdesk.Organisation, error) {
	return scanOrg(r.db.QueryRow(ctx, orgSelectSQL+` WHERE id = $1`, id))
}

func (r powerDeskRepo) GetOrgForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.Organisation, error) {
	return scanOrg(r.db.QueryRow(ctx, orgSelectSQL+` WHERE id = $1 FOR UPDATE`, id))
}

func (r powerDeskRepo) UpdateOrg(ctx context.Context, org *powerdesk.Organisation) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE organisations SET
			name                             = $2,
			legal_name                       = NULLIF($3, ''),
			registration_number              = NULLIF($4, ''),
			verification_status              = $5,
			manager_approval_threshold_minor = $6,
			updated_at                       = $7
		WHERE id = $1`,
		org.ID, org.Name, org.LegalName, org.RegistrationNumber,
		string(org.VerificationStatus), org.ManagerApprovalThresholdMinor,
		org.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- members ---------------------------------------------------------------

func (r powerDeskRepo) UpsertMember(ctx context.Context, m *powerdesk.Member) error {
	uid, err := uuid.Parse(m.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	roleID, err := r.orgRoleID(ctx, m.Role)
	if err != nil {
		return err
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	} else {
		m.CreatedAt = m.CreatedAt.UTC()
	}
	m.Status = powerdesk.MemberActive
	err = r.db.QueryRow(ctx, `
		INSERT INTO organisation_members (id, organisation_id, user_id, role_id, status, created_at)
		VALUES ($1, $2, $3, $4, 'ACTIVE', $5)
		ON CONFLICT (organisation_id, user_id) DO UPDATE SET
			role_id = EXCLUDED.role_id,
			status  = 'ACTIVE'
		RETURNING id, created_at`,
		m.ID, m.OrganisationID, uid, roleID, m.CreatedAt,
	).Scan(&m.ID, &m.CreatedAt)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return nil
}

func (r powerDeskRepo) GetActiveMemberRole(ctx context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return "", store.ErrNotFound
	}
	var roleName string
	err = r.db.QueryRow(ctx, `
		SELECT rl.name
		FROM organisation_members om
		JOIN roles rl ON rl.id = om.role_id
		WHERE om.organisation_id = $1 AND om.user_id = $2 AND om.status = 'ACTIVE'`,
		orgID, uid,
	).Scan(&roleName)
	if err != nil {
		return "", mapErr(err)
	}
	return powerdesk.Role(roleName), nil
}

func (r powerDeskRepo) GetMemberForUpdate(ctx context.Context, orgID uuid.UUID, userID string) (*powerdesk.Member, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	var (
		m              powerdesk.Member
		roleName, stat string
	)
	err = r.db.QueryRow(ctx, `
		SELECT om.id, om.organisation_id, om.user_id::text, rl.name, om.status, om.created_at
		FROM organisation_members om
		JOIN roles rl ON rl.id = om.role_id
		WHERE om.organisation_id = $1 AND om.user_id = $2 AND om.status = 'ACTIVE'
		FOR UPDATE OF om`,
		orgID, uid,
	).Scan(&m.ID, &m.OrganisationID, &m.UserID, &roleName, &stat, &m.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	m.Role = powerdesk.Role(roleName)
	m.Status = powerdesk.MemberStatus(stat)
	m.CreatedAt = m.CreatedAt.UTC()
	return &m, nil
}

func (r powerDeskRepo) ListActiveMembers(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Member, error) {
	rows, err := r.db.Query(ctx, `
		SELECT om.id, om.organisation_id, om.user_id::text, rl.name, om.status, om.created_at
		FROM organisation_members om
		JOIN roles rl ON rl.id = om.role_id
		WHERE om.organisation_id = $1 AND om.status = 'ACTIVE'
		ORDER BY om.created_at ASC, om.id ASC`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []*powerdesk.Member{}
	for rows.Next() {
		var (
			m              powerdesk.Member
			roleName, stat string
		)
		if err := rows.Scan(&m.ID, &m.OrganisationID, &m.UserID, &roleName, &stat, &m.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		m.Role = powerdesk.Role(roleName)
		m.Status = powerdesk.MemberStatus(stat)
		m.CreatedAt = m.CreatedAt.UTC()
		out = append(out, &m)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) ListOrgsForUser(ctx context.Context, userID string) ([]*powerdesk.Organisation, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	rows, err := r.db.Query(ctx, orgSelectSQL+`
		WHERE id IN (
			SELECT organisation_id FROM organisation_members
			WHERE user_id = $1 AND status = 'ACTIVE'
		)
		ORDER BY created_at ASC, id ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.Organisation{}
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) CountActiveOwners(ctx context.Context, orgID uuid.UUID) (int, error) {
	var cnt int
	err := r.db.QueryRow(ctx, `
		SELECT count(*)
		FROM organisation_members om
		JOIN roles rl ON rl.id = om.role_id
		WHERE om.organisation_id = $1 AND om.status = 'ACTIVE' AND rl.name = 'OWNER'`,
		orgID,
	).Scan(&cnt)
	return cnt, mapErr(err)
}

func (r powerDeskRepo) RemoveMember(ctx context.Context, orgID uuid.UUID, userID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE organisation_members
		SET status = 'REMOVED'
		WHERE organisation_id = $1 AND user_id = $2 AND status = 'ACTIVE'`,
		orgID, uid,
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- properties, buildings, units ------------------------------------------

const propertySelectSQL = `
	SELECT id, organisation_id, coalesce(owner_user_id::text, ''), name,
	       coalesce(address, ''), coalesce(city, ''), coalesce(state, ''),
	       trim(country), created_at
	FROM properties`

func scanProperty(row pgx.Row) (*powerdesk.Property, error) {
	var p powerdesk.Property
	if err := row.Scan(
		&p.ID, &p.OrganisationID, &p.OwnerUserID, &p.Name,
		&p.Address, &p.City, &p.State, &p.Country, &p.CreatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return &p, nil
}

func (r powerDeskRepo) CreateProperty(ctx context.Context, p *powerdesk.Property) error {
	var ownerParam any
	if p.OwnerUserID != "" {
		uid, err := uuid.Parse(p.OwnerUserID)
		if err != nil {
			return store.ErrNotFound
		}
		ownerParam = uid
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	} else {
		p.CreatedAt = p.CreatedAt.UTC()
	}
	if strings.TrimSpace(p.Country) == "" {
		p.Country = "NG"
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO properties (
			id, organisation_id, owner_user_id, name, address, city, state, country, created_at
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8,$9)`,
		p.ID, p.OrganisationID, ownerParam, p.Name,
		p.Address, p.City, p.State, p.Country, p.CreatedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r powerDeskRepo) GetProperty(ctx context.Context, id uuid.UUID) (*powerdesk.Property, error) {
	return scanProperty(r.db.QueryRow(ctx, propertySelectSQL+` WHERE id = $1`, id))
}

func (r powerDeskRepo) ListPropertiesForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Property, error) {
	rows, err := r.db.Query(ctx, propertySelectSQL+` WHERE organisation_id = $1 ORDER BY created_at ASC, id ASC`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.Property{}
	for rows.Next() {
		p, err := scanProperty(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) EnsureDefaultBuilding(ctx context.Context, propertyID uuid.UUID, name string, now time.Time) (*powerdesk.Building, error) {
	clean := strings.TrimSpace(name)
	if clean == "" {
		clean = powerdesk.DefaultBuildingName
	}
	var b powerdesk.Building
	err := r.db.QueryRow(ctx, `
		INSERT INTO buildings (id, property_id, name, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (property_id, lower(name)) DO UPDATE SET name = buildings.name
		RETURNING id, property_id, name, created_at`,
		uuid.New(), propertyID, clean, now.UTC(),
	).Scan(&b.ID, &b.PropertyID, &b.Name, &b.CreatedAt)
	if isFKViolation(err) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, mapErr(err)
	}
	b.CreatedAt = b.CreatedAt.UTC()
	return &b, nil
}

func (r powerDeskRepo) GetBuilding(ctx context.Context, id uuid.UUID) (*powerdesk.Building, error) {
	var b powerdesk.Building
	if err := r.db.QueryRow(ctx, `SELECT id, property_id, name, created_at FROM buildings WHERE id = $1`, id).
		Scan(&b.ID, &b.PropertyID, &b.Name, &b.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	b.CreatedAt = b.CreatedAt.UTC()
	return &b, nil
}

func (r powerDeskRepo) ListBuildingsForProperty(ctx context.Context, propertyID uuid.UUID) ([]*powerdesk.Building, error) {
	rows, err := r.db.Query(ctx, `SELECT id, property_id, name, created_at FROM buildings WHERE property_id = $1 ORDER BY created_at ASC, id ASC`, propertyID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.Building{}
	for rows.Next() {
		var b powerdesk.Building
		if err := rows.Scan(&b.ID, &b.PropertyID, &b.Name, &b.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		b.CreatedAt = b.CreatedAt.UTC()
		out = append(out, &b)
	}
	return out, mapErr(rows.Err())
}

const unitSelectSQL = `
	SELECT u.id, p.organisation_id, p.id, u.building_id, u.unit_name,
	       coalesce(u.occupancy_status, ''), u.created_at
	FROM units u
	JOIN buildings b ON b.id = u.building_id
	JOIN properties p ON p.id = b.property_id`

func scanUnit(row pgx.Row) (*powerdesk.Unit, error) {
	var u powerdesk.Unit
	if err := row.Scan(
		&u.ID, &u.OrganisationID, &u.PropertyID, &u.BuildingID,
		&u.UnitName, &u.OccupancyStatus, &u.CreatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return &u, nil
}

func (r powerDeskRepo) CreateUnit(ctx context.Context, u *powerdesk.Unit) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	} else {
		u.CreatedAt = u.CreatedAt.UTC()
	}
	err := r.db.QueryRow(ctx, `
		SELECT p.id, p.organisation_id
		FROM buildings b
		JOIN properties p ON p.id = b.property_id
		WHERE b.id = $1`, u.BuildingID).Scan(&u.PropertyID, &u.OrganisationID)
	if err != nil {
		return mapErr(err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO units (id, building_id, unit_name, occupancy_status, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)`,
		u.ID, u.BuildingID, u.UnitName, u.OccupancyStatus, u.CreatedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r powerDeskRepo) GetUnit(ctx context.Context, id uuid.UUID) (*powerdesk.Unit, error) {
	return scanUnit(r.db.QueryRow(ctx, unitSelectSQL+` WHERE u.id = $1`, id))
}

func (r powerDeskRepo) ListUnitsForProperty(ctx context.Context, propertyID uuid.UUID) ([]*powerdesk.Unit, error) {
	rows, err := r.db.Query(ctx, unitSelectSQL+` WHERE p.id = $1 ORDER BY u.created_at ASC, u.id ASC`, propertyID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.Unit{}
	for rows.Next() {
		u, err := scanUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) ListUnitsForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Unit, error) {
	rows, err := r.db.Query(ctx, unitSelectSQL+` WHERE p.organisation_id = $1 ORDER BY u.created_at ASC, u.id ASC`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.Unit{}
	for rows.Next() {
		u, err := scanUnit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, mapErr(rows.Err())
}

// ---- unit_meters -----------------------------------------------------------

const unitMeterSelectSQL = `
	SELECT id, organisation_id, property_id, unit_id, meter_id, status,
	       verified, verification_method, evidence_reference, disco_account_name,
	       coalesce(verified_by_user_id::text, ''), verified_at, verification_reason,
	       created_by_user_id::text, created_at, updated_at
	FROM unit_meters`

func scanUnitMeter(row pgx.Row) (*powerdesk.UnitMeter, error) {
	var (
		um         powerdesk.UnitMeter
		stat, vMet string
		verifiedAt *time.Time
	)
	if err := row.Scan(
		&um.ID, &um.OrganisationID, &um.PropertyID, &um.UnitID, &um.MeterID, &stat,
		&um.Verified, &vMet, &um.EvidenceReference, &um.DiscoAccountName,
		&um.VerifiedByUserID, &verifiedAt, &um.VerificationReason,
		&um.CreatedByUserID, &um.CreatedAt, &um.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	um.Status = powerdesk.UnitMeterStatus(stat)
	um.VerificationMethod = powerdesk.VerificationMethod(vMet)
	um.CreatedAt = um.CreatedAt.UTC()
	um.UpdatedAt = um.UpdatedAt.UTC()
	if verifiedAt != nil {
		t := verifiedAt.UTC()
		um.VerifiedAt = &t
	}
	return &um, nil
}

func (r powerDeskRepo) AttachUnitMeter(ctx context.Context, um *powerdesk.UnitMeter) error {
	creatorUID, err := uuid.Parse(um.CreatedByUserID)
	if err != nil {
		return store.ErrNotFound
	}
	u, err := r.GetUnit(ctx, um.UnitID)
	if err != nil {
		return err
	}
	um.OrganisationID = u.OrganisationID
	um.PropertyID = u.PropertyID
	if um.ID == uuid.Nil {
		um.ID = uuid.New()
	}
	now := time.Now().UTC()
	if um.CreatedAt.IsZero() {
		um.CreatedAt = now
	} else {
		um.CreatedAt = um.CreatedAt.UTC()
	}
	if um.UpdatedAt.IsZero() {
		um.UpdatedAt = um.CreatedAt
	} else {
		um.UpdatedAt = um.UpdatedAt.UTC()
	}
	if um.Status == "" {
		um.Status = powerdesk.UnitMeterActive
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO unit_meters (
			id, organisation_id, property_id, unit_id, meter_id, status,
			verified, verification_method, evidence_reference, disco_account_name,
			verified_by_user_id, verified_at, verification_reason,
			created_by_user_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		um.ID, um.OrganisationID, um.PropertyID, um.UnitID, um.MeterID, string(um.Status),
		um.Verified, string(um.VerificationMethod), um.EvidenceReference, um.DiscoAccountName,
		nullUUID(um.VerifiedByUserID), um.VerifiedAt, um.VerificationReason,
		creatorUID, um.CreatedAt, um.UpdatedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r powerDeskRepo) GetUnitMeter(ctx context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error) {
	return scanUnitMeter(r.db.QueryRow(ctx, unitMeterSelectSQL+` WHERE id = $1`, id))
}

func (r powerDeskRepo) GetUnitMeterForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error) {
	return scanUnitMeter(r.db.QueryRow(ctx, unitMeterSelectSQL+` WHERE id = $1 FOR UPDATE`, id))
}

func (r powerDeskRepo) GetActiveUnitMeterByUnit(ctx context.Context, unitID uuid.UUID) (*powerdesk.UnitMeter, error) {
	return scanUnitMeter(r.db.QueryRow(ctx, unitMeterSelectSQL+` WHERE unit_id = $1 AND status = 'ACTIVE'`, unitID))
}

func (r powerDeskRepo) ListUnitMetersForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.UnitMeter, error) {
	rows, err := r.db.Query(ctx, unitMeterSelectSQL+` WHERE organisation_id = $1 AND status = 'ACTIVE' ORDER BY created_at ASC, id ASC`, orgID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.UnitMeter{}
	for rows.Next() {
		um, err := scanUnitMeter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, um)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) UpdateUnitMeter(ctx context.Context, um *powerdesk.UnitMeter) error {
	var verifiedByParam any
	if um.VerifiedByUserID != "" {
		uid, err := uuid.Parse(um.VerifiedByUserID)
		if err != nil {
			return store.ErrNotFound
		}
		verifiedByParam = uid
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE unit_meters SET
			status              = $2,
			verified            = $3,
			verification_method = $4,
			evidence_reference  = $5,
			verified_by_user_id = $6,
			verified_at         = $7,
			verification_reason = $8,
			updated_at          = $9
		WHERE id = $1`,
		um.ID, string(um.Status), um.Verified, string(um.VerificationMethod),
		um.EvidenceReference, verifiedByParam, um.VerifiedAt, um.VerificationReason,
		um.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- bulk_topups & items ---------------------------------------------------

const bulkTopupSelectSQL = `
	SELECT pot_id, organisation_id, created_by_user_id::text, creator_role,
	       approval_status, coalesce(approved_by_user_id::text, ''), approved_at,
	       contribution_id, electricity_total_minor, fee_total_minor, total_minor,
	       trim(currency), idempotency_key, created_at, updated_at
	FROM bulk_topups`

func scanBulkTopup(row pgx.Row) (*powerdesk.BulkTopup, error) {
	var (
		bt                      powerdesk.BulkTopup
		cRole, appSts, currency string
		approvedAt              *time.Time
		contribID               *uuid.UUID
	)
	if err := row.Scan(
		&bt.PotID, &bt.OrganisationID, &bt.CreatedByUserID, &cRole,
		&appSts, &bt.ApprovedByUserID, &approvedAt,
		&contribID, &bt.ElectricityTotalMinor, &bt.FeeTotalMinor, &bt.TotalMinor,
		&currency, &bt.IdempotencyKey, &bt.CreatedAt, &bt.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	bt.CreatorRole = powerdesk.Role(cRole)
	bt.ApprovalStatus = powerdesk.ApprovalStatus(appSts)
	bt.Currency = money.Currency(currency)
	bt.ContributionID = contribID
	bt.CreatedAt = bt.CreatedAt.UTC()
	bt.UpdatedAt = bt.UpdatedAt.UTC()
	if approvedAt != nil {
		t := approvedAt.UTC()
		bt.ApprovedAt = &t
	}
	return &bt, nil
}

const bulkItemSelectSQL = `
	SELECT id, pot_id, organisation_id, property_id, unit_id, unit_meter_id, meter_id,
	       amount_minor, fee_minor, total_minor, trim(currency),
	       transaction_id, allocation_id, failure_reason, created_at, updated_at
	FROM bulk_topup_items`

func scanBulkItem(row pgx.Row) (*powerdesk.BulkTopupItem, error) {
	var (
		it       powerdesk.BulkTopupItem
		currency string
	)
	if err := row.Scan(
		&it.ID, &it.PotID, &it.OrganisationID, &it.PropertyID, &it.UnitID, &it.UnitMeterID, &it.MeterID,
		&it.AmountMinor, &it.FeeMinor, &it.TotalMinor, &currency,
		&it.TransactionID, &it.AllocationID, &it.FailureReason, &it.CreatedAt, &it.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	it.Currency = money.Currency(currency)
	it.CreatedAt = it.CreatedAt.UTC()
	it.UpdatedAt = it.UpdatedAt.UTC()
	return &it, nil
}

func (r powerDeskRepo) CreateBulkTopup(ctx context.Context, bt *powerdesk.BulkTopup, items []*powerdesk.BulkTopupItem) error {
	creatorUID, err := uuid.Parse(bt.CreatedByUserID)
	if err != nil {
		return store.ErrNotFound
	}
	return r.atomic(ctx, func(db DB) error {
		_, err := db.Exec(ctx, `
			INSERT INTO bulk_topups (
				pot_id, organisation_id, created_by_user_id, creator_role,
				approval_status, approved_by_user_id, approved_at, contribution_id,
				electricity_total_minor, fee_total_minor, total_minor,
				currency, idempotency_key, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			bt.PotID, bt.OrganisationID, creatorUID, string(bt.CreatorRole),
			string(bt.ApprovalStatus), nullUUID(bt.ApprovedByUserID), bt.ApprovedAt, bt.ContributionID,
			bt.ElectricityTotalMinor, bt.FeeTotalMinor, bt.TotalMinor,
			string(bt.Currency), bt.IdempotencyKey, bt.CreatedAt.UTC(), bt.UpdatedAt.UTC(),
		)
		if isFKViolation(err) {
			return store.ErrNotFound
		}
		if err != nil {
			return mapErr(err)
		}
		for _, it := range items {
			if it.ID == uuid.Nil {
				it.ID = uuid.New()
			}
			_, err := db.Exec(ctx, `
				INSERT INTO bulk_topup_items (
					id, pot_id, organisation_id, property_id, unit_id, unit_meter_id, meter_id,
					amount_minor, fee_minor, total_minor, currency,
					transaction_id, allocation_id, failure_reason, created_at, updated_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				it.ID, it.PotID, it.OrganisationID, it.PropertyID, it.UnitID, it.UnitMeterID, it.MeterID,
				it.AmountMinor, it.FeeMinor, it.TotalMinor, string(it.Currency),
				it.TransactionID, it.AllocationID, it.FailureReason,
				it.CreatedAt.UTC(), it.UpdatedAt.UTC(),
			)
			if isFKViolation(err) {
				return store.ErrNotFound
			}
			if err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

func (r powerDeskRepo) GetBulkTopup(ctx context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error) {
	return scanBulkTopup(r.db.QueryRow(ctx, bulkTopupSelectSQL+` WHERE pot_id = $1`, potID))
}

func (r powerDeskRepo) GetBulkTopupForUpdate(ctx context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error) {
	return scanBulkTopup(r.db.QueryRow(ctx, bulkTopupSelectSQL+` WHERE pot_id = $1 FOR UPDATE`, potID))
}

func (r powerDeskRepo) GetBulkTopupByIdempotency(ctx context.Context, orgID uuid.UUID, key string) (*powerdesk.BulkTopup, error) {
	if key == "" {
		return nil, store.ErrNotFound
	}
	return scanBulkTopup(r.db.QueryRow(ctx, bulkTopupSelectSQL+` WHERE organisation_id = $1 AND idempotency_key = $2`, orgID, key))
}

func (r powerDeskRepo) UpdateBulkTopup(ctx context.Context, bt *powerdesk.BulkTopup) error {
	var approvedByParam any
	if bt.ApprovedByUserID != "" {
		uid, err := uuid.Parse(bt.ApprovedByUserID)
		if err != nil {
			return store.ErrNotFound
		}
		approvedByParam = uid
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE bulk_topups SET
			approval_status     = $2,
			approved_by_user_id = $3,
			approved_at         = $4,
			contribution_id     = $5,
			updated_at          = $6
		WHERE pot_id = $1`,
		bt.PotID, string(bt.ApprovalStatus), approvedByParam,
		bt.ApprovedAt, bt.ContributionID, bt.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r powerDeskRepo) ListBulkTopupItems(ctx context.Context, potID uuid.UUID) ([]*powerdesk.BulkTopupItem, error) {
	rows, err := r.db.Query(ctx, bulkItemSelectSQL+` WHERE pot_id = $1 ORDER BY created_at ASC, id ASC`, potID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.BulkTopupItem{}
	for rows.Next() {
		it, err := scanBulkItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, mapErr(rows.Err())
}

func (r powerDeskRepo) UpdateBulkTopupItem(ctx context.Context, item *powerdesk.BulkTopupItem) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE bulk_topup_items SET
			transaction_id = $2,
			allocation_id  = $3,
			failure_reason = $4,
			updated_at     = $5
		WHERE id = $1`,
		item.ID, item.TransactionID, item.AllocationID, item.FailureReason, item.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r powerDeskRepo) ListBulkTopupItemsForOrg(ctx context.Context, orgID uuid.UUID, propertyID *uuid.UUID) ([]*powerdesk.BulkTopupItem, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if propertyID != nil {
		rows, err = r.db.Query(ctx, bulkItemSelectSQL+` WHERE organisation_id = $1 AND property_id = $2 ORDER BY created_at ASC, id ASC`, orgID, *propertyID)
	} else {
		rows, err = r.db.Query(ctx, bulkItemSelectSQL+` WHERE organisation_id = $1 ORDER BY created_at ASC, id ASC`, orgID)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []*powerdesk.BulkTopupItem{}
	for rows.Next() {
		it, err := scanBulkItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, mapErr(rows.Err())
}
