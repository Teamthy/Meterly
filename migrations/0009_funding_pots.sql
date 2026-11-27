-- 0009: M8 reusable Funding Pot engine (Split the Meter + future Landlord Bulk),
-- POT_FUNDS_HELD liability account, and POT funding source on electricity_transactions.

INSERT INTO ledger_accounts (account_code, account_name, account_type, currency)
VALUES ('POT_FUNDS_HELD', 'Pot funds held', 'LIABILITY', 'NGN')
ON CONFLICT (account_code) DO NOTHING;

ALTER TABLE electricity_transactions
    ADD COLUMN IF NOT EXISTS funding_source      TEXT   NOT NULL DEFAULT 'PSP' CHECK (funding_source IN ('PSP', 'POT')),
    ADD COLUMN IF NOT EXISTS refund_reason       TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS refund_amount_minor BIGINT NOT NULL DEFAULT 0 CHECK (refund_amount_minor >= 0);

ALTER TABLE notification_deliveries
    DROP CONSTRAINT IF EXISTS notification_deliveries_recipient_kind_check,
    ADD CONSTRAINT notification_deliveries_recipient_kind_check
        CHECK (recipient_kind IN ('PURCHASER', 'GIFT_RECIPIENT', 'PAYER', 'POT_CONTRIBUTOR'));

CREATE TABLE IF NOT EXISTS pots (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              TEXT        NOT NULL CHECK (kind IN ('SPLIT_METER', 'LANDLORD_BULK')),
    owner_user_id     UUID        NOT NULL REFERENCES users(id),
    meter_id          UUID        REFERENCES meters(id),
    target_minor      BIGINT      NOT NULL CHECK (target_minor > 0),
    fee_minor         BIGINT      NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    currency          TEXT        NOT NULL DEFAULT 'NGN',
    suggested_shares  INTEGER     NOT NULL DEFAULT 0 CHECK (suggested_shares = 0 OR (suggested_shares >= 2 AND suggested_shares <= 50)),
    note_ciphertext   BYTEA,
    note_key_version  TEXT        NOT NULL DEFAULT '',
    status            TEXT        NOT NULL CHECK (status IN ('OPEN', 'FUNDED', 'ALLOCATING', 'SETTLED', 'EXPIRED', 'CANCELLED', 'REFUNDING', 'REFUNDED')),
    code_hmac         TEXT        NOT NULL UNIQUE,
    deadline          TIMESTAMPTZ NOT NULL,
    review_reason     TEXT        NOT NULL DEFAULT '',
    check_attempts    INTEGER     NOT NULL DEFAULT 0 CHECK (check_attempts >= 0),
    next_check_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pots_split_meter_requires_meter_check CHECK (
        kind <> 'SPLIT_METER' OR meter_id IS NOT NULL
    )
);

CREATE INDEX IF NOT EXISTS pots_owner_idx
    ON pots (owner_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS pots_due_idx
    ON pots (next_check_at)
    WHERE review_reason = '' AND status NOT IN ('SETTLED', 'REFUNDED');

CREATE TABLE IF NOT EXISTS pot_contributions (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pot_id                  UUID        NOT NULL REFERENCES pots(id),
    payer_user_id           UUID        REFERENCES users(id),
    email_ciphertext        BYTEA       NOT NULL,
    email_key_version       TEXT        NOT NULL,
    name_ciphertext         BYTEA,
    name_key_version        TEXT        NOT NULL DEFAULT '',
    initials                TEXT        NOT NULL DEFAULT 'Anonymous',
    phone_ciphertext        BYTEA,
    phone_key_version       TEXT        NOT NULL DEFAULT '',
    opt_in_sms              BOOLEAN     NOT NULL DEFAULT false,
    amount_minor            BIGINT      NOT NULL CHECK (amount_minor > 0),
    retained_minor          BIGINT      NOT NULL DEFAULT 0 CHECK (retained_minor >= 0 AND retained_minor <= amount_minor),
    refund_amount_minor     BIGINT      NOT NULL DEFAULT 0 CHECK (refund_amount_minor >= 0 AND refund_amount_minor <= amount_minor),
    refunded_minor          BIGINT      NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0 AND refunded_minor <= refund_amount_minor),
    refund_step             INTEGER     NOT NULL DEFAULT 0 CHECK (refund_step >= 0),
    currency                TEXT        NOT NULL DEFAULT 'NGN',
    status                  TEXT        NOT NULL CHECK (status IN ('PENDING', 'PAID', 'REFUND_PENDING', 'REFUNDED', 'FAILED')),
    payment_provider_name   TEXT        NOT NULL DEFAULT '',
    payment_provider_ref    TEXT        NOT NULL DEFAULT '',
    checkout_url            TEXT        NOT NULL DEFAULT '',
    refund_provider_ref     TEXT        NOT NULL DEFAULT '',
    refund_claim_until      TIMESTAMPTZ NOT NULL DEFAULT TIMESTAMP WITH TIME ZONE 'epoch',
    paid_at                 TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pot_contributions_accounting_check CHECK (
        retained_minor + refund_amount_minor <= amount_minor
    )
);

CREATE INDEX IF NOT EXISTS pot_contributions_pot_idx
    ON pot_contributions (pot_id, created_at ASC, id ASC);

CREATE UNIQUE INDEX IF NOT EXISTS pot_contributions_payref_idx
    ON pot_contributions (payment_provider_ref)
    WHERE payment_provider_ref <> '';

CREATE TABLE IF NOT EXISTS pot_allocations (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pot_id         UUID        NOT NULL REFERENCES pots(id),
    transaction_id UUID        NOT NULL UNIQUE REFERENCES electricity_transactions(id),
    meter_id       UUID        NOT NULL REFERENCES meters(id),
    amount_minor   BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency       TEXT        NOT NULL DEFAULT 'NGN',
    status         TEXT        NOT NULL CHECK (status IN ('ACTIVE', 'RETURNED')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS pot_allocations_pot_idx
    ON pot_allocations (pot_id, created_at ASC);
