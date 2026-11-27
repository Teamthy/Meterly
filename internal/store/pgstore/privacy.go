package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
)

type privacyRepo struct{ *repos }

func heldUUIDs(held map[string]bool) []uuid.UUID {
	var out []uuid.UUID
	for id := range held {
		u, err := uuid.Parse(id)
		if err == nil {
			out = append(out, u)
		}
	}
	if out == nil {
		out = []uuid.UUID{}
	}
	return out
}

func (r privacyRepo) ListConsents(ctx context.Context, userID string) ([]privacy.Record, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id::text, purpose, channel, status, source, created_at
		FROM consents WHERE user_id = $1 ORDER BY created_at ASC, id ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []privacy.Record
	for rows.Next() {
		var rec privacy.Record
		var ch string
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.Purpose, &ch, &rec.Status, &rec.Source, &rec.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		rec.Channel = notifications.Channel(ch)
		rec.CreatedAt = rec.CreatedAt.UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r privacyRepo) AppendConsent(ctx context.Context, rec privacy.Record) error {
	uid, err := uuid.Parse(rec.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO consents (id, user_id, purpose, channel, status, source, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rec.ID, uid, rec.Purpose, string(rec.Channel), rec.Status, rec.Source, rec.CreatedAt.UTC())
	return mapErr(err)
}

func (r privacyRepo) AddLegalHold(ctx context.Context, h privacy.Hold) error {
	uid, err := uuid.Parse(h.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if h.ID == uuid.Nil {
		h.ID = uuid.New()
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO legal_holds (id, user_id, reason, created_at, created_by, released_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		h.ID, uid, h.Reason, h.CreatedAt.UTC(), h.CreatedBy, h.ReleasedAt)
	return mapErr(err)
}

func (r privacyRepo) ActiveHeldUserIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.Query(ctx, `SELECT user_id::text FROM legal_holds WHERE released_at IS NULL`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (r privacyRepo) RecordRetentionRun(ctx context.Context, run privacy.RetentionRun) error {
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	if run.Counts == nil {
		run.Counts = map[string]int{}
	}
	raw, err := json.Marshal(run.Counts)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(map[string]any{"dry_run": run.DryRun, "counts": run.Counts, "error": run.Error})
	if err != nil {
		return err
	}
	at := run.StartedAt.UTC()
	if run.FinishedAt != nil {
		at = run.FinishedAt.UTC()
	}
	return r.atomic(ctx, func(db DB) error {
		if _, err := db.Exec(ctx, `
			INSERT INTO retention_runs (id, started_at, finished_at, dry_run, status, counts, error)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			run.ID, run.StartedAt.UTC(), run.FinishedAt, run.DryRun, run.Status, raw, run.Error); err != nil {
			return mapErr(err)
		}
		_, err := db.Exec(ctx, `
			INSERT INTO audit_events (actor_type, action, resource_type, resource_id, metadata_redacted, created_at)
			VALUES ('SYSTEM', 'retention.purge', 'retention_run', $1, $2, $3)`,
			run.ID, meta, at)
		return mapErr(err)
	})
}

func (r privacyRepo) PurgeChannelOTPs(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	heldIDs := heldUUIDs(held)
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM channel_otps
				WHERE created_at < $1
				  AND (cardinality($2::uuid[]) = 0 OR NOT (target_user_id = ANY($2)))
				LIMIT $3
			) s`, cutoff.UTC(), heldIDs, batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM channel_otps
		WHERE id IN (
			SELECT id FROM channel_otps
			WHERE created_at < $1
			  AND (cardinality($2::uuid[]) = 0 OR NOT (target_user_id = ANY($2)))
			LIMIT $3
		)`, cutoff.UTC(), heldIDs, batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) PurgeAuthChallenges(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	heldIDs := heldUUIDs(held)
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM auth_challenges
				WHERE expires_at < $1
				  AND (cardinality($2::uuid[]) = 0 OR user_id IS NULL OR NOT (user_id = ANY($2)))
				LIMIT $3
			) s`, cutoff.UTC(), heldIDs, batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM auth_challenges
		WHERE id IN (
			SELECT id FROM auth_challenges
			WHERE expires_at < $1
			  AND (cardinality($2::uuid[]) = 0 OR user_id IS NULL OR NOT (user_id = ANY($2)))
			LIMIT $3
		)`, cutoff.UTC(), heldIDs, batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) PurgeAuthSessions(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	heldIDs := heldUUIDs(held)
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM auth_sessions
				WHERE COALESCE(revoked_at, expires_at) < $1
				  AND (cardinality($2::uuid[]) = 0 OR NOT (user_id = ANY($2)))
				LIMIT $3
			) s`, cutoff.UTC(), heldIDs, batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM auth_sessions
		WHERE id IN (
			SELECT id FROM auth_sessions
			WHERE COALESCE(revoked_at, expires_at) < $1
			  AND (cardinality($2::uuid[]) = 0 OR NOT (user_id = ANY($2)))
			LIMIT $3
		)`, cutoff.UTC(), heldIDs, batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) PurgeIdempotencyKeys(ctx context.Context, cutoff time.Time, batch int, dryRun bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT 1 FROM idempotency_keys
				WHERE result_ref IS NOT NULL AND result_ref <> '' AND created_at < $1
				LIMIT $2
			) s`, cutoff.UTC(), batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE ctid IN (
			SELECT ctid FROM idempotency_keys
			WHERE result_ref IS NOT NULL AND result_ref <> '' AND created_at < $1
			LIMIT $2
		)`, cutoff.UTC(), batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) PurgeRiskObservations(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	heldIDs := heldUUIDs(held)
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM risk_observations
				WHERE observed_at < $1
				  AND (cardinality($2::uuid[]) = 0 OR user_id IS NULL OR NOT (user_id = ANY($2)))
				LIMIT $3
			) s`, cutoff.UTC(), heldIDs, batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM risk_observations
		WHERE id IN (
			SELECT id FROM risk_observations
			WHERE observed_at < $1
			  AND (cardinality($2::uuid[]) = 0 OR user_id IS NULL OR NOT (user_id = ANY($2)))
			LIMIT $3
		)`, cutoff.UTC(), heldIDs, batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) PurgeClosedSupportThreads(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	heldIDs := heldUUIDs(held)
	if dryRun {
		var n int
		err := r.db.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT id FROM support_threads
				WHERE status IN ('CLOSED', 'RESOLVED') AND updated_at < $1
				  AND (cardinality($2::uuid[]) = 0 OR NOT (customer_id = ANY($2)))
				LIMIT $3
			) s`, cutoff.UTC(), heldIDs, batch).Scan(&n)
		return n, mapErr(err)
	}
	tag, err := r.db.Exec(ctx, `
		DELETE FROM support_threads
		WHERE id IN (
			SELECT id FROM support_threads
			WHERE status IN ('CLOSED', 'RESOLVED') AND updated_at < $1
			  AND (cardinality($2::uuid[]) = 0 OR NOT (customer_id = ANY($2)))
			LIMIT $3
		)`, cutoff.UTC(), heldIDs, batch)
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r privacyRepo) CountPurgeableDeliveryHMACs(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `
		SELECT count(*) FROM notification_deliveries
		WHERE destination_hmac <> ''
		  AND status IN ('SENT', 'FAILED', 'SUPPRESSED')
		  AND created_at < $1`, cutoff.UTC()).Scan(&n)
	return n, mapErr(err)
}

func (r privacyRepo) PurgeDeliveryHMACs(ctx context.Context, cutoff time.Time) (int, error) {
	return notifRepo{r.repos}.PurgeOldDestinationHMACs(ctx, cutoff)
}
