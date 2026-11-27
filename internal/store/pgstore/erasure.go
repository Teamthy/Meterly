package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
)

const erasureCols = `id, user_id::text, status, due_at, cancelled_at, started_at, finished_at, step, error, tombstone_id, created_at, updated_at`

func scanErasure(row interface{ Scan(dest ...any) error }) (privacy.ErasureRequest, error) {
	var j privacy.ErasureRequest
	var tomb *uuid.UUID
	err := row.Scan(&j.ID, &j.UserID, &j.Status, &j.DueAt, &j.CancelledAt, &j.StartedAt, &j.FinishedAt,
		&j.Step, &j.Error, &tomb, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return privacy.ErasureRequest{}, mapErr(err)
	}
	j.DueAt = j.DueAt.UTC()
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()
	if j.CancelledAt != nil {
		t := j.CancelledAt.UTC()
		j.CancelledAt = &t
	}
	if j.StartedAt != nil {
		t := j.StartedAt.UTC()
		j.StartedAt = &t
	}
	if j.FinishedAt != nil {
		t := j.FinishedAt.UTC()
		j.FinishedAt = &t
	}
	if tomb != nil {
		j.TombstoneID = tomb.String()
	}
	return j, nil
}

func (r privacyRepo) EnsureTombstone(ctx context.Context) (string, error) {
	var id string
	err := r.db.QueryRow(ctx, `
		INSERT INTO users (id, external_id, status, display_name)
		VALUES ($1, $2, 'ERASED', '')
		ON CONFLICT (id) DO UPDATE SET status = 'ERASED', external_id = EXCLUDED.external_id
		RETURNING id::text`, privacy.TombstoneUserID, privacy.TombstoneExternalID).Scan(&id)
	return id, mapErr(err)
}

func (r privacyRepo) CreateErasure(ctx context.Context, job privacy.ErasureRequest) error {
	uid, err := uuid.Parse(job.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
	}
	var tomb any
	if job.TombstoneID != "" {
		t, err := uuid.Parse(job.TombstoneID)
		if err != nil {
			return store.ErrNotFound
		}
		tomb = t
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO erasure_requests (
			id, user_id, status, due_at, cancelled_at, started_at, finished_at,
			step, error, tombstone_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		job.ID, uid, job.Status, job.DueAt.UTC(), job.CancelledAt, job.StartedAt, job.FinishedAt,
		job.Step, job.Error, tomb, job.CreatedAt.UTC(), job.UpdatedAt.UTC())
	return mapErr(err)
}

func (r privacyRepo) GetErasure(ctx context.Context, id uuid.UUID) (privacy.ErasureRequest, error) {
	return scanErasure(r.db.QueryRow(ctx, `SELECT `+erasureCols+` FROM erasure_requests WHERE id = $1`, id))
}

func (r privacyRepo) GetOpenErasureForUser(ctx context.Context, userID string) (privacy.ErasureRequest, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return privacy.ErasureRequest{}, store.ErrNotFound
	}
	return scanErasure(r.db.QueryRow(ctx, `
		SELECT `+erasureCols+` FROM erasure_requests
		WHERE user_id = $1 AND status IN ('COOLING_OFF','READY','RUNNING','BLOCKED','FAILED')
		ORDER BY created_at DESC LIMIT 1`, uid))
}

