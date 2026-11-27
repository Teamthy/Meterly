package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

func utcDay(day time.Time) time.Time {
	return time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
}

func (r partnerRepo) GetUsage(ctx context.Context, partnerID uuid.UUID, day time.Time) (partners.Usage, error) {
	d := utcDay(day)
	var u partners.Usage
	var got time.Time
	err := r.db.QueryRow(ctx, `
		SELECT partner_id, day, request_count, value_minor
		FROM partner_usage WHERE partner_id=$1 AND day=$2::date`, partnerID, d).
		Scan(&u.PartnerID, &got, &u.RequestCount, &u.ValueMinor)
	if err != nil {
		if errors.Is(mapErr(err), store.ErrNotFound) {
			return partners.Usage{PartnerID: partnerID, Day: d}, nil
		}
		return partners.Usage{}, mapErr(err)
	}
	u.Day = utcDay(got)
	return u, nil
}

func scanFloat(row interface{ Scan(dest ...any) error }) (partners.Float, error) {
	var f partners.Float
	err := row.Scan(&f.PartnerID, &f.AvailableMinor, &f.Currency, &f.Version, &f.UpdatedAt)
	if err != nil {
		return partners.Float{}, mapErr(err)
	}
	f.UpdatedAt = f.UpdatedAt.UTC()
	return f, nil
}

func (r partnerRepo) GetFloat(ctx context.Context, partnerID uuid.UUID) (partners.Float, error) {
	f, err := scanFloat(r.db.QueryRow(ctx, `
		SELECT partner_id, available_minor, currency, version, updated_at
		FROM partner_float WHERE partner_id=$1`, partnerID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return partners.Float{PartnerID: partnerID, Currency: "NGN"}, nil
		}
		return partners.Float{}, err
	}
	return f, nil
}

func (r partnerRepo) GetFloatForUpdate(ctx context.Context, partnerID uuid.UUID) (partners.Float, error) {
	return scanFloat(r.db.QueryRow(ctx, `
		SELECT partner_id, available_minor, currency, version, updated_at
		FROM partner_float WHERE partner_id=$1 FOR UPDATE`, partnerID))
}

func (r partnerRepo) EnsureFloatForUpdate(ctx context.Context, partnerID uuid.UUID, now time.Time) (partners.Float, error) {
	_, err := r.db.Exec(ctx, `
		INSERT INTO partner_float (partner_id, available_minor, currency, version, updated_at)
		VALUES ($1, 0, 'NGN', 1, $2)
		ON CONFLICT (partner_id) DO NOTHING`, partnerID, now.UTC())
	if err != nil {
		return partners.Float{}, mapErr(err)
	}
	return r.GetFloatForUpdate(ctx, partnerID)
}

func (r partnerRepo) UpdateFloat(ctx context.Context, f partners.Float) error {
	if f.AvailableMinor < 0 {
		return partners.ErrInvalidInput
	}
	cur := f.Currency
	if cur == "" {
		cur = "NGN"
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE partner_float
		SET available_minor=$2, currency=$3, version=version+1, updated_at=$4
		WHERE partner_id=$1 AND version=$5`,
		f.PartnerID, f.AvailableMinor, cur, f.UpdatedAt.UTC(), f.Version)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return partners.ErrFloatConflict
	}
	return nil
}

const topupCols = `id, partner_id, receipt_ref, amount_minor, currency, status,
	proposed_by, proposed_at, approved_by, approved_at, rejected_by, rejected_at,
	reason, ledger_reference`

func scanTopup(row interface{ Scan(dest ...any) error }) (partners.FloatTopup, error) {
	var t partners.FloatTopup
	var proposedBy uuid.UUID
	var approvedBy, rejectedBy *uuid.UUID
	err := row.Scan(
		&t.ID, &t.PartnerID, &t.ReceiptRef, &t.AmountMinor, &t.Currency, &t.Status,
		&proposedBy, &t.ProposedAt, &approvedBy, &t.ApprovedAt, &rejectedBy, &t.RejectedAt,
		&t.Reason, &t.LedgerReference,
	)
	if err != nil {
		return partners.FloatTopup{}, mapErr(err)
	}
	t.ProposedBy = proposedBy.String()
	t.ApprovedBy = uuidString(approvedBy)
	t.RejectedBy = uuidString(rejectedBy)
	t.ProposedAt = t.ProposedAt.UTC()
	if t.ApprovedAt != nil {
		at := t.ApprovedAt.UTC()
		t.ApprovedAt = &at
	}
	if t.RejectedAt != nil {
		at := t.RejectedAt.UTC()
		t.RejectedAt = &at
	}
	return t, nil
}

func (r partnerRepo) CreateTopup(ctx context.Context, t partners.FloatTopup) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	proposedBy, err := uuid.Parse(t.ProposedBy)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO partner_float_topups (
			id, partner_id, receipt_ref, amount_minor, currency, status,
			proposed_by, proposed_at, approved_by, approved_at, rejected_by, rejected_at,
			reason, ledger_reference
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		t.ID, t.PartnerID, t.ReceiptRef, t.AmountMinor, t.Currency, t.Status,
		proposedBy, t.ProposedAt.UTC(), nullUUID(t.ApprovedBy), t.ApprovedAt,
		nullUUID(t.RejectedBy), t.RejectedAt, t.Reason, t.LedgerReference)
	return mapErr(err)
}

func (r partnerRepo) GetTopup(ctx context.Context, id uuid.UUID) (partners.FloatTopup, error) {
	return scanTopup(r.db.QueryRow(ctx, `SELECT `+topupCols+` FROM partner_float_topups WHERE id=$1`, id))
}

func (r partnerRepo) GetTopupForUpdate(ctx context.Context, id uuid.UUID) (partners.FloatTopup, error) {
	return scanTopup(r.db.QueryRow(ctx, `SELECT `+topupCols+` FROM partner_float_topups WHERE id=$1 FOR UPDATE`, id))
}

func (r partnerRepo) GetTopupByReceipt(ctx context.Context, receiptRef string) (partners.FloatTopup, error) {
	return scanTopup(r.db.QueryRow(ctx, `SELECT `+topupCols+` FROM partner_float_topups WHERE receipt_ref=$1`, receiptRef))
}

func (r partnerRepo) UpdateTopup(ctx context.Context, t partners.FloatTopup) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE partner_float_topups SET
			status=$2, approved_by=$3, approved_at=$4, rejected_by=$5, rejected_at=$6,
			reason=$7, ledger_reference=$8
		WHERE id=$1`,
		t.ID, t.Status, nullUUID(t.ApprovedBy), t.ApprovedAt, nullUUID(t.RejectedBy), t.RejectedAt,
		t.Reason, t.LedgerReference)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
