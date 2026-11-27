-- 0029: Partner platform part 2 (B5). Prefunded partner float and purchase API.
-- PARTNER_FLOAT is a liability: operators record bank receipts (two-person),
-- purchases debit the float, definitive vend-failure refunds credit it back.

INSERT INTO ledger_accounts (account_code, account_name, account_type, currency)
VALUES ('PARTNER_FLOAT', 'Partner float held', 'LIABILITY', 'NGN')
ON CONFLICT (account_code) DO NOTHING;

ALTER TABLE electricity_transactions
    DROP CONSTRAINT IF EXISTS electricity_transactions_funding_source_check;
ALTER TABLE electricity_transactions
    ADD CONSTRAINT electricity_transactions_funding_source_check
        CHECK (funding_source IN ('PSP', 'POT', 'PARTNER_FLOAT'));

ALTER TABLE electricity_transactions
    ADD COLUMN IF NOT EXISTS partner_id  UUID REFERENCES partners(id),
    ADD COLUMN IF NOT EXISTS partner_ref TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS electricity_transactions_partner_ref_uidx
    ON electricity_transactions (partner_id, partner_ref)
    WHERE partner_id IS NOT NULL AND partner_ref <> '';

CREATE INDEX IF NOT EXISTS electricity_transactions_partner_idx
    ON electricity_transactions (partner_id, created_at DESC)
    WHERE partner_id IS NOT NULL;

ALTER TABLE quotes
    ADD COLUMN IF NOT EXISTS partner_id UUID REFERENCES partners(id);

CREATE TABLE IF NOT EXISTS partner_float (
    partner_id       UUID        PRIMARY KEY REFERENCES partners(id),
    available_minor  BIGINT      NOT NULL DEFAULT 0 CHECK (available_minor >= 0),
    currency         CHAR(3)     NOT NULL DEFAULT 'NGN',
    version          INTEGER     NOT NULL DEFAULT 1,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS partner_float_topups (
    id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    partner_id         UUID        NOT NULL REFERENCES partners(id),
    receipt_ref        TEXT        NOT NULL,
    amount_minor       BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency           CHAR(3)     NOT NULL DEFAULT 'NGN',
    status             TEXT        NOT NULL CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    proposed_by        UUID        NOT NULL REFERENCES users(id),
    proposed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by        UUID        REFERENCES users(id),
    approved_at        TIMESTAMPTZ,
    rejected_by        UUID        REFERENCES users(id),
    rejected_at        TIMESTAMPTZ,
    reason             TEXT        NOT NULL DEFAULT '',
    ledger_reference   TEXT        NOT NULL DEFAULT '',
    UNIQUE (receipt_ref)
);
CREATE INDEX IF NOT EXISTS partner_float_topups_partner_idx
    ON partner_float_topups (partner_id, proposed_at DESC);
