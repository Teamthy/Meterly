package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

type partnerRepo struct{ *repos }

func uuidString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func scanPartner(row interface{ Scan(dest ...any) error }) (partners.Partner, error) {
	var p partners.Partner
	var createdBy uuid.UUID
	var proposedBy, activatedBy, suspendedBy *uuid.UUID
	err := row.Scan(
		&p.ID, &p.Organisation, &p.Status, &p.Environment, &p.ContactRef,
		&p.DailyValueCapMinor, &p.RateLimitPerMinute, &createdBy, &p.CreatedAt, &p.UpdatedAt,
		&proposedBy, &p.ProposedActivationAt, &activatedBy, &p.ActivatedAt, &suspendedBy, &p.SuspendedAt,
	)
	if err != nil {
		return partners.Partner{}, mapErr(err)
	}
	p.CreatedBy = createdBy.String()
	p.ProposedActivationBy = uuidString(proposedBy)
	p.ActivatedBy = uuidString(activatedBy)
	p.SuspendedBy = uuidString(suspendedBy)
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	if p.ProposedActivationAt != nil {
		t := p.ProposedActivationAt.UTC()
		p.ProposedActivationAt = &t
	}
	if p.ActivatedAt != nil {
		t := p.ActivatedAt.UTC()
		p.ActivatedAt = &t
	}
	if p.SuspendedAt != nil {
		t := p.SuspendedAt.UTC()
		p.SuspendedAt = &t
	}
	return p, nil
}

const partnerCols = `id, organisation, status, environment, contact_ref,
	daily_value_cap_minor, rate_limit_per_minute, created_by, created_at, updated_at,
	proposed_activation_by, proposed_activation_at, activated_by, activated_at, suspended_by, suspended_at`

func (r partnerRepo) CreatePartner(ctx context.Context, p partners.Partner) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	createdBy, err := uuid.Parse(p.CreatedBy)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO partners (
			id, organisation, status, environment, contact_ref,
			daily_value_cap_minor, rate_limit_per_minute, created_by, created_at, updated_at,
			proposed_activation_by, proposed_activation_at, activated_by, activated_at, suspended_by, suspended_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		p.ID, p.Organisation, p.Status, p.Environment, p.ContactRef,
		p.DailyValueCapMinor, p.RateLimitPerMinute, createdBy, p.CreatedAt.UTC(), p.UpdatedAt.UTC(),
		nullUUID(p.ProposedActivationBy), p.ProposedActivationAt, nullUUID(p.ActivatedBy), p.ActivatedAt,
		nullUUID(p.SuspendedBy), p.SuspendedAt)
	return mapErr(err)
}

func (r partnerRepo) GetPartner(ctx context.Context, id uuid.UUID) (partners.Partner, error) {
	return scanPartner(r.db.QueryRow(ctx, `SELECT `+partnerCols+` FROM partners WHERE id=$1`, id))
}

