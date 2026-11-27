-- 0026: NDPA subject-access export jobs (B2).
-- One job per user per UTC day. Archive and download token are encrypted at
-- rest; ciphertext is cleared on download or expiry. Counts/status only in
-- audit — never PII.

CREATE TABLE IF NOT EXISTS data_exports (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day                  DATE        NOT NULL,
    status               TEXT        NOT NULL CHECK (status IN ('PENDING', 'READY', 'DOWNLOADED', 'EXPIRED', 'FAILED')),
    archive_ciphertext   BYTEA,
    archive_key_version  TEXT        NOT NULL DEFAULT '',
    token_hmac           TEXT        NOT NULL DEFAULT '',
    token_ciphertext     BYTEA,
    token_key_version    TEXT        NOT NULL DEFAULT '',
    expires_at           TIMESTAMPTZ NOT NULL,
    downloaded_at        TIMESTAMPTZ,
    error                TEXT        NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, day)
);
CREATE INDEX IF NOT EXISTS data_exports_pending_idx
    ON data_exports (created_at)
    WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS data_exports_expiry_idx
    ON data_exports (expires_at)
    WHERE status IN ('PENDING', 'READY');
