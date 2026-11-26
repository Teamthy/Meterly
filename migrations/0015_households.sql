-- Verified ownership and household membership (FR-008).
-- The 0001 households / household_members tables are extended; we never treat
-- legacy self-registered MANAGE grants as ownership.

ALTER TABLE households
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE household_members
    ADD COLUMN IF NOT EXISTS invited_by UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS invited_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS accepted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE household_members DROP CONSTRAINT IF EXISTS household_members_status_check;
ALTER TABLE household_members
    ADD CONSTRAINT household_members_status_check
    CHECK (status IN ('INVITED','ACTIVE','DECLINED','REVOKED'));

CREATE TABLE IF NOT EXISTS household_meters (
    household_id UUID NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    meter_id     UUID NOT NULL REFERENCES meters(id),
    added_by     UUID NOT NULL REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (household_id, meter_id)
);

CREATE TABLE IF NOT EXISTS meter_ownership (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    meter_id     UUID NOT NULL REFERENCES meters(id),
    user_id      UUID NOT NULL REFERENCES users(id),
    method       TEXT NOT NULL CHECK (method IN ('ACCOUNT_NAME_MATCH','OPERATOR_ATTESTATION')),
    reason       TEXT NOT NULL DEFAULT '',
    verified_by  UUID NOT NULL REFERENCES users(id),
    verified_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    UNIQUE (meter_id, user_id)
);

CREATE INDEX IF NOT EXISTS meter_ownership_active_idx
    ON meter_ownership (meter_id) WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS household_members_user_idx
    ON household_members (user_id) WHERE status IN ('INVITED','ACTIVE');
