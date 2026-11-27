-- 0007: M7 Pay-for-Me payment request links and guest transaction payers.
-- Link codes and status codes are never stored in plaintext; notes and guest
-- contact fields are AES-256-GCM encrypted bound to their owning rows.

CREATE TABLE IF NOT EXISTS payment_requests (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_user_id UUID        NOT NULL REFERENCES users(id),
    meter_id          UUID        NOT NULL REFERENCES meters(id),
    amount_mode       TEXT        NOT NULL CHECK (amount_mode IN ('FIXED', 'RANGE')),
    amount_minor      BIGINT      NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    min_amount_minor  BIGINT      NOT NULL DEFAULT 0 CHECK (min_amount_minor >= 0),
    max_amount_minor  BIGINT      NOT NULL DEFAULT 0 CHECK (max_amount_minor >= 0),
    currency          TEXT        NOT NULL DEFAULT 'NGN',
    note_ciphertext   BYTEA,
    note_key_version  TEXT        NOT NULL DEFAULT '',
    status            TEXT        NOT NULL CHECK (status IN ('OPEN', 'PAID', 'EXPIRED', 'CANCELLED')),
    max_payments      INTEGER     NOT NULL DEFAULT 1 CHECK (max_payments >= 1 AND max_payments <= 100),
    payments_count    INTEGER     NOT NULL DEFAULT 0 CHECK (payments_count >= 0 AND payments_count <= max_payments),
    code_hmac         TEXT        NOT NULL UNIQUE,
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_requests_amounts_by_mode_check CHECK (
        (amount_mode = 'FIXED' AND amount_minor > 0 AND min_amount_minor = 0 AND max_amount_minor = 0)
        OR (amount_mode = 'RANGE' AND amount_minor = 0 AND min_amount_minor > 0 AND max_amount_minor >= min_amount_minor)
    )
);

CREATE INDEX IF NOT EXISTS payment_requests_requester_idx
    ON payment_requests (requester_user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS transaction_payers (
    transaction_id          UUID        PRIMARY KEY REFERENCES electricity_transactions(id),
    payment_request_id      UUID        NOT NULL REFERENCES payment_requests(id),
    email_ciphertext        BYTEA       NOT NULL,
    email_key_version       TEXT        NOT NULL,
    payer_name_ciphertext   BYTEA,
    payer_name_key_version  TEXT        NOT NULL DEFAULT '',
    status_code_hmac        TEXT        NOT NULL UNIQUE,
    status_code_ciphertext  BYTEA       NOT NULL,
    status_code_key_version TEXT        NOT NULL,
    slot_released           BOOLEAN     NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS transaction_payers_request_idx
    ON transaction_payers (payment_request_id);
