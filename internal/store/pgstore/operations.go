package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// ---- audit ---------------------------------------------------------------------

type auditRepo struct{ *repos }

func (r auditRepo) Add(ctx context.Context, e audit.Event) error {
	meta, err := json.Marshal(e.Metadata)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO audit_events (actor_user_id, actor_type, action, resource_type, resource_id, correlation_id, metadata_redacted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		nullUUID(e.ActorUserID), e.ActorType, e.Action, e.ResourceType, e.ResourceID, e.CorrelationID, meta, e.CreatedAt)
	return mapErr(err)
}

func (r auditRepo) ForResource(ctx context.Context, typ string, id uuid.UUID) ([]audit.Event, error) {
	rows, err := r.db.Query(ctx, `SELECT id, coalesce(actor_user_id::text, ''), actor_type, action, resource_type, resource_id,
		correlation_id, metadata_redacted, created_at FROM audit_events WHERE resource_type = $1 AND resource_id = $2 ORDER BY id`, typ, id)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []audit.Event
	for rows.Next() {
		var e audit.Event
		var meta []byte
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorType, &e.Action, &e.ResourceType, &e.ResourceID,
			&e.CorrelationID, &meta, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(meta, &e.Metadata); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r auditRepo) Recent(ctx context.Context, limit int) ([]audit.Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT id, coalesce(actor_user_id::text, ''), actor_type, action, resource_type, resource_id,
		correlation_id, metadata_redacted, created_at FROM audit_events ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]audit.Event, 0)
	for rows.Next() {
		var e audit.Event
		var meta []byte
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorType, &e.Action, &e.ResourceType, &e.ResourceID,
			&e.CorrelationID, &meta, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(meta, &e.Metadata); err != nil {
			return nil, err
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- providers -----------------------------------------------------------------

type providerRepo struct{ *repos }

func (r providerRepo) Enabled(ctx context.Context, name string) (bool, error) {
	var status string
	err := r.db.QueryRow(ctx, `SELECT status FROM providers WHERE name = $1 AND environment = $2`, name, r.s.environment).Scan(&status)
	if mapErr(err) == store.ErrNotFound {
		return true, nil
	}
	if err != nil {
		return false, mapErr(err)
	}
	return status != "DISABLED", nil
}

func (r providerRepo) SetEnabled(ctx context.Context, name, providerType string, enabled bool) error {
	status := "ACTIVE"
	if !enabled {
		status = "DISABLED"
	}
	_, err := r.db.Exec(ctx, `INSERT INTO providers (name, provider_type, environment, status) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name, environment) DO UPDATE SET status = EXCLUDED.status`, name, providerType, r.s.environment, status)
	return mapErr(err)
}

// ---- search --------------------------------------------------------------------

func txnWhere(f store.TxnFilter) (clause string, args []any) {
	var where []string
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.ReviewOnly {
		where = append(where, "review_reason <> ''")
	}
	if f.Payment != "" {
		add("payment_status = $%d", f.Payment)
	}
	if f.Vending != "" {
		add("vending_status = $%d", f.Vending)
	}
	if f.Delivery != "" {
		add("delivery_status = $%d", f.Delivery)
	}
	if f.CustomerID != "" {
		add("customer_id = $%d", f.CustomerID)
	}
	if f.MeterID != uuid.Nil {
		add("meter_id = $%d", f.MeterID)
	}
	if !f.Before.IsZero() {
		add("created_at < $%d", f.Before)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

func (r txnRepo) Search(ctx context.Context, f store.TxnFilter) ([]*tx.Transaction, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	clause, args := txnWhere(f)
	args = append(args, limit)
	sql := "SELECT id FROM electricity_transactions" + clause + fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make([]*tx.Transaction, 0, len(ids))
	for _, id := range ids {
		t, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (r txnRepo) Count(ctx context.Context, f store.TxnFilter) (int, error) {
	clause, args := txnWhere(f)
	var n int
	err := r.db.QueryRow(ctx, "SELECT count(*) FROM electricity_transactions"+clause, args...).Scan(&n)
	return n, mapErr(err)
}
