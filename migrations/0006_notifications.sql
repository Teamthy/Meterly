-- 0006: M5 notification delivery, encrypted user contacts, preferences,
-- global opt-outs and WhatsApp 24-hour customer service windows.
-- Append-only migration; removes unused plaintext email/phone columns on users.

DROP INDEX IF EXISTS users_email_key;
DROP INDEX IF EXISTS users_phone_key;

ALTER TABLE users
    DROP COLUMN IF EXISTS email,
    DROP COLUMN IF EXISTS phone,
    ADD COLUMN IF NOT EXISTS email_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS email_hmac       TEXT,
    ADD COLUMN IF NOT EXISTS email_masked     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS email_key_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS phone_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS phone_hmac       TEXT,
    ADD COLUMN IF NOT EXISTS phone_masked     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS phone_key_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS display_name     TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS users_email_hmac_idx ON users (email_hmac) WHERE email_hmac IS NOT NULL;
CREATE INDEX IF NOT EXISTS users_phone_hmac_idx ON users (phone_hmac) WHERE phone_hmac IS NOT NULL;

CREATE TABLE IF NOT EXISTS notification_preferences (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel    TEXT        NOT NULL CHECK (channel IN ('SMS', 'EMAIL', 'WHATSAPP')),
    enabled    BOOLEAN     NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, channel)
);

-- Global opt-outs keyed by destination HMAC so STOP works for non-users (e.g. gift recipients).
CREATE TABLE IF NOT EXISTS opt_outs (
    destination_hmac TEXT        NOT NULL,
    channel          TEXT        NOT NULL CHECK (channel IN ('SMS', 'EMAIL', 'WHATSAPP')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (destination_hmac, channel)
);

-- Tracks verified inbound WhatsApp messages so free-form vs template rules can be enforced.
CREATE TABLE IF NOT EXISTS whatsapp_service_windows (
    destination_hmac TEXT        NOT NULL PRIMARY KEY,
    last_inbound_at  TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Durable notification job metadata; never stores rendered message body, token or raw destination.
CREATE TABLE IF NOT EXISTS notification_deliveries (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id         UUID        NOT NULL,
    transaction_id   UUID        NOT NULL REFERENCES electricity_transactions(id),
    recipient_kind   TEXT        NOT NULL CHECK (recipient_kind IN ('PURCHASER', 'GIFT_RECIPIENT')),
    channel          TEXT        NOT NULL CHECK (channel IN ('SMS', 'EMAIL', 'WHATSAPP')),
    destination_hmac TEXT        NOT NULL DEFAULT '',
    template_id      TEXT        NOT NULL,
    template_version TEXT        NOT NULL,
    locale           TEXT        NOT NULL DEFAULT 'en',
    status           TEXT        NOT NULL CHECK (status IN ('PENDING', 'SENDING', 'SENT', 'FAILED', 'UNKNOWN', 'SUPPRESSED')),
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider_name    TEXT        NOT NULL DEFAULT '',
    provider_ref     TEXT        NOT NULL DEFAULT '',
    last_error_code  TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at          TIMESTAMPTZ,
    UNIQUE (event_id, recipient_kind, channel)
);

CREATE INDEX IF NOT EXISTS notification_deliveries_due_idx
    ON notification_deliveries (next_attempt_at)
    WHERE status IN ('PENDING', 'UNKNOWN', 'SENDING');

CREATE INDEX IF NOT EXISTS notification_deliveries_txn_idx
    ON notification_deliveries (transaction_id, created_at);

CREATE INDEX IF NOT EXISTS notification_deliveries_retention_idx
    ON notification_deliveries (created_at)
    WHERE destination_hmac <> '' AND status IN ('SENT', 'FAILED', 'SUPPRESSED');