func (r privacyRepo) UpdateErasure(ctx context.Context, job privacy.ErasureRequest) error {
	uid, err := uuid.Parse(job.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	var tomb any
	if job.TombstoneID != "" {
		t, err := uuid.Parse(job.TombstoneID)
		if err != nil {
			return store.ErrNotFound
		}
		tomb = t
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE erasure_requests SET
			user_id = $2, status = $3, due_at = $4, cancelled_at = $5, started_at = $6,
			finished_at = $7, step = $8, error = $9, tombstone_id = $10, updated_at = $11
		WHERE id = $1`,
		job.ID, uid, job.Status, job.DueAt.UTC(), job.CancelledAt, job.StartedAt, job.FinishedAt,
		job.Step, job.Error, tomb, job.UpdatedAt.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r privacyRepo) ListDueErasures(ctx context.Context, now time.Time, limit int) ([]privacy.ErasureRequest, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+erasureCols+` FROM erasure_requests
		WHERE (status = 'COOLING_OFF' AND due_at <= $1)
		   OR status IN ('READY','RUNNING','BLOCKED','FAILED')
		ORDER BY due_at ASC
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []privacy.ErasureRequest
	for rows.Next() {
		j, err := scanErasure(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r privacyRepo) CountOpenDisputesForUser(ctx context.Context, userID string) (int, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, store.ErrNotFound
	}
	var n int
	err = r.db.QueryRow(ctx, `
		SELECT count(*) FROM disputes d
		JOIN electricity_transactions t ON t.id = d.transaction_id
		WHERE t.customer_id = $1 AND d.status IN ('OPEN','REMINDED')`, uid).Scan(&n)
	return n, mapErr(err)
}

func (r privacyRepo) CountUnresolvedTxnsForUser(ctx context.Context, userID string) (int, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, store.ErrNotFound
	}
	var n int
	err = r.db.QueryRow(ctx, `
		SELECT count(*) FROM electricity_transactions
		WHERE customer_id = $1 AND (
			coalesce(review_reason, '') <> ''
			OR vending_status = 'UNKNOWN'
			OR delivery_status IN ('UNKNOWN','FAILED')
			OR (payment_status = 'CONFIRMED' AND vending_status = 'FAILED')
		)`, uid).Scan(&n)
	return n, mapErr(err)
}

func (r privacyRepo) CountActivePotsForUser(ctx context.Context, userID string) (int, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, store.ErrNotFound
	}
	var n int
	err = r.db.QueryRow(ctx, `
		SELECT count(*) FROM pots p
		WHERE p.owner_user_id = $1 AND (
			p.status IN ('FUNDED','ALLOCATING','REFUNDING')
			OR (p.status = 'OPEN' AND EXISTS (
				SELECT 1 FROM pot_contributions c
				WHERE c.pot_id = p.id AND c.status IN ('PAID','REFUND_PENDING')
			))
		)`, uid).Scan(&n)
	return n, mapErr(err)
}

func (r privacyRepo) CountUnpaidObligationsForUser(ctx context.Context, userID string) (int, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, store.ErrNotFound
	}
	var n int
	err = r.db.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM payment_requests WHERE requester_user_id = $1 AND status = 'OPEN')
			+
			(SELECT count(*) FROM pot_contributions c
			 JOIN pots p ON p.id = c.pot_id
			 WHERE c.payer_user_id = $1
			   AND c.status IN ('PAID','REFUND_PENDING')
			   AND p.status NOT IN ('SETTLED','REFUNDED','CANCELLED'))`, uid).Scan(&n)
	return n, mapErr(err)
}

func (r privacyRepo) ApplyErasureStep(ctx context.Context, userID, tombstoneID, step string, now time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	tomb, err := uuid.Parse(tombstoneID)
	if err != nil {
		return store.ErrNotFound
	}
	now = now.UTC()
	switch step {
	case privacy.ErasureStepFinancial:
		return r.eraseFinancial(ctx, uid, tomb)
	case privacy.ErasureStepIdentity:
		return r.eraseIdentity(ctx, uid, now)
	case privacy.ErasureStepMemberships:
		return r.eraseMemberships(ctx, uid, tomb, now)
	case privacy.ErasureStepSupport:
		return r.eraseSupport(ctx, uid)
	case privacy.ErasureStepNotify:
		return r.eraseNotify(ctx, uid)
	case privacy.ErasureStepCards:
		return r.eraseCards(ctx, uid, now)
	case privacy.ErasureStepExports:
		_, err = r.db.Exec(ctx, `DELETE FROM data_exports WHERE user_id = $1`, uid)
		return mapErr(err)
	default:
		return store.ErrNotFound
	}
}

func (r privacyRepo) eraseFinancial(ctx context.Context, uid, tomb uuid.UUID) error {
	stmts := []string{
		`UPDATE quotes SET customer_id = $2 WHERE customer_id = $1`,
		`UPDATE electricity_transactions SET customer_id = $2, updated_at = now() WHERE customer_id = $1`,
		`UPDATE gift_transactions SET
			purchaser_user_id = $2,
			recipient_name = '',
			recipient_contact_encrypted = NULL,
			contact_key_version = NULL,
			notification_requested = false,
			recipient_user_id = CASE WHEN recipient_user_id = $1 THEN NULL ELSE recipient_user_id END
		 WHERE purchaser_user_id = $1 OR recipient_user_id = $1`,
		`UPDATE token_shares SET
			shared_by_user_id = $2,
			recipient_reference_hash = NULL,
			destination_ciphertext = NULL,
			destination_key_version = '',
			destination_masked = ''
		 WHERE shared_by_user_id = $1`,
		`UPDATE pots SET owner_user_id = $2, note_ciphertext = NULL, note_key_version = '' WHERE owner_user_id = $1`,
		`UPDATE pot_contributions SET
			payer_user_id = $2,
			email_ciphertext = '\x'::bytea,
			name_ciphertext = NULL,
			phone_ciphertext = NULL,
			initials = ''
		 WHERE payer_user_id = $1`,
		`UPDATE payment_requests SET requester_user_id = $2, note_ciphertext = NULL, note_key_version = '' WHERE requester_user_id = $1`,
	}
	for _, q := range stmts {
		if _, err := r.db.Exec(ctx, q, uid, tomb); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (r privacyRepo) eraseIdentity(ctx context.Context, uid uuid.UUID, now time.Time) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM webauthn_credentials WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM auth_challenges WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM security_events WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `UPDATE offline_purchases SET phone_hmac = '' WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `UPDATE offline_token_resends SET phone_hmac = '' WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `UPDATE risk_observations SET email_hmac = '', ip_hmac = '', phone_hmac = '' WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `UPDATE risk_decisions SET email_hmac = '', ip_hmac = '' WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	_, err := r.db.Exec(ctx, `
		UPDATE users SET
			display_name = '',
			email_ciphertext = NULL,
			email_hmac = NULL,
			email_masked = '',
			email_key_version = '',
			phone_ciphertext = NULL,
			phone_hmac = NULL,
			phone_masked = '',
			phone_key_version = '',
			first_name = NULL,
			last_name = NULL,
			status = 'ERASED',
			external_id = 'erased:' || id::text,
			updated_at = $2
		WHERE id = $1`, uid, now)
	return mapErr(err)
}

func (r privacyRepo) eraseMemberships(ctx context.Context, uid, tomb uuid.UUID, now time.Time) error {
	if _, err := r.db.Exec(ctx, `
		UPDATE meters m SET customer_name = '', address = ''
		WHERE EXISTS (
			SELECT 1 FROM meter_access_grants g
			WHERE g.meter_id = m.id AND g.user_id = $1 AND g.revoked_at IS NULL
		) AND NOT EXISTS (
			SELECT 1 FROM meter_access_grants g2
			WHERE g2.meter_id = m.id AND g2.user_id <> $1 AND g2.revoked_at IS NULL
		)`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE meter_access_grants SET revoked_at = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, uid, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE household_members SET status = 'REVOKED', revoked_at = $2, updated_at = $2
		WHERE user_id = $1 AND status IN ('ACTIVE','INVITED')`, uid, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE households SET owner_user_id = $2, name = '', updated_at = $3
		WHERE owner_user_id = $1`, uid, tomb, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE meter_ownership SET revoked_at = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, uid, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM beneficiaries WHERE owner_user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE autopower_runs SET status = 'CANCELLED', updated_at = $2
		WHERE user_id = $1 AND status IN ('SCHEDULED','CHARGING')`, uid, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `
		UPDATE autopower_rules SET status = 'CANCELLED', updated_at = $2
		WHERE user_id = $1 AND status NOT IN ('CANCELLED','COMPLETED')`, uid, now); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM organisation_members WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM phone_channel_links WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM channel_otps WHERE target_user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	_, err := r.db.Exec(ctx, `DELETE FROM conversation_sessions WHERE user_id = $1`, uid)
	return mapErr(err)
}

func (r privacyRepo) eraseSupport(ctx context.Context, uid uuid.UUID) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM support_identity_keys WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	_, err := r.db.Exec(ctx, `DELETE FROM support_threads WHERE customer_id = $1`, uid)
	return mapErr(err)
}

func (r privacyRepo) eraseNotify(ctx context.Context, uid uuid.UUID) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM notification_preferences WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM consents WHERE user_id = $1`, uid); err != nil {
		return mapErr(err)
	}
	_, err := r.db.Exec(ctx, `DELETE FROM legal_holds WHERE user_id = $1`, uid)
	return mapErr(err)
}

func (r privacyRepo) eraseCards(ctx context.Context, uid uuid.UUID, now time.Time) error {
	// TODO: revoke Paystack authorizations when that API is documented; until
	// then we only soft-delete the local saved method and wipe ciphertext.
	_, err := r.db.Exec(ctx, `
		UPDATE saved_payment_methods
		SET deleted_at = $2,
		    auth_code_ciphertext = '\x'::bytea,
		    email_ciphertext = '\x'::bytea
		WHERE user_id = $1 AND deleted_at IS NULL`, uid, now)
	return mapErr(err)
}
