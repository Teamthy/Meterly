-- Columns and constraints needed to persist the orchestrator's aggregate
-- and to run the resume/reconciliation worker.

-- Meters: validated identity and capability snapshot. The per-provider
-- capability matrix (meter_capabilities) is reserved for when more than one
-- vending/AMI provider can serve the same meter; see docs/DECISIONS.md.
ALTER TABLE meters
    ADD COLUMN customer_name TEXT,
    ADD COLUMN address       TEXT,
    ADD COLUMN capabilities  JSONB NOT NULL DEFAULT '{}';

ALTER TABLE quotes
    ADD COLUMN delivery_options TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN created_at       TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE electricity_transactions
    ADD COLUMN review_reason  TEXT NOT NULL DEFAULT '',
    ADD COLUMN check_attempts INTEGER NOT NULL DEFAULT 0 CHECK (check_attempts >= 0),
    ADD COLUMN next_check_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN failure_code   TEXT;

DROP INDEX electricity_transactions_recon_idx;
CREATE INDEX electricity_transactions_due_idx ON electricity_transactions (next_check_at)
    WHERE review_reason = ''
      AND payment_status NOT IN ('FAILED', 'REFUNDED')
      AND delivery_status NOT IN ('CONFIRMED', 'FAILED');

-- One payment / vend / delivery / refund record per transaction for now.
ALTER TABLE payments ADD COLUMN checkout_url TEXT;
CREATE UNIQUE INDEX payments_transaction_uq         ON payments (transaction_id);
CREATE UNIQUE INDEX vending_records_transaction_uq  ON vending_records (transaction_id);
CREATE UNIQUE INDEX delivery_records_transaction_uq ON delivery_records (transaction_id);
CREATE UNIQUE INDEX refunds_transaction_uq          ON refunds (transaction_id);
CREATE INDEX payments_reference_idx ON payments (provider_reference);

-- 'review' records moves into MANUAL_REVIEW.
ALTER TABLE transaction_state_events DROP CONSTRAINT transaction_state_events_dimension_check;
ALTER TABLE transaction_state_events ADD CONSTRAINT transaction_state_events_dimension_check
    CHECK (dimension IN ('payment', 'vending', 'delivery', 'review'));
CREATE INDEX transaction_state_events_txn_idx ON transaction_state_events (transaction_id, id);

CREATE INDEX meter_access_user_idx ON meter_access_grants (user_id) WHERE revoked_at IS NULL;
CREATE INDEX outbox_aggregate_idx ON outbox_events (aggregate_id);
