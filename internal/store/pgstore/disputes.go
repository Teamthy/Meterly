package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/store"
)

type disputeRepo struct{ *repos }

func scanDispute(row scanner) (disputes.Dispute, error) {
	var d disputes.Dispute
	var txnID *uuid.UUID
	var due, reminded, resolved *time.Time
	var evidence []byte
	var minor int64
	var currency string
	err := row.Scan(&d.ID, &d.PSPDisputeID, &txnID, &d.PaymentRef, &minor, &currency, &d.Status,
		&d.PSPStatus, &d.Resolution, &d.Category, &due, &reminded, &resolved, &evidence, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if mapErr(err) == store.ErrNotFound {
			return disputes.Dispute{}, disputes.ErrNotFound
		}
		return disputes.Dispute{}, mapErr(err)
	}
	d.TransactionID = scanUUID(txnID)
	d.Amount = money.New(minor, money.Currency(currency))
	if due != nil {
		d.DueAt = due.UTC()
	}
	if reminded != nil {
		d.RemindedAt = reminded.UTC()
	}
	if resolved != nil {
		d.ResolvedAt = resolved.UTC()
	}
	d.Evidence = append(json.RawMessage(nil), evidence...)
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	return d, nil
}

const disputeCols = `id, psp_dispute_id, transaction_id, payment_ref, amount_minor, currency, status,
	psp_status, resolution, category, due_at, reminded_at, resolved_at, evidence, created_at, updated_at`

func (r disputeRepo) Upsert(ctx context.Context, d disputes.Dispute) (disputes.Dispute, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.Status == "" {
		d.Status = disputes.StatusOpen
	}
	cur := d.Amount.Currency
	if cur == "" {
		cur = money.NGN
	}
	var due, reminded, resolved any
	if !d.DueAt.IsZero() {
		due = d.DueAt.UTC()
	}
	if !d.RemindedAt.IsZero() {
		reminded = d.RemindedAt.UTC()
	}
	if !d.ResolvedAt.IsZero() {
		resolved = d.ResolvedAt.UTC()
	}
	ev := d.Evidence
	if len(ev) == 0 {
		ev = json.RawMessage(`{}`)
	}
	return scanDispute(r.db.QueryRow(ctx, `
		INSERT INTO disputes (`+disputeCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now(),now())
		ON CONFLICT (psp_dispute_id) DO UPDATE SET
			transaction_id = COALESCE(EXCLUDED.transaction_id, disputes.transaction_id),
			payment_ref = CASE WHEN EXCLUDED.payment_ref <> '' THEN EXCLUDED.payment_ref ELSE disputes.payment_ref END,
			amount_minor = CASE WHEN EXCLUDED.amount_minor > 0 THEN EXCLUDED.amount_minor ELSE disputes.amount_minor END,
			status = EXCLUDED.status,
			psp_status = CASE WHEN EXCLUDED.psp_status <> '' THEN EXCLUDED.psp_status ELSE disputes.psp_status END,
			resolution = CASE WHEN EXCLUDED.resolution <> '' THEN EXCLUDED.resolution ELSE disputes.resolution END,
			category = CASE WHEN EXCLUDED.category <> '' THEN EXCLUDED.category ELSE disputes.category END,
			due_at = COALESCE(EXCLUDED.due_at, disputes.due_at),
			reminded_at = COALESCE(EXCLUDED.reminded_at, disputes.reminded_at),
			resolved_at = COALESCE(EXCLUDED.resolved_at, disputes.resolved_at),
			evidence = CASE WHEN EXCLUDED.evidence <> '{}'::jsonb THEN EXCLUDED.evidence ELSE disputes.evidence END,
			updated_at = now()
		RETURNING `+disputeCols,
		d.ID, d.PSPDisputeID, optUUID(d.TransactionID), d.PaymentRef, d.Amount.Minor, string(cur), string(d.Status),
		d.PSPStatus, d.Resolution, d.Category, due, reminded, resolved, ev))
}

func (r disputeRepo) Get(ctx context.Context, id uuid.UUID) (disputes.Dispute, error) {
	return scanDispute(r.db.QueryRow(ctx, `SELECT `+disputeCols+` FROM disputes WHERE id=$1`, id))
}

func (r disputeRepo) ByPSP(ctx context.Context, pspID string) (disputes.Dispute, error) {
	return scanDispute(r.db.QueryRow(ctx, `SELECT `+disputeCols+` FROM disputes WHERE psp_dispute_id=$1`, pspID))
}

func (r disputeRepo) List(ctx context.Context, f disputes.Filter) ([]disputes.Dispute, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + disputeCols + ` FROM disputes`
	var args []any
	if f.Status != "" {
		q += ` WHERE status=$1`
		args = append(args, string(f.Status))
		q += ` ORDER BY created_at DESC LIMIT $2`
		args = append(args, limit)
	} else {
		q += ` ORDER BY created_at DESC LIMIT $1`
		args = append(args, limit)
	}
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []disputes.Dispute
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r disputeRepo) CountLostForUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM disputes d
		JOIN electricity_transactions t ON t.id = d.transaction_id
		WHERE t.customer_id = $1 AND d.status IN ('LOST', 'ACCEPTED')`, optUUIDString(userID)).Scan(&n)
	return n, mapErr(err)
}

func (r disputeRepo) SetEvidence(ctx context.Context, id uuid.UUID, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	tag, err := r.db.Exec(ctx, `UPDATE disputes SET evidence=$2::jsonb, updated_at=now() WHERE id=$1`, id, raw)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return disputes.ErrNotFound
	}
	return nil
}
