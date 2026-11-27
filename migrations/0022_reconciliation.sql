-- 0022: daily settlement / reconciliation (P2).
-- New ledger accounts for cash, PSP fees, VTpass wallet prefund and
-- commission; run + exception + two-person adjustment tables.
-- Fee and commission treatment is the P2 default (flagged in DECISIONS).

INSERT INTO ledger_accounts (account_code, account_name, account_type, currency) VALUES
    ('CASH_AT_BANK',              'Cash at bank',              'ASSET',   'NGN'),
    ('PSP_FEES_EXPENSE',          'PSP fees',                  'EXPENSE', 'NGN'),
    ('VENDOR_WALLET_PREFUND',     'Vendor wallet prefund',    'ASSET',   'NGN'),
    ('VENDOR_COMMISSION_INCOME',  'Vendor commission income',  'INCOME',  'NGN')
ON CONFLICT (account_code) DO NOTHING;

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider         TEXT        NOT NULL CHECK (provider IN ('paystack', 'vtpass')),
    business_date    DATE        NOT NULL,
    status           TEXT        NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    matched_count    INTEGER     NOT NULL DEFAULT 0 CHECK (matched_count >= 0),
    matched_minor    BIGINT      NOT NULL DEFAULT 0 CHECK (matched_minor >= 0),
    exception_count  INTEGER     NOT NULL DEFAULT 0 CHECK (exception_count >= 0),
    error            TEXT        NOT NULL DEFAULT '',
    UNIQUE (provider, business_date)
);

CREATE TABLE IF NOT EXISTS reconciliation_exceptions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID        NOT NULL REFERENCES reconciliation_runs(id),
    provider            TEXT        NOT NULL CHECK (provider IN ('paystack', 'vtpass')),
    business_date       DATE        NOT NULL,
    category            TEXT        NOT NULL CHECK (category IN (
        'IN_PSP_NOT_OURS',
        'IN_OURS_NOT_PSP',
        'AMOUNT_MISMATCH',
        'CURRENCY_MISMATCH',
        'STATUS_MISMATCH',
        'REFUND_MISMATCH',
        'TOKEN_MISSING',
        'SETTLEMENT_UNBALANCED'
    )),
    severity            TEXT        NOT NULL CHECK (severity IN ('HIGH', 'MEDIUM', 'LOW')),
    provider_ref        TEXT        NOT NULL DEFAULT '',
    our_transaction_id  UUID        REFERENCES electricity_transactions(id),
    ours_minor          BIGINT      NOT NULL DEFAULT 0,
    theirs_minor        BIGINT      NOT NULL DEFAULT 0,
    currency            CHAR(3)     NOT NULL DEFAULT 'NGN',
    status              TEXT        NOT NULL CHECK (status IN ('OPEN', 'INVESTIGATING', 'RESOLVED', 'WRITTEN_OFF')),
    owner               TEXT        NOT NULL DEFAULT '',
    notes               TEXT        NOT NULL DEFAULT '',
    fingerprint         TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS reconciliation_exceptions_fingerprint_uq
    ON reconciliation_exceptions (fingerprint);
CREATE INDEX IF NOT EXISTS reconciliation_exceptions_open_idx
    ON reconciliation_exceptions (provider, category)
    WHERE status IN ('OPEN', 'INVESTIGATING');

CREATE TABLE IF NOT EXISTS reconciliation_adjustments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    exception_id      UUID        NOT NULL REFERENCES reconciliation_exceptions(id),
    proposed_by       UUID        NOT NULL REFERENCES users(id),
    approved_by       UUID        REFERENCES users(id),
    status            TEXT        NOT NULL CHECK (status IN ('PROPOSED', 'APPROVED', 'REJECTED')),
    debit_account     TEXT        NOT NULL,
    credit_account    TEXT        NOT NULL,
    amount_minor      BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency          CHAR(3)     NOT NULL DEFAULT 'NGN',
    ledger_reference  TEXT        NOT NULL DEFAULT '',
    reason            TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_at       TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS reconciliation_adjustments_open_uq
    ON reconciliation_adjustments (exception_id)
    WHERE status = 'PROPOSED';
