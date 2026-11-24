package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/store"
)

type potRepo struct{ *repos }

func (r potRepo) Create(ctx context.Context, p *pots.Pot) error {
	ownerUID, err := uuid.Parse(p.OwnerID)
	if err != nil {
		return store.ErrNotFound
	}
	var meterParam any
	if p.MeterID != uuid.Nil {
		meterParam = p.MeterID
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO pots (
			id, kind, owner_user_id, meter_id,
			target_minor, fee_minor, currency, suggested_shares,
			note_ciphertext, note_key_version, status, code_hmac,
			deadline, review_reason, check_attempts, next_check_at,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		p.ID, string(p.Kind), ownerUID, meterParam,
		p.TargetMinor, p.FeeMinor, string(p.Currency), p.SuggestedShares,
		p.NoteCiphertext, p.NoteKeyVersion, string(p.Status), p.CodeHMAC,
		p.Deadline.UTC(), p.ReviewReason, p.CheckAttempts, p.NextCheckAt.UTC(),
		p.CreatedAt.UTC(), p.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r potRepo) Get(ctx context.Context, id uuid.UUID) (*pots.Pot, error) {
	return r.scanPot(r.db.QueryRow(ctx, potSelectSQL+` WHERE id = $1`, id))
}

func (r potRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*pots.Pot, error) {
	return r.scanPot(r.db.QueryRow(ctx, potSelectSQL+` WHERE id = $1 FOR UPDATE`, id))
}

func (r potRepo) GetByCodeHMAC(ctx context.Context, codeHMAC string) (*pots.Pot, error) {
	p, err := r.scanPot(r.db.QueryRow(ctx, potSelectSQL+` WHERE code_hmac = $1`, codeHMAC))
	if err != nil {
		return nil, err
	}
	if !pots.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (r potRepo) GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*pots.Pot, error) {
	p, err := r.scanPot(r.db.QueryRow(ctx, potSelectSQL+` WHERE code_hmac = $1 FOR UPDATE`, codeHMAC))
	if err != nil {
		return nil, err
	}
	if !pots.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (r potRepo) Update(ctx context.Context, p *pots.Pot) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE pots SET
			status         = $2,
			review_reason  = $3,
			check_attempts = $4,
			next_check_at  = $5,
			updated_at     = $6
		WHERE id = $1`,
		p.ID, string(p.Status), p.ReviewReason, p.CheckAttempts, p.NextCheckAt.UTC(), p.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r potRepo) ClaimDue(ctx context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		UPDATE pots SET next_check_at = $3
		WHERE id IN (
			SELECT id FROM pots
			WHERE review_reason = ''
			  AND status NOT IN ('SETTLED', 'REFUNDED')
			  AND next_check_at <= $1
			  AND (
			      (status = 'OPEN' AND deadline <= $1)
			      OR status IN ('FUNDED', 'EXPIRED', 'CANCELLED', 'REFUNDING')
			      OR updated_at <= $2
			  )
			ORDER BY next_check_at ASC, id ASC
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id`, now.UTC(), staleBefore.UTC(), now.Add(lease).UTC(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

const potSelectSQL = `
	SELECT id, kind, owner_user_id::text, meter_id,
	       target_minor, fee_minor, currency, suggested_shares,
	       note_ciphertext, note_key_version, status, code_hmac,
	       deadline, review_reason, check_attempts, next_check_at,
	       created_at, updated_at
	FROM pots`

func (r potRepo) scanPot(row pgx.Row) (*pots.Pot, error) {
	var (
		p                 pots.Pot
		kindS, cur, statS string
		meterID           *uuid.UUID
	)
	err := row.Scan(
		&p.ID, &kindS, &p.OwnerID, &meterID,
		&p.TargetMinor, &p.FeeMinor, &cur, &p.SuggestedShares,
		&p.NoteCiphertext, &p.NoteKeyVersion, &statS, &p.CodeHMAC,
		&p.Deadline, &p.ReviewReason, &p.CheckAttempts, &p.NextCheckAt,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	if meterID != nil {
		p.MeterID = *meterID
	}
	p.Kind = pots.Kind(kindS)
	p.Currency = money.Currency(cur)
	p.Status = pots.Status(statS)
	p.Deadline = p.Deadline.UTC()
	p.NextCheckAt = p.NextCheckAt.UTC()
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	return &p, nil
}

func (r potRepo) CreateContribution(ctx context.Context, c *pots.Contribution) error {
	var payerParam any
	if c.PayerUserID != "" {
		uid, err := uuid.Parse(c.PayerUserID)
		if err != nil {
			return store.ErrNotFound
		}
		payerParam = uid
	}
	var paidAt any
	if c.PaidAt != nil {
		t := c.PaidAt.UTC()
		paidAt = t
	}
	claimUntil := c.RefundClaimUntil
	if claimUntil.IsZero() {
		claimUntil = time.Unix(0, 0).UTC()
	} else {
		claimUntil = claimUntil.UTC()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO pot_contributions (
			id, pot_id, payer_user_id,
			email_ciphertext, email_key_version,
			name_ciphertext, name_key_version, initials,
			phone_ciphertext, phone_key_version, opt_in_sms,
			amount_minor, retained_minor, refund_amount_minor, refunded_minor, refund_step, currency,
			status, payment_provider_name, payment_provider_ref, checkout_url,
			refund_provider_ref, refund_claim_until, paid_at,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)`,
		c.ID, c.PotID, payerParam,
		c.EmailCiphertext, c.EmailKeyVersion,
		c.NameCiphertext, c.NameKeyVersion, c.Initials,
		c.PhoneCiphertext, c.PhoneKeyVersion, c.OptInSMS,
		c.AmountMinor, c.RetainedMinor, c.RefundAmountMinor, c.RefundedMinor, c.RefundStep, string(c.Currency),
		string(c.Status), c.PaymentProviderName, c.PaymentProviderRef, c.CheckoutURL,
		c.RefundProviderRef, claimUntil, paidAt,
		c.CreatedAt.UTC(), c.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r potRepo) GetContribution(ctx context.Context, id uuid.UUID) (*pots.Contribution, error) {
	return r.scanContrib(r.db.QueryRow(ctx, contribSelectSQL+` WHERE id = $1`, id))
}

func (r potRepo) GetContributionForUpdate(ctx context.Context, id uuid.UUID) (*pots.Contribution, error) {
	return r.scanContrib(r.db.QueryRow(ctx, contribSelectSQL+` WHERE id = $1 FOR UPDATE`, id))
}

func (r potRepo) ContributionByPaymentRef(ctx context.Context, ref string) (*pots.Contribution, error) {
	if ref == "" {
		return nil, store.ErrNotFound
	}
	return r.scanContrib(r.db.QueryRow(ctx, contribSelectSQL+` WHERE payment_provider_ref = $1`, ref))
}

func (r potRepo) UpdateContribution(ctx context.Context, c *pots.Contribution) error {
	var paidAt any
	if c.PaidAt != nil {
		t := c.PaidAt.UTC()
		paidAt = t
	}
	claimUntil := c.RefundClaimUntil
	if claimUntil.IsZero() {
		claimUntil = time.Unix(0, 0).UTC()
	} else {
		claimUntil = claimUntil.UTC()
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE pot_contributions SET
			retained_minor        = $2,
			refund_amount_minor   = $3,
			refunded_minor        = $4,
			refund_step           = $5,
			status                = $6,
			payment_provider_name = $7,
			payment_provider_ref  = $8,
			checkout_url          = $9,
			refund_provider_ref   = $10,
			refund_claim_until    = $11,
			paid_at               = $12,
			updated_at            = $13
		WHERE id = $1`,
		c.ID,
		c.RetainedMinor, c.RefundAmountMinor, c.RefundedMinor, c.RefundStep,
		string(c.Status), c.PaymentProviderName, c.PaymentProviderRef, c.CheckoutURL,
		c.RefundProviderRef, claimUntil, paidAt, c.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r potRepo) ListContributions(ctx context.Context, potID uuid.UUID) ([]*pots.Contribution, error) {
	return r.queryContribs(ctx, contribSelectSQL+` WHERE pot_id = $1 ORDER BY created_at ASC, id ASC`, potID)
}

func (r potRepo) ListContributionsForUpdate(ctx context.Context, potID uuid.UUID) ([]*pots.Contribution, error) {
	return r.queryContribs(ctx, contribSelectSQL+` WHERE pot_id = $1 ORDER BY created_at ASC, id ASC FOR UPDATE`, potID)
}

func (r potRepo) queryContribs(ctx context.Context, q string, arg any) ([]*pots.Contribution, error) {
	rows, err := r.db.Query(ctx, q, arg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*pots.Contribution
	for rows.Next() {
		c, err := r.scanContrib(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

const contribSelectSQL = `
	SELECT id, pot_id, coalesce(payer_user_id::text, ''),
	       email_ciphertext, email_key_version,
	       name_ciphertext, name_key_version, initials,
	       phone_ciphertext, phone_key_version, opt_in_sms,
	       amount_minor, retained_minor, refund_amount_minor, refunded_minor, refund_step, currency,
	       status, payment_provider_name, payment_provider_ref, checkout_url,
	       refund_provider_ref, refund_claim_until, paid_at,
	       created_at, updated_at
	FROM pot_contributions`

func (r potRepo) scanContrib(row pgx.Row) (*pots.Contribution, error) {
	var (
		c          pots.Contribution
		cur, statS string
		paidAt     *time.Time
	)
	err := row.Scan(
		&c.ID, &c.PotID, &c.PayerUserID,
		&c.EmailCiphertext, &c.EmailKeyVersion,
		&c.NameCiphertext, &c.NameKeyVersion, &c.Initials,
		&c.PhoneCiphertext, &c.PhoneKeyVersion, &c.OptInSMS,
		&c.AmountMinor, &c.RetainedMinor, &c.RefundAmountMinor, &c.RefundedMinor, &c.RefundStep, &cur,
		&statS, &c.PaymentProviderName, &c.PaymentProviderRef, &c.CheckoutURL,
		&c.RefundProviderRef, &c.RefundClaimUntil, &paidAt,
		&c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	c.Currency = money.Currency(cur)
	c.Status = pots.ContributionStatus(statS)
	c.RefundClaimUntil = c.RefundClaimUntil.UTC()
	if paidAt != nil {
		t := paidAt.UTC()
		c.PaidAt = &t
	}
	c.CreatedAt = c.CreatedAt.UTC()
	c.UpdatedAt = c.UpdatedAt.UTC()
	return &c, nil
}

func (r potRepo) CreateAllocation(ctx context.Context, a *pots.Allocation) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO pot_allocations (
			id, pot_id, transaction_id, meter_id,
			amount_minor, currency, status, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.ID, a.PotID, a.TransactionID, a.MeterID,
		a.AmountMinor, string(a.Currency), string(a.Status),
		a.CreatedAt.UTC(), a.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r potRepo) GetAllocationByTxn(ctx context.Context, txnID uuid.UUID) (*pots.Allocation, error) {
	return r.scanAlloc(r.db.QueryRow(ctx, allocSelectSQL+` WHERE transaction_id = $1`, txnID))
}

func (r potRepo) GetAllocationByTxnForUpdate(ctx context.Context, txnID uuid.UUID) (*pots.Allocation, error) {
	return r.scanAlloc(r.db.QueryRow(ctx, allocSelectSQL+` WHERE transaction_id = $1 FOR UPDATE`, txnID))
}

func (r potRepo) UpdateAllocation(ctx context.Context, a *pots.Allocation) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE pot_allocations SET status = $2, updated_at = $3 WHERE id = $1`,
		a.ID, string(a.Status), a.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r potRepo) ListAllocations(ctx context.Context, potID uuid.UUID) ([]*pots.Allocation, error) {
	rows, err := r.db.Query(ctx, allocSelectSQL+` WHERE pot_id = $1 ORDER BY created_at ASC, id ASC`, potID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*pots.Allocation
	for rows.Next() {
		a, err := r.scanAlloc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

const allocSelectSQL = `
	SELECT id, pot_id, transaction_id, meter_id, amount_minor, currency, status, created_at, updated_at
	FROM pot_allocations`

func (r potRepo) scanAlloc(row pgx.Row) (*pots.Allocation, error) {
	var (
		a          pots.Allocation
		cur, statS string
	)
	err := row.Scan(&a.ID, &a.PotID, &a.TransactionID, &a.MeterID, &a.AmountMinor, &cur, &statS, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	a.Currency = money.Currency(cur)
	a.Status = pots.AllocationStatus(statS)
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	return &a, nil
}
