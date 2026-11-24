-- 0011: M9 AutoPower — saved payment methods (AEAD-bound reusable card
-- authorizations + explicit consent), scheduled & capability-gated low-balance
-- rules, idempotent slot runs, and account-level notification deliveries.

ALTER TABLE notification_deliveries
    ALTER COLUMN transaction_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS user_id UUID REFERENCES users(id);

ALTER TABLE notification_deliveries
    DROP CONSTRAINT IF EXISTS notification_deliveries_target_check,
    ADD CONSTRAINT notification_deliveries_target_check
        CHECK (transaction_id IS NOT NULL OR user_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS notification_deliveries_user_idx
    ON notification_deliveries (user_id, created_at DESC)
    WHERE user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS saved_payment_methods (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               UUID        NOT NULL REFERENCES users(id),
    provider              TEXT        NOT NULL,
    source_transaction_id UUID        NOT NULL REFERENCES electricity_transactions(id),
    auth_code_ciphertext  BYTEA       NOT NULL,
    email_ciphertext      BYTEA       NOT NULL,
    key_version           TEXT        NOT NULL,
    card_brand            TEXT        NOT NULL,
    last4                 TEXT        NOT NULL CHECK (char_length(last4) = 4),
    exp_month             INTEGER     NOT NULL CHECK (exp_month >= 1 AND exp_month <= 12),
    exp_year              INTEGER     NOT NULL CHECK (exp_year >= 2000 AND exp_year <= 2200),
    signature             TEXT        NOT NULL DEFAULT '',
    consent_version       TEXT        NOT NULL,
    consent_text          TEXT        NOT NULL,
    consented_at          TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at            TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS saved_payment_methods_active_source_uq
    ON saved_payment_methods (user_id, source_transaction_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS saved_payment_methods_user_idx
    ON saved_payment_methods (user_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS autopower_rules (
    id                            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                       UUID        NOT NULL REFERENCES users(id),
    meter_id                      UUID        NOT NULL REFERENCES meters(id),
    payment_method_id             UUID        NOT NULL REFERENCES saved_payment_methods(id),
    trigger_type                  TEXT        NOT NULL CHECK (trigger_type IN ('SCHEDULE', 'LOW_BALANCE', 'BOTH')),
    amount_minor                  BIGINT      NOT NULL CHECK (amount_minor > 0),
    monthly_cap_minor             BIGINT      NOT NULL CHECK (monthly_cap_minor >= amount_minor),
    currency                      CHAR(3)     NOT NULL DEFAULT 'NGN',
    delivery_preference           TEXT        NOT NULL DEFAULT 'AUTO' CHECK (delivery_preference IN ('AUTO', 'REMOTE', 'TOKEN')),
    frequency                     TEXT        NOT NULL DEFAULT '' CHECK (frequency IN ('', 'WEEKLY', 'MONTHLY')),
    days_of_week                  INTEGER[]   NOT NULL DEFAULT '{}',
    days_of_month                 INTEGER[]   NOT NULL DEFAULT '{}',
    time_of_day                   TEXT        NOT NULL DEFAULT '',
    timezone                      TEXT        NOT NULL DEFAULT 'Africa/Lagos',
    ends_at                       TIMESTAMPTZ,
    low_balance_milli_kwh         BIGINT      NOT NULL DEFAULT 0 CHECK (low_balance_milli_kwh >= 0),
    status                        TEXT        NOT NULL CHECK (status IN ('ACTIVE', 'PAUSED', 'CANCELLED', 'COMPLETED')),
    pause_reason                  TEXT        NOT NULL DEFAULT '',
    consecutive_failures          INTEGER     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    next_scheduled_at             TIMESTAMPTZ,
    next_poll_at                  TIMESTAMPTZ,
    last_low_balance_triggered_at TIMESTAMPTZ,
    last_reading_milli_kwh        BIGINT,
    last_reading_at               TIMESTAMPTZ,
    lease_until                   TIMESTAMPTZ NOT NULL DEFAULT TIMESTAMP WITH TIME ZONE 'epoch',
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS autopower_rules_user_idx
    ON autopower_rules (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS autopower_rules_payment_method_idx
    ON autopower_rules (payment_method_id)
    WHERE status <> 'CANCELLED';

CREATE INDEX IF NOT EXISTS autopower_rules_sched_due_idx
    ON autopower_rules (next_scheduled_at)
    WHERE status = 'ACTIVE' AND next_scheduled_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS autopower_rules_poll_due_idx
    ON autopower_rules (next_poll_at)
    WHERE status = 'ACTIVE' AND next_poll_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS autopower_runs (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id              UUID        NOT NULL REFERENCES autopower_rules(id),
    user_id              UUID        NOT NULL REFERENCES users(id),
    meter_id             UUID        NOT NULL REFERENCES meters(id),
    trigger_source       TEXT        NOT NULL CHECK (trigger_source IN ('SCHEDULE', 'LOW_BALANCE')),
    scheduled_for        TIMESTAMPTZ NOT NULL,
    executed_at          TIMESTAMPTZ,
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    total_minor          BIGINT      NOT NULL DEFAULT 0 CHECK (total_minor >= 0),
    currency             CHAR(3)     NOT NULL DEFAULT 'NGN',
    status               TEXT        NOT NULL CHECK (status IN ('SCHEDULED', 'CHARGING', 'SUCCEEDED', 'FAILED', 'REFUNDED', 'SKIPPED_USER', 'SKIPPED_CAP', 'MISSED', 'CANCELLED')),
    heads_up_sent_at     TIMESTAMPTZ,
    transaction_id       UUID        REFERENCES electricity_transactions(id),
    payment_provider_ref TEXT        NOT NULL DEFAULT '',
    charge_dispatched_at TIMESTAMPTZ,
    failure_reason       TEXT        NOT NULL DEFAULT '',
    completed_at         TIMESTAMPTZ,
    lease_until          TIMESTAMPTZ NOT NULL DEFAULT TIMESTAMP WITH TIME ZONE 'epoch',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (rule_id, scheduled_for)
);

CREATE INDEX IF NOT EXISTS autopower_runs_rule_idx
    ON autopower_runs (rule_id, scheduled_for DESC);

CREATE INDEX IF NOT EXISTS autopower_runs_txn_idx
    ON autopower_runs (transaction_id)
    WHERE transaction_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS autopower_runs_due_idx
    ON autopower_runs (scheduled_for)
    WHERE status IN ('SCHEDULED', 'CHARGING');

CREATE INDEX IF NOT EXISTS autopower_runs_user_spend_idx
    ON autopower_runs (user_id, coalesce(executed_at, scheduled_for))
    WHERE status IN ('CHARGING', 'SUCCEEDED');
