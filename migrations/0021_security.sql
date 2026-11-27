-- Customer security engine: session metadata, step-up, append-only events.
-- Events never store tokens, meter numbers, contacts or raw IPs.

ALTER TABLE auth_challenges DROP CONSTRAINT IF EXISTS auth_challenges_purpose_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_purpose_check
    CHECK (purpose IN ('register', 'login', 'step-up'));

ALTER TABLE auth_sessions
    ADD COLUMN IF NOT EXISTS ip_hmac TEXT,
    ADD COLUMN IF NOT EXISTS ua_hmac TEXT,
    ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS step_up_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS security_events (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action     TEXT NOT NULL,
    outcome    TEXT NOT NULL CHECK (outcome IN ('ALLOW', 'STEP_UP', 'DENY', 'RECORDED')),
    reason     TEXT NOT NULL DEFAULT '',
    session_id UUID,
    ip_hmac    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS security_events_user_idx ON security_events (user_id, created_at DESC);