func (r partnerRepo) ListPartners(ctx context.Context, limit int) ([]partners.Partner, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `SELECT `+partnerCols+` FROM partners ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []partners.Partner
	for rows.Next() {
		p, err := scanPartner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r partnerRepo) UpdatePartner(ctx context.Context, p partners.Partner) error {
	createdBy, err := uuid.Parse(p.CreatedBy)
	if err != nil {
		return store.ErrNotFound
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE partners SET
			organisation=$2, status=$3, environment=$4, contact_ref=$5,
			daily_value_cap_minor=$6, rate_limit_per_minute=$7, created_by=$8, updated_at=$9,
			proposed_activation_by=$10, proposed_activation_at=$11, activated_by=$12, activated_at=$13,
			suspended_by=$14, suspended_at=$15
		WHERE id=$1`,
		p.ID, p.Organisation, p.Status, p.Environment, p.ContactRef,
		p.DailyValueCapMinor, p.RateLimitPerMinute, createdBy, p.UpdatedAt.UTC(),
		nullUUID(p.ProposedActivationBy), p.ProposedActivationAt, nullUUID(p.ActivatedBy), p.ActivatedAt,
		nullUUID(p.SuspendedBy), p.SuspendedAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanKey(row interface{ Scan(dest ...any) error }) (partners.APIKey, error) {
	var k partners.APIKey
	var createdBy uuid.UUID
	var revokedBy *uuid.UUID
	err := row.Scan(
		&k.ID, &k.PartnerID, &k.Prefix, &k.SecretHash, &k.Environment, &k.Scopes,
		&createdBy, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &revokedBy,
		&k.OverlapUntil, &k.RotatedFrom,
	)
	if err != nil {
		return partners.APIKey{}, mapErr(err)
	}
	k.CreatedBy = createdBy.String()
	k.RevokedBy = uuidString(revokedBy)
	k.CreatedAt = k.CreatedAt.UTC()
	if k.LastUsedAt != nil {
		t := k.LastUsedAt.UTC()
		k.LastUsedAt = &t
	}
	if k.ExpiresAt != nil {
		t := k.ExpiresAt.UTC()
		k.ExpiresAt = &t
	}
	if k.RevokedAt != nil {
		t := k.RevokedAt.UTC()
		k.RevokedAt = &t
	}
	if k.OverlapUntil != nil {
		t := k.OverlapUntil.UTC()
		k.OverlapUntil = &t
	}
	return k, nil
}

const keyCols = `id, partner_id, prefix, secret_hash, environment, scopes,
	created_by, created_at, last_used_at, expires_at, revoked_at, revoked_by,
	overlap_until, rotated_from`

func (r partnerRepo) InsertKey(ctx context.Context, k partners.APIKey) error {
	if k.ID == uuid.Nil {
		k.ID = uuid.New()
	}
	createdBy, err := uuid.Parse(k.CreatedBy)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO partner_api_keys (
			id, partner_id, prefix, secret_hash, environment, scopes,
			created_by, created_at, last_used_at, expires_at, revoked_at, revoked_by,
			overlap_until, rotated_from
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		k.ID, k.PartnerID, k.Prefix, k.SecretHash, k.Environment, k.Scopes,
		createdBy, k.CreatedAt.UTC(), k.LastUsedAt, k.ExpiresAt, k.RevokedAt, nullUUID(k.RevokedBy),
		k.OverlapUntil, k.RotatedFrom)
	return mapErr(err)
}

func (r partnerRepo) GetKey(ctx context.Context, id uuid.UUID) (partners.APIKey, error) {
	return scanKey(r.db.QueryRow(ctx, `SELECT `+keyCols+` FROM partner_api_keys WHERE id=$1`, id))
}

func (r partnerRepo) GetKeyByPrefix(ctx context.Context, prefix string) (partners.APIKey, error) {
	return scanKey(r.db.QueryRow(ctx, `SELECT `+keyCols+` FROM partner_api_keys WHERE prefix=$1`, prefix))
}

func (r partnerRepo) ListKeys(ctx context.Context, partnerID uuid.UUID) ([]partners.APIKey, error) {
	rows, err := r.db.Query(ctx, `SELECT `+keyCols+` FROM partner_api_keys WHERE partner_id=$1 ORDER BY created_at DESC`, partnerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []partners.APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (r partnerRepo) UpdateKey(ctx context.Context, k partners.APIKey) error {
	createdBy, err := uuid.Parse(k.CreatedBy)
	if err != nil {
		return store.ErrNotFound
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE partner_api_keys SET
			prefix=$2, secret_hash=$3, environment=$4, scopes=$5,
			created_by=$6, last_used_at=$7, expires_at=$8, revoked_at=$9, revoked_by=$10,
			overlap_until=$11, rotated_from=$12
		WHERE id=$1`,
		k.ID, k.Prefix, k.SecretHash, k.Environment, k.Scopes,
		createdBy, k.LastUsedAt, k.ExpiresAt, k.RevokedAt, nullUUID(k.RevokedBy),
		k.OverlapUntil, k.RotatedFrom)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r partnerRepo) TouchKeyUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE partner_api_keys SET last_used_at=$2 WHERE id=$1`, id, at.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r partnerRepo) IncrementUsage(ctx context.Context, partnerID uuid.UUID, day time.Time, requests, valueMinor int64) error {
	d := time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
	_, err := r.db.Exec(ctx, `
		INSERT INTO partner_usage (partner_id, day, request_count, value_minor)
		VALUES ($1, $2::date, $3, $4)
		ON CONFLICT (partner_id, day) DO UPDATE SET
			request_count = partner_usage.request_count + EXCLUDED.request_count,
			value_minor = partner_usage.value_minor + EXCLUDED.value_minor`,
		partnerID, d, requests, valueMinor)
	return mapErr(err)
}

func (r partnerRepo) ListUsage(ctx context.Context, partnerID uuid.UUID, from, to time.Time) ([]partners.Usage, error) {
	fromD := time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC)
	toD := time.Date(to.UTC().Year(), to.UTC().Month(), to.UTC().Day(), 0, 0, 0, 0, time.UTC)
	rows, err := r.db.Query(ctx, `
		SELECT partner_id, day, request_count, value_minor
		FROM partner_usage
		WHERE partner_id=$1 AND day >= $2::date AND day <= $3::date
		ORDER BY day ASC`, partnerID, fromD, toD)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []partners.Usage
	for rows.Next() {
		var u partners.Usage
		var day time.Time
		if err := rows.Scan(&u.PartnerID, &day, &u.RequestCount, &u.ValueMinor); err != nil {
			return nil, mapErr(err)
		}
		u.Day = time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, u)
	}
	return out, rows.Err()
}
