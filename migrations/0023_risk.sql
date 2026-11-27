-- 0023: risk engine and Paystack chargebacks (P3).
-- Risk rules, decisions, observations, denylist and link pauses store
-- metadata and HMACs only — never raw emails, phones, IPs, meter numbers
-- or tokens. Dispute reserve/loss accounts are the P3 finance default.

INSERT INTO ledger_accounts (account_code, account_name, account_type, currency) VALUES
    ('DISPUTE_RESERVE', 'Dispute reserve', 'LIABILITY', 'NGN'),
    ('DISPUTE_LOSS',    'Dispute loss',    'EXPENSE',   'NGN')
ON CONFLICT (account_code) DO NOTHING;

CREATE TABLE IF NOT EXISTS risk_rules (
    code            TEXT PRIMARY KEY,
    enabled         BOOLEAN     NOT NULL DEFAULT true,
    action          TEXT        NOT NULL CHECK (action IN ('DENY', 'REVIEW')),
    window_seconds  INTEGER     NOT NULL DEFAULT 0 CHECK (window_seconds >= 0),
    threshold       BIGINT      NOT NULL DEFAULT 0 CHECK (threshold >= 0),
    params          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID        REFERENCES users(id)
);

INSERT INTO risk_rules (code, enabled, action, window_seconds, threshold, params) VALUES
    ('velocity_user',         true, 'DENY',   3600, 8,        '{}'),
    ('velocity_card',         true, 'REVIEW', 3600, 5,        '{}'),
    ('velocity_email',        true, 'DENY',   3600, 8,        '{}'),
    ('velocity_ip',           true, 'REVIEW', 3600, 20,       '{}'),
    ('velocity_meter',        true, 'REVIEW', 3600, 8,        '{}'),
    ('velocity_device',       true, 'REVIEW', 3600, 12,       '{}'),
    ('card_testing',          true, 'REVIEW', 3600, 3,        '{}'),
    ('first_purchase_cap',    true, 'DENY',   0,    5000000,  '{}'),
    ('amount_anomaly',        true, 'REVIEW', 0,    0,        '{"multiplier": 8, "min_history": 5}'),
    ('public_link_failures',  true, 'DENY',   900,  8,        '{}')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE IF NOT EXISTS risk_observations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    observed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind             TEXT        NOT NULL CHECK (kind IN ('checkout', 'payment_ok', 'payment_failed', 'card_seen')),
    surface          TEXT        NOT NULL DEFAULT '',
    user_id          UUID        REFERENCES users(id),
    meter_id         UUID        REFERENCES meters(id),
    link_id          UUID,
    transaction_id   UUID        REFERENCES electricity_transactions(id),
    email_hmac       TEXT        NOT NULL DEFAULT '',
    ip_hmac          TEXT        NOT NULL DEFAULT '',
    device_hmac      TEXT        NOT NULL DEFAULT '',
    phone_hmac       TEXT        NOT NULL DEFAULT '',
    card_signature   TEXT        NOT NULL DEFAULT '',
    amount_minor     BIGINT      NOT NULL DEFAULT 0 CHECK (amount_minor >= 0)
);
CREATE INDEX IF NOT EXISTS risk_observations_user_idx
    ON risk_observations (user_id, kind, observed_at DESC);
CREATE INDEX IF NOT EXISTS risk_observations_link_idx
    ON risk_observations (link_id, kind, observed_at DESC);
CREATE INDEX IF NOT EXISTS risk_observations_card_idx
    ON risk_observations (card_signature, observed_at DESC)
    WHERE card_signature <> '';
CREATE INDEX IF NOT EXISTS risk_observations_email_idx
    ON risk_observations (email_hmac, observed_at DESC)
    WHERE email_hmac <> '';
CREATE INDEX IF NOT EXISTS risk_observations_ip_idx
    ON risk_observations (ip_hmac, observed_at DESC)
    WHERE ip_hmac <> '';
CREATE INDEX IF NOT EXISTS risk_observations_meter_idx
    ON risk_observations (meter_id, observed_at DESC)
    WHERE meter_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS risk_observations_device_idx
    ON risk_observations (device_hmac, observed_at DESC)
    WHERE device_hmac <> '';

CREATE TABLE IF NOT EXISTS risk_decisions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    outcome          TEXT        NOT NULL CHECK (outcome IN ('ALLOW', 'REVIEW', 'DENY')),
    rule_code        TEXT        NOT NULL DEFAULT '',
    surface          TEXT        NOT NULL DEFAULT '',
    user_id          UUID        REFERENCES users(id),
    meter_id         UUID        REFERENCES meters(id),
    link_id          UUID,
    transaction_id   UUID        REFERENCES electricity_transactions(id),
    amount_minor     BIGINT      NOT NULL DEFAULT 0,
    email_hmac       TEXT        NOT NULL DEFAULT '',
    ip_hmac          TEXT        NOT NULL DEFAULT '',
    device_hmac      TEXT        NOT NULL DEFAULT '',
    card_signature   TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS risk_decisions_txn_idx
    ON risk_decisions (transaction_id, created_at DESC);
CREATE INDEX IF NOT EXISTS risk_decisions_user_idx
    ON risk_decisions (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS risk_denylist (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         TEXT        NOT NULL CHECK (kind IN ('CARD', 'EMAIL', 'PHONE', 'IP_RANGE')),
    value        TEXT        NOT NULL,
    reason       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   UUID        REFERENCES users(id),
    UNIQUE (kind, value)
);

CREATE TABLE IF NOT EXISTS risk_link_pauses (
    link_id     UUID PRIMARY KEY,
    rule_code   TEXT        NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    paused_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS disputes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    psp_dispute_id      TEXT        NOT NULL UNIQUE,
    transaction_id      UUID        REFERENCES electricity_transactions(id),
    payment_ref         TEXT        NOT NULL DEFAULT '',
    amount_minor        BIGINT      NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    currency            CHAR(3)     NOT NULL DEFAULT 'NGN',
    status              TEXT        NOT NULL CHECK (status IN (
        'OPEN', 'REMINDED', 'WON', 'LOST', 'ACCEPTED'
    )),
    psp_status          TEXT        NOT NULL DEFAULT '',
    resolution          TEXT        NOT NULL DEFAULT '',
    category            TEXT        NOT NULL DEFAULT '',
    due_at              TIMESTAMPTZ,
    reminded_at         TIMESTAMPTZ,
    resolved_at         TIMESTAMPTZ,
    evidence            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS disputes_txn_idx ON disputes (transaction_id);
CREATE INDEX IF NOT EXISTS disputes_open_idx ON disputes (status)
    WHERE status IN ('OPEN', 'REMINDED');
