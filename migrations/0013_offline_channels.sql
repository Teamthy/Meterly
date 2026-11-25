-- 0013: M11 Meterly Offline (USSD & WhatsApp purchase channels).
-- Append-only migration. Stores zero plaintext phone numbers, OTP codes,
-- raw meter numbers, or STS tokens.

CREATE TABLE IF NOT EXISTS phone_channel_links (
    phone_hmac     TEXT        PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status         TEXT        NOT NULL CHECK (status IN ('UNVERIFIED', 'VERIFIED', 'REVERIFY_REQUIRED')),
    verified_at    TIMESTAMPTZ,
    last_active_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS phone_channel_links_user_idx
    ON phone_channel_links (user_id);

CREATE TABLE IF NOT EXISTS channel_otps (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_hmac     TEXT        NOT NULL,
    target_user_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hmac      TEXT        NOT NULL,
    attempts       INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    consumed_at    TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS channel_otps_phone_created_idx
    ON channel_otps (phone_hmac, created_at DESC);

CREATE TABLE IF NOT EXISTS conversation_sessions (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    channel               TEXT        NOT NULL CHECK (channel IN ('USSD', 'WHATSAPP')),
    provider_session_id   TEXT        NOT NULL,
    phone_hmac            TEXT        NOT NULL,
    user_id               UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    state                 TEXT        NOT NULL CHECK (state IN (
        'MENU',
        'AWAIT_OTP',
        'AWAIT_METER_CHOICE',
        'AWAIT_DISCO',
        'AWAIT_METER_NUMBER',
        'AWAIT_AMOUNT',
        'AWAIT_CONFIRM',
        'COMPLETED',
        'CANCELLED',
        'EXPIRED'
    )),
    step                  INTEGER     NOT NULL DEFAULT 0 CHECK (step >= 0),
    meter_options         JSONB       NOT NULL DEFAULT '[]'::jsonb,
    selected_meter_id     UUID        REFERENCES meters(id),
    selected_meter_masked TEXT        NOT NULL DEFAULT '',
    selected_disco        TEXT        NOT NULL DEFAULT '',
    amount_minor          BIGINT      NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    fee_minor             BIGINT      NOT NULL DEFAULT 0 CHECK (fee_minor >= 0),
    total_minor           BIGINT      NOT NULL DEFAULT 0 CHECK (total_minor >= 0),
    post_otp_action       TEXT        NOT NULL DEFAULT '',
    last_raw_input        TEXT        NOT NULL DEFAULT '',
    last_reply            TEXT        NOT NULL DEFAULT '',
    has_cached_turn       BOOLEAN     NOT NULL DEFAULT FALSE,
    last_inbound_msg_ts   TIMESTAMPTZ NOT NULL DEFAULT 'epoch'::timestamptz,
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (channel, provider_session_id)
);

CREATE INDEX IF NOT EXISTS conversation_sessions_phone_active_idx
    ON conversation_sessions (channel, phone_hmac, updated_at DESC);

CREATE TABLE IF NOT EXISTS processed_channel_messages (
    channel    TEXT        NOT NULL CHECK (channel IN ('USSD', 'WHATSAPP')),
    message_id TEXT        NOT NULL,
    phone_hmac TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, message_id)
);

CREATE TABLE IF NOT EXISTS offline_purchases (
    transaction_id      UUID        PRIMARY KEY REFERENCES electricity_transactions(id),
    user_id             UUID        NOT NULL REFERENCES users(id),
    channel             TEXT        NOT NULL CHECK (channel IN ('USSD', 'WHATSAPP')),
    phone_hmac          TEXT        NOT NULL,
    provider_session_id TEXT        NOT NULL DEFAULT '',
    amount_minor        BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency            CHAR(3)     NOT NULL DEFAULT 'NGN',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS offline_purchases_phone_day_idx
    ON offline_purchases (phone_hmac, created_at);

CREATE TABLE IF NOT EXISTS offline_token_resends (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL REFERENCES users(id),
    transaction_id UUID        NOT NULL REFERENCES electricity_transactions(id),
    channel        TEXT        NOT NULL CHECK (channel IN ('USSD', 'WHATSAPP')),
    phone_hmac     TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS offline_token_resends_user_day_idx
    ON offline_token_resends (user_id, created_at);
