package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// The transaction aggregate is stored normalised (spec §5): the main row in
// electricity_transactions, plus payments, vending_records,
// delivery_records, refunds, gift_transactions and transaction_state_events.

type txnRepo struct{ *repos }

const txnSelect = `
SELECT t.id, t.customer_id::text, t.meter_id, t.quote_id, t.is_gift, coalesce(g.recipient_name, ''),
       t.amount_minor, t.fee_minor, t.total_minor, t.currency,
       t.payment_status, t.vending_status, t.delivery_status, t.delivery_method,
       coalesce(pp.name, ''), coalesce(p.provider_reference, ''), coalesce(p.checkout_url, ''),
       coalesce(vp.name, ''), coalesce(v.provider_reference, ''), v.token_ciphertext,
       coalesce(v.token_key_version, ''), coalesce(v.units_kwh::text, ''),
       coalesce(dp.name, ''), coalesce(d.provider_reference, ''),
       coalesce(t.failure_code, ''), coalesce(rf.reason, ''), coalesce(rf.provider_reference, ''),
       t.review_reason, t.check_attempts, t.next_check_at, t.version, t.created_at, t.updated_at
FROM electricity_transactions t
LEFT JOIN gift_transactions g ON g.transaction_id = t.id
LEFT JOIN payments p          ON p.transaction_id = t.id
LEFT JOIN providers pp        ON pp.id = p.provider_id
LEFT JOIN vending_records v   ON v.transaction_id = t.id
LEFT JOIN providers vp        ON vp.id = v.provider_id
LEFT JOIN delivery_records d  ON d.transaction_id = t.id
LEFT JOIN providers dp        ON dp.id = d.provider_id
LEFT JOIN refunds rf          ON rf.transaction_id = t.id
`

