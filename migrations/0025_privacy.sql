-- 0025: NDPA consent ledger, legal holds, retention-run audit (B1).
-- Consent history is append-only. Current transactional state still lives
-- in notification_preferences; marketing defaults to withdrawn (opt-in).
-- Retention-run metadata stores counts only — never PII.

CREATE TABLE IF NOT EXISTS consents (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose     TEXT        NOT NULL CHECK (purpose IN ('TRANSACTIONAL', 'MARKETING')),
    channel     TEXT        NOT NULL CHECK (channel IN ('SMS', 'EMAIL', 'WHATSAPP')),
    status      TEXT        NOT NULL CHECK (status IN ('GRANTED', 'WITHDRAWN')),
    source      TEXT        NOT NULL DEFAULT 'user' CHECK (source IN ('user', 'default', 'inbound_stop', 'preferences')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS consents_user_idx
    ON consents (user_id, purpose, channel, created_at DESC);

CREATE TABLE IF NOT EXISTS legal_holds (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id),
    reason      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT        NOT NULL DEFAULT '',
    released_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS legal_holds_active_idx
    ON legal_holds (user_id)
    WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS retention_runs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    dry_run      BOOLEAN     NOT NULL DEFAULT false,
    status       TEXT        NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    counts       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    error        TEXT        NOT NULL DEFAULT ''
);
