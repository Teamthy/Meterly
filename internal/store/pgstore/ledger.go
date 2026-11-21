package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
)

// ---- ledger --------------------------------------------------------------------

type ledgerRepo struct{ *repos }

func (r ledgerRepo) Post(ctx context.Context, t ledger.Transaction) (ledger.Transaction, error) {
	if err := ledger.Validate(t.Entries); err != nil {
		return ledger.Transaction{}, err
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.PostedAt.IsZero() {
		t.PostedAt = time.Now().UTC()
	}
	var out ledger.Transaction
	err := r.atomic(ctx, func(db DB) error {
		tag, err := db.Exec(ctx, `INSERT INTO ledger_transactions (id, reference, source_type, source_id, description, posted_at)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (reference) DO NOTHING`,
			t.ID, t.Reference, t.SourceType, t.SourceID, t.Description, t.PostedAt)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() == 0 { // already posted: idempotent no-op
			existing, err := r.byWhere(ctx, db, `reference = $1`, t.Reference)
			if err != nil {
				return err
			}
			if len(existing) != 1 {
				return fmt.Errorf("ledger: reference %s vanished", t.Reference)
			}
			out = existing[0]
			return nil
		}
		for _, e := range t.Entries {
			tag, err := db.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, direction, amount_minor, currency)
				SELECT $1, id, $3, $4, $5 FROM ledger_accounts WHERE account_code = $2`,
				t.ID, e.Account, e.Direction, e.Amount.Minor, string(e.Amount.Currency))
			if err != nil {
				return mapErr(err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("ledger: unknown account %s", e.Account)
			}
		}
		out = t
		return nil
	})
	return out, err
}

func (r ledgerRepo) byWhere(ctx context.Context, db DB, where string, arg any) ([]ledger.Transaction, error) {
	rows, err := db.Query(ctx, `
		SELECT lt.id, lt.reference, lt.source_type, lt.source_id, coalesce(lt.description, ''), lt.posted_at,
		       a.account_code, e.direction, e.amount_minor, e.currency
		FROM ledger_transactions lt
		JOIN ledger_entries e  ON e.ledger_transaction_id = lt.id
		JOIN ledger_accounts a ON a.id = e.ledger_account_id
		WHERE lt.`+where+`
		ORDER BY lt.posted_at, lt.id, e.id`, arg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []ledger.Transaction
	idx := map[uuid.UUID]int{}
	for rows.Next() {
		var lt ledger.Transaction
		var e ledger.Entry
		var cur string
		if err := rows.Scan(&lt.ID, &lt.Reference, &lt.SourceType, &lt.SourceID, &lt.Description, &lt.PostedAt,
			&e.Account, &e.Direction, &e.Amount.Minor, &cur); err != nil {
			return nil, err
		}
		e.Amount.Currency = money.Currency(cur)
		i, ok := idx[lt.ID]
		if !ok {
			lt.PostedAt = lt.PostedAt.UTC()
			out = append(out, lt)
			i = len(out) - 1
			idx[lt.ID] = i
		}
		out[i].Entries = append(out[i].Entries, e)
	}
	return out, rows.Err()
}

func (r ledgerRepo) BySource(ctx context.Context, id uuid.UUID) ([]ledger.Transaction, error) {
	return r.byWhere(ctx, r.db, `source_id = $1`, id)
}

func (r ledgerRepo) Balance(ctx context.Context, acct ledger.AccountCode, c money.Currency) (int64, error) {
	var v int64
	err := r.db.QueryRow(ctx, `
		SELECT coalesce(sum(CASE e.direction WHEN 'DEBIT' THEN e.amount_minor ELSE -e.amount_minor END), 0)
		FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.ledger_account_id
		WHERE a.account_code = $1 AND e.currency = $2`, acct, string(c)).Scan(&v)
	return v, mapErr(err)
}

func (r ledgerRepo) TrialBalance(ctx context.Context) (int64, error) {
	var v int64
	err := r.db.QueryRow(ctx, `SELECT coalesce(sum(CASE direction WHEN 'DEBIT' THEN amount_minor ELSE -amount_minor END), 0)
		FROM ledger_entries`).Scan(&v)
	return v, mapErr(err)
}

// ---- outbox --------------------------------------------------------------------

type outboxRepo struct{ *repos }

func (r outboxRepo) Add(ctx context.Context, e events.Event) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, e.ID, e.AggregateType, e.AggregateID, e.Type, payload, e.CreatedAt)
	return mapErr(err)
}

func (r outboxRepo) query(ctx context.Context, sql string, args ...any) ([]events.Event, error) {
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		var e events.Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.Type, &payload, &e.CreatedAt, &e.PublishedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const outboxCols = `id, aggregate_type, aggregate_id, event_type, payload, created_at, published_at`

func (r outboxRepo) ClaimUnpublished(ctx context.Context, limit int) ([]events.Event, error) {
	return r.query(ctx, `SELECT `+outboxCols+` FROM outbox_events WHERE published_at IS NULL
		ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
}

func (r outboxRepo) MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE outbox_events SET published_at = $2 WHERE id = $1 AND published_at IS NULL`, id, at)
	return mapErr(err)
}

func (r outboxRepo) ForAggregate(ctx context.Context, id uuid.UUID) ([]events.Event, error) {
	return r.query(ctx, `SELECT `+outboxCols+` FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at`, id)
}

// ---- idempotency ---------------------------------------------------------------

type idemRepo struct{ *repos }

func (r idemRepo) Reserve(ctx context.Context, rec idempotency.Record) (*idempotency.Record, error) {
	var existing *idempotency.Record
	err := r.atomic(ctx, func(db DB) error {
		if _, err := db.Exec(ctx, `DELETE FROM idempotency_keys
			WHERE scope = $1 AND operation = $2 AND key = $3 AND expires_at <= now()`, rec.Scope, rec.Operation, rec.Key); err != nil {
			return mapErr(err)
		}
		// A concurrent reservation of the same key blocks here until the
		// other transaction ends; then we either insert or see its row.
		tag, err := db.Exec(ctx, `INSERT INTO idempotency_keys (scope, operation, key, request_hash, result_ref, created_at, expires_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7) ON CONFLICT DO NOTHING`,
			rec.Scope, rec.Operation, rec.Key, rec.RequestHash, rec.ResultRef, rec.CreatedAt, rec.ExpiresAt)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var e idempotency.Record
		if err := db.QueryRow(ctx, `SELECT scope, operation, key, request_hash, coalesce(result_ref, ''), created_at, expires_at
			FROM idempotency_keys WHERE scope = $1 AND operation = $2 AND key = $3`, rec.Scope, rec.Operation, rec.Key).
			Scan(&e.Scope, &e.Operation, &e.Key, &e.RequestHash, &e.ResultRef, &e.CreatedAt, &e.ExpiresAt); err != nil {
			return mapErr(err)
		}
		existing = &e
		return nil
	})
	return existing, err
}

func (r idemRepo) Complete(ctx context.Context, scope, op, key, resultRef string) error {
	_, err := r.db.Exec(ctx, `UPDATE idempotency_keys SET result_ref = $4 WHERE scope = $1 AND operation = $2 AND key = $3`,
		scope, op, key, resultRef)
	return mapErr(err)
}
