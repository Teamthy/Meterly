package pgstore

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/store"
)

type payReqRepo struct{ *repos }

func (r payReqRepo) Create(ctx context.Context, p *payrequests.PaymentRequest) error {
	requesterUID, err := uuid.Parse(p.RequesterID)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO payment_requests (
			id, requester_user_id, meter_id, amount_mode,
			amount_minor, min_amount_minor, max_amount_minor, currency,
			note_ciphertext, note_key_version, status,
			max_payments, payments_count, code_hmac,
			expires_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		p.ID, requesterUID, p.MeterID, string(p.AmountMode),
		p.AmountMinor, p.MinAmountMinor, p.MaxAmountMinor, string(p.Currency),
		p.NoteCiphertext, p.NoteKeyVersion, string(p.Status),
		p.MaxPayments, p.PaymentsCount, p.CodeHMAC,
		p.ExpiresAt.UTC(), p.CreatedAt.UTC(), p.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r payReqRepo) Get(ctx context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error) {
	return r.scanOne(r.db.QueryRow(ctx, payReqSelectSQL+` WHERE id = $1`, id))
}

func (r payReqRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error) {
	return r.scanOne(r.db.QueryRow(ctx, payReqSelectSQL+` WHERE id = $1 FOR UPDATE`, id))
}

func (r payReqRepo) GetByCodeHMAC(ctx context.Context, codeHMAC string) (*payrequests.PaymentRequest, error) {
	p, err := r.scanOne(r.db.QueryRow(ctx, payReqSelectSQL+` WHERE code_hmac = $1`, codeHMAC))
	if err != nil {
		return nil, err
	}
	if !payrequests.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (r payReqRepo) GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*payrequests.PaymentRequest, error) {
	p, err := r.scanOne(r.db.QueryRow(ctx, payReqSelectSQL+` WHERE code_hmac = $1 FOR UPDATE`, codeHMAC))
	if err != nil {
		return nil, err
	}
	if !payrequests.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (r payReqRepo) Update(ctx context.Context, p *payrequests.PaymentRequest) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE payment_requests SET
			status         = $2,
			payments_count = $3,
			updated_at     = $4
		WHERE id = $1`,
		p.ID, string(p.Status), p.PaymentsCount, p.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r payReqRepo) ListForRequester(ctx context.Context, requesterID string) ([]*payrequests.PaymentRequest, error) {
	uid, err := uuid.Parse(requesterID)
	if err != nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, payReqSelectSQL+` WHERE requester_user_id = $1 ORDER BY created_at DESC, id ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*payrequests.PaymentRequest
	for rows.Next() {
		p, err := r.scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, mapErr(rows.Err())
}

const payReqSelectSQL = `
	SELECT id, requester_user_id::text, meter_id, amount_mode,
	       amount_minor, min_amount_minor, max_amount_minor, currency,
	       note_ciphertext, note_key_version, status,
	       max_payments, payments_count, code_hmac,
	       expires_at, created_at, updated_at
	FROM payment_requests`

func (r payReqRepo) scanOne(row pgx.Row) (*payrequests.PaymentRequest, error) {
	var (
		p                   payrequests.PaymentRequest
		mode, curr, statusS string
	)
	err := row.Scan(
		&p.ID, &p.RequesterID, &p.MeterID, &mode,
		&p.AmountMinor, &p.MinAmountMinor, &p.MaxAmountMinor, &curr,
		&p.NoteCiphertext, &p.NoteKeyVersion, &statusS,
		&p.MaxPayments, &p.PaymentsCount, &p.CodeHMAC,
		&p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	p.AmountMode = payrequests.AmountMode(mode)
	p.Currency = money.Currency(curr)
	p.Status = payrequests.Status(statusS)
	p.ExpiresAt = p.ExpiresAt.UTC()
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	return &p, nil
}

func (r payReqRepo) CreatePayer(ctx context.Context, payer payrequests.Payer) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO transaction_payers (
			transaction_id, payment_request_id,
			email_ciphertext, email_key_version,
			payer_name_ciphertext, payer_name_key_version,
			status_code_hmac, status_code_ciphertext, status_code_key_version,
			slot_released, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		payer.TransactionID, payer.PaymentRequestID,
		payer.EmailCiphertext, payer.EmailKeyVersion,
		payer.PayerNameCiphertext, payer.PayerNameKeyVersion,
		payer.StatusCodeHMAC, payer.StatusCodeCiphertext, payer.StatusCodeKeyVersion,
		payer.SlotReleased, payer.CreatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r payReqRepo) GetPayer(ctx context.Context, txnID uuid.UUID) (payrequests.Payer, error) {
	return r.scanPayer(r.db.QueryRow(ctx, payerSelectSQL+` WHERE transaction_id = $1`, txnID))
}

func (r payReqRepo) GetPayerForUpdate(ctx context.Context, txnID uuid.UUID) (payrequests.Payer, error) {
	return r.scanPayer(r.db.QueryRow(ctx, payerSelectSQL+` WHERE transaction_id = $1 FOR UPDATE`, txnID))
}

func (r payReqRepo) GetPayerByStatusHMAC(ctx context.Context, statusHMAC string) (payrequests.Payer, error) {
	p, err := r.scanPayer(r.db.QueryRow(ctx, payerSelectSQL+` WHERE status_code_hmac = $1`, statusHMAC))
	if err != nil {
		return payrequests.Payer{}, err
	}
	if !payrequests.ConstantTimeEqualHMAC(p.StatusCodeHMAC, statusHMAC) {
		return payrequests.Payer{}, store.ErrNotFound
	}
	return p, nil
}

func (r payReqRepo) MarkPayerSlotReleased(ctx context.Context, txnID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE transaction_payers SET slot_released = true WHERE transaction_id = $1`, txnID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const payerSelectSQL = `
	SELECT transaction_id, payment_request_id,
	       email_ciphertext, email_key_version,
	       payer_name_ciphertext, payer_name_key_version,
	       status_code_hmac, status_code_ciphertext, status_code_key_version,
	       slot_released, created_at
	FROM transaction_payers`

func (r payReqRepo) scanPayer(row pgx.Row) (payrequests.Payer, error) {
	var p payrequests.Payer
	err := row.Scan(
		&p.TransactionID, &p.PaymentRequestID,
		&p.EmailCiphertext, &p.EmailKeyVersion,
		&p.PayerNameCiphertext, &p.PayerNameKeyVersion,
		&p.StatusCodeHMAC, &p.StatusCodeCiphertext, &p.StatusCodeKeyVersion,
		&p.SlotReleased, &p.CreatedAt,
	)
	if err != nil {
		return payrequests.Payer{}, mapErr(err)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}
