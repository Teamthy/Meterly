package pgstore

import (
	"context"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type householdRepo struct{ *repos }

func (r householdRepo) Create(ctx context.Context, h households.Household) error {
	_, err := r.db.Exec(ctx, `INSERT INTO households (id, owner_user_id, name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5)`, h.ID, h.OwnerID, h.Name, h.CreatedAt, h.UpdatedAt)
	return mapErr(err)
}

func (r householdRepo) Get(ctx context.Context, id uuid.UUID) (households.Household, error) {
	var h households.Household
	err := r.db.QueryRow(ctx, `SELECT id, owner_user_id::text, name, created_at, updated_at
		FROM households WHERE id = $1`, id).Scan(&h.ID, &h.OwnerID, &h.Name, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return households.Household{}, mapErr(err)
	}
	h.CreatedAt, h.UpdatedAt = h.CreatedAt.UTC(), h.UpdatedAt.UTC()
	return h, nil
}

func (r householdRepo) HouseholdIDsForUser(ctx context.Context, userID string) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `SELECT DISTINCT household_id FROM household_members
		WHERE user_id = $1 AND status IN ('INVITED','ACTIVE')`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r householdRepo) UpsertMember(ctx context.Context, m households.Member) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO household_members (id, household_id, user_id, role, status, invited_by, invited_at, accepted_at, revoked_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
		ON CONFLICT (household_id, user_id) DO UPDATE SET
			role = EXCLUDED.role, status = EXCLUDED.status, invited_by = EXCLUDED.invited_by,
			invited_at = EXCLUDED.invited_at, accepted_at = EXCLUDED.accepted_at,
			revoked_at = EXCLUDED.revoked_at, updated_at = now()`,
		m.ID, m.HouseholdID, m.UserID, m.Role, m.Status, nullUUID(m.InvitedBy), m.InvitedAt, m.AcceptedAt, m.RevokedAt)
	return mapErr(err)
}

func (r householdRepo) Member(ctx context.Context, householdID uuid.UUID, userID string) (households.Member, error) {
	var m households.Member
	err := r.db.QueryRow(ctx, `SELECT id, household_id, user_id::text, role, status,
			coalesce(invited_by::text, ''), invited_at, accepted_at, revoked_at
		FROM household_members WHERE household_id = $1 AND user_id = $2`, householdID, userID).
		Scan(&m.ID, &m.HouseholdID, &m.UserID, &m.Role, &m.Status, &m.InvitedBy, &m.InvitedAt, &m.AcceptedAt, &m.RevokedAt)
	if err != nil {
		return households.Member{}, mapErr(err)
	}
	m.InvitedAt = m.InvitedAt.UTC()
	return m, nil
}

func (r householdRepo) ListMembers(ctx context.Context, householdID uuid.UUID) ([]households.Member, error) {
	rows, err := r.db.Query(ctx, `SELECT id, household_id, user_id::text, role, status,
			coalesce(invited_by::text, ''), invited_at, accepted_at, revoked_at
		FROM household_members WHERE household_id = $1 ORDER BY invited_at`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []households.Member
	for rows.Next() {
		var m households.Member
		if err := rows.Scan(&m.ID, &m.HouseholdID, &m.UserID, &m.Role, &m.Status, &m.InvitedBy, &m.InvitedAt, &m.AcceptedAt, &m.RevokedAt); err != nil {
			return nil, err
		}
		m.InvitedAt = m.InvitedAt.UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r householdRepo) AddMeter(ctx context.Context, link households.MeterLink) error {
	_, err := r.db.Exec(ctx, `INSERT INTO household_meters (household_id, meter_id, added_by, created_at)
		VALUES ($1,$2,$3,$4)`, link.HouseholdID, link.MeterID, link.AddedBy, link.CreatedAt)
	return mapErr(err)
}

func (r householdRepo) ListMeters(ctx context.Context, householdID uuid.UUID) ([]households.MeterLink, error) {
	rows, err := r.db.Query(ctx, `SELECT household_id, meter_id, added_by::text, created_at
		FROM household_meters WHERE household_id = $1 ORDER BY created_at`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []households.MeterLink
	for rows.Next() {
		var l households.MeterLink
		if err := rows.Scan(&l.HouseholdID, &l.MeterID, &l.AddedBy, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.CreatedAt = l.CreatedAt.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r householdRepo) UpsertOwnership(ctx context.Context, o households.Ownership) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO meter_ownership (id, meter_id, user_id, method, reason, verified_by, verified_at, revoked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (meter_id, user_id) DO UPDATE SET
			method = EXCLUDED.method, reason = EXCLUDED.reason, verified_by = EXCLUDED.verified_by,
			verified_at = EXCLUDED.verified_at, revoked_at = EXCLUDED.revoked_at`,
		o.ID, o.MeterID, o.UserID, o.Method, o.Reason, o.VerifiedBy, o.VerifiedAt, o.RevokedAt)
	return mapErr(err)
}

func (r householdRepo) ActiveOwnership(ctx context.Context, meterID uuid.UUID, userID string) (households.Ownership, error) {
	var o households.Ownership
	err := r.db.QueryRow(ctx, `SELECT id, meter_id, user_id::text, method, reason, verified_by::text, verified_at, revoked_at
		FROM meter_ownership WHERE meter_id = $1 AND user_id = $2 AND revoked_at IS NULL`, meterID, userID).
		Scan(&o.ID, &o.MeterID, &o.UserID, &o.Method, &o.Reason, &o.VerifiedBy, &o.VerifiedAt, &o.RevokedAt)
	if err != nil {
		return households.Ownership{}, mapErr(err)
	}
	o.VerifiedAt = o.VerifiedAt.UTC()
	return o, nil
}

func (r householdRepo) ListOwnership(ctx context.Context, meterID uuid.UUID) ([]households.Ownership, error) {
	rows, err := r.db.Query(ctx, `SELECT id, meter_id, user_id::text, method, reason, verified_by::text, verified_at, revoked_at
		FROM meter_ownership WHERE meter_id = $1 AND revoked_at IS NULL ORDER BY verified_at`, meterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []households.Ownership
	for rows.Next() {
		var o households.Ownership
		if err := rows.Scan(&o.ID, &o.MeterID, &o.UserID, &o.Method, &o.Reason, &o.VerifiedBy, &o.VerifiedAt, &o.RevokedAt); err != nil {
			return nil, err
		}
		o.VerifiedAt = o.VerifiedAt.UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r householdRepo) PermissionForMeter(ctx context.Context, userID string, meterID uuid.UUID) (meters.Permission, error) {
	rows, err := r.db.Query(ctx, `
		SELECT hm.role FROM household_members hm
		JOIN household_meters ht ON ht.household_id = hm.household_id
		WHERE hm.user_id = $1 AND ht.meter_id = $2 AND hm.status = 'ACTIVE'`, userID, meterID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var best meters.Permission
	found := false
	for rows.Next() {
		var role households.Role
		if err := rows.Scan(&role); err != nil {
			return "", err
		}
		p := households.PermissionForRole(role)
		if p == "" {
			continue
		}
		if !found || p.Allows(best) {
			best = p
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", store.ErrNotFound
	}
	return best, nil
}