func (r txnRepo) load(ctx context.Context, where string, arg any) (*tx.Transaction, error) {
	var t tx.Transaction
	var cur string
	err := r.db.QueryRow(ctx, txnSelect+where, arg).Scan(
		&t.ID, &t.CustomerID, &t.MeterID, &t.QuoteID, &t.IsGift, &t.RecipientName,
		&t.Amount.Minor, &t.Fee.Minor, &t.Total.Minor, &cur,
		&t.Payment, &t.Vending, &t.Delivery, &t.DeliveryMethod,
		&t.PaymentProviderName, &t.PaymentProviderRef, &t.CheckoutURL,
		&t.VendProviderName, &t.VendProviderRef, &t.TokenCiphertext,
		&t.TokenKeyVersion, &t.UnitsKWh,
		&t.DeliveryProviderName, &t.DeliveryProviderRef,
		&t.FailureCode, &t.RefundReason, &t.RefundProviderRef,
		&t.ReviewReason, &t.CheckAttempts, &t.NextCheckAt, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	c := money.Currency(cur)
	t.Amount.Currency, t.Fee.Currency, t.Total.Currency = c, c, c
	t.NextCheckAt, t.CreatedAt, t.UpdatedAt = t.NextCheckAt.UTC(), t.CreatedAt.UTC(), t.UpdatedAt.UTC()

	rows, err := r.db.Query(ctx, `SELECT dimension, from_state, to_state, coalesce(note, ''), created_at
		FROM transaction_state_events WHERE transaction_id = $1 ORDER BY id`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e tx.Event
		if err := rows.Scan(&e.Dimension, &e.From, &e.To, &e.Note, &e.At); err != nil {
			return nil, err
		}
		e.At = e.At.UTC()
		t.History = append(t.History, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t.PersistedEvents = len(t.History)
	return &t, nil
}

func (r txnRepo) Get(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	return r.load(ctx, `WHERE t.id = $1`, id)
}

func (r txnRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	return r.load(ctx, `WHERE t.id = $1 FOR UPDATE OF t`, id)
}

func (r txnRepo) ByPaymentRef(ctx context.Context, ref string) (*tx.Transaction, error) {
	return r.load(ctx, `WHERE p.provider_reference = $1`, ref)
}

func (r txnRepo) Create(ctx context.Context, t *tx.Transaction) error {
	return r.atomic(ctx, func(db DB) error {
		_, err := db.Exec(ctx, `
			INSERT INTO electricity_transactions (id, customer_id, meter_id, quote_id, amount_minor, fee_minor, total_minor,
			    currency, payment_status, vending_status, delivery_status, delivery_method, is_gift, failure_code,
			    review_reason, check_attempts, next_check_at, version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NULLIF($14, ''), $15, $16, $17, 1, $18, $19)`,
			t.ID, t.CustomerID, t.MeterID, t.QuoteID, t.Amount.Minor, t.Fee.Minor, t.Total.Minor,
			string(t.Total.Currency), t.Payment, t.Vending, t.Delivery, t.DeliveryMethod, t.IsGift, t.FailureCode,
			t.ReviewReason, t.CheckAttempts, t.NextCheckAt, t.CreatedAt, t.UpdatedAt)
		if err != nil {
			return mapErr(err)
		}
		if t.IsGift {
			if _, err := db.Exec(ctx, `INSERT INTO gift_transactions (transaction_id, purchaser_user_id, recipient_name)
				VALUES ($1, $2, NULLIF($3, ''))`, t.ID, t.CustomerID, t.RecipientName); err != nil {
				return mapErr(err)
			}
		}
		if err := r.saveChildren(ctx, db, t); err != nil {
			return err
		}
		t.Version = 1
		t.PersistedEvents = len(t.History)
		return nil
	})
}

func (r txnRepo) Save(ctx context.Context, t *tx.Transaction) error {
	return r.atomic(ctx, func(db DB) error {
		tag, err := db.Exec(ctx, `
			UPDATE electricity_transactions
			SET payment_status = $3, vending_status = $4, delivery_status = $5, delivery_method = $6,
			    failure_code = NULLIF($7, ''), review_reason = $8, check_attempts = $9, next_check_at = $10,
			    updated_at = $11, version = version + 1
			WHERE id = $1 AND version = $2`,
			t.ID, t.Version, t.Payment, t.Vending, t.Delivery, t.DeliveryMethod,
			t.FailureCode, t.ReviewReason, t.CheckAttempts, t.NextCheckAt, t.UpdatedAt)
		if err != nil {
			return mapErr(err)
		}
		if tag.RowsAffected() == 0 {
			return store.ErrConflict
		}
		if err := r.saveChildren(ctx, db, t); err != nil {
			return err
		}
		t.Version++
		t.PersistedEvents = len(t.History)
		return nil
	})
}

func (r txnRepo) saveChildren(ctx context.Context, db DB, t *tx.Transaction) error {
	if t.PaymentProviderRef != "" {
		pid, err := r.providerID(ctx, t.PaymentProviderName, "PAYMENT")
		if err != nil {
			return err
		}
		verified := t.Payment == tx.PayConfirmed || t.Payment == tx.PayRefundPending || t.Payment == tx.PayRefunded
		if _, err := db.Exec(ctx, `
			INSERT INTO payments (transaction_id, provider_id, provider_reference, amount_minor, currency, status, checkout_url, verified_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), CASE WHEN $8 THEN now() END)
			ON CONFLICT (transaction_id) DO UPDATE SET
			    status = EXCLUDED.status, provider_reference = EXCLUDED.provider_reference,
			    checkout_url = EXCLUDED.checkout_url,
			    verified_at = coalesce(payments.verified_at, EXCLUDED.verified_at)`,
			t.ID, pid, t.PaymentProviderRef, t.Total.Minor, string(t.Total.Currency), t.Payment, t.CheckoutURL, verified); err != nil {
			return mapErr(err)
		}
	}
	if t.Vending != tx.VendNotStarted {
		vid, err := r.providerID(ctx, t.VendProviderName, "VENDING")
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO vending_records (transaction_id, provider_id, idempotency_key, provider_reference, status,
			                             token_ciphertext, token_key_version, units_kwh, confirmed_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, NULLIF($7, ''), NULLIF($8::text, '')::numeric,
			        CASE WHEN $5 = 'CONFIRMED' THEN now() END)
			ON CONFLICT (transaction_id) DO UPDATE SET
			    provider_reference = EXCLUDED.provider_reference, status = EXCLUDED.status,
			    token_ciphertext = EXCLUDED.token_ciphertext, token_key_version = EXCLUDED.token_key_version,
			    units_kwh = EXCLUDED.units_kwh,
			    confirmed_at = coalesce(vending_records.confirmed_at, EXCLUDED.confirmed_at)`,
			t.ID, vid, "vend:"+t.ID.String(), t.VendProviderRef, t.Vending,
			nilIfEmpty(t.TokenCiphertext), t.TokenKeyVersion, t.UnitsKWh); err != nil {
			return mapErr(err)
		}
	}
	if t.Delivery != tx.DelNotStarted {
		var did any
		if t.DeliveryProviderName != "" {
			id, err := r.providerID(ctx, t.DeliveryProviderName, "AMI")
			if err != nil {
				return err
			}
			did = id
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO delivery_records (transaction_id, provider_id, idempotency_key, provider_reference, delivery_method,
			                              status, accepted_at, confirmed_at, failure_code)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6,
			        CASE WHEN $6 IN ('ACCEPTED', 'CONFIRMED') THEN now() END,
			        CASE WHEN $6 = 'CONFIRMED' THEN now() END, NULLIF($7, ''))
			ON CONFLICT (transaction_id) DO UPDATE SET
			    provider_id = EXCLUDED.provider_id, provider_reference = EXCLUDED.provider_reference,
			    delivery_method = EXCLUDED.delivery_method, status = EXCLUDED.status,
			    accepted_at = coalesce(delivery_records.accepted_at, EXCLUDED.accepted_at),
			    confirmed_at = coalesce(delivery_records.confirmed_at, EXCLUDED.confirmed_at),
			    failure_code = EXCLUDED.failure_code`,
			t.ID, did, "delivery:"+t.ID.String(), t.DeliveryProviderRef, t.DeliveryMethod, t.Delivery, t.FailureCode); err != nil {
			return mapErr(err)
		}
	}
	if t.Payment == tx.PayRefundPending || t.Payment == tx.PayRefunded {
		if _, err := db.Exec(ctx, `
			INSERT INTO refunds (transaction_id, payment_id, amount_minor, reason, status, provider_reference)
			SELECT $1, p.id, $2, $3, $4, NULLIF($5, '') FROM payments p WHERE p.transaction_id = $1
			ON CONFLICT (transaction_id) DO UPDATE SET
			    status = EXCLUDED.status, provider_reference = EXCLUDED.provider_reference`,
			t.ID, t.Total.Minor, t.RefundReason, t.Payment, t.RefundProviderRef); err != nil {
			return mapErr(err)
		}
	}
	for _, e := range t.History[t.PersistedEvents:] {
		if _, err := db.Exec(ctx, `INSERT INTO transaction_state_events (transaction_id, dimension, from_state, to_state, note, actor, created_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), 'system', $6)`, t.ID, e.Dimension, e.From, e.To, e.Note, e.At); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (r txnRepo) ListForCustomer(ctx context.Context, customerID string) ([]*tx.Transaction, error) {
	rows, err := r.db.Query(ctx, `SELECT id FROM electricity_transactions WHERE customer_id = $1
		ORDER BY created_at DESC LIMIT 200`, customerID)
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

func (r txnRepo) ClaimDue(ctx context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE electricity_transactions SET next_check_at = $3
		WHERE id IN (
		    SELECT id FROM electricity_transactions
		    WHERE review_reason = ''
		      AND payment_status NOT IN ('FAILED', 'REFUNDED')
		      AND delivery_status NOT IN ('CONFIRMED', 'FAILED')
		      AND next_check_at <= $1 AND updated_at <= $2
		    ORDER BY next_check_at
		    LIMIT $4
		    FOR UPDATE SKIP LOCKED)
		RETURNING id`, now, staleBefore, now.Add(lease), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
