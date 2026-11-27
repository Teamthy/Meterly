-- 0027: NDPA account erasure (B3). Cooling-off requests and a tombstone user
-- for financial FKs. Identity is wiped; books are re-pointed, never deleted.
-- B4 partners uses 0028_*.sql.

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('ACTIVE', 'SUSPENDED', 'CLOSED', 'ERASED'));

INSERT INTO users (id, external_id, status, display_name, created_at, updated_at)
VALUES (
    '00000000-0000-0000-0000-00000000e5ed',
    'tombstone:erasure',
    'ERASED',
    '',
    now(),
    now()
)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS erasure_requests (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id),
    status        TEXT        NOT NULL CHECK (status IN (
        'COOLING_OFF', 'CANCELLED', 'READY', 'RUNNING', 'COMPLETED', 'BLOCKED', 'FAILED'
    )),
    due_at        TIMESTAMPTZ NOT NULL,
    cancelled_at  TIMESTAMPTZ,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    step          TEXT        NOT NULL DEFAULT '',
    error         TEXT        NOT NULL DEFAULT '',
    tombstone_id  UUID        REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS erasure_requests_open_user_idx
    ON erasure_requests (user_id)
    WHERE status IN ('COOLING_OFF', 'READY', 'RUNNING', 'BLOCKED', 'FAILED');
CREATE INDEX IF NOT EXISTS erasure_requests_due_idx
    ON erasure_requests (due_at)
    WHERE status IN ('COOLING_OFF', 'READY', 'RUNNING', 'BLOCKED', 'FAILED');
