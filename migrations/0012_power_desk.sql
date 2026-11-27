-- M10: Landlord Power Desk (organisations, unit_meters, LANDLORD_BULK pots & approval rule).

-- Seed canonical ORGANISATION-scoped RBAC roles into existing roles table.
INSERT INTO roles (name, scope, description) VALUES
    ('OWNER',   'ORGANISATION', 'Organisation owner: full administrative, approval and billing control'),
    ('MANAGER', 'ORGANISATION', 'Organisation manager: properties, units, meter attachment and bulk top-ups'),
    ('VIEWER',  'ORGANISATION', 'Organisation viewer / accountant: read-only access and monthly statement export')
ON CONFLICT (scope, name) DO NOTHING;

-- Extend organisations with optional manager bulk-top-up approval threshold and updated_at.
ALTER TABLE organisations
    ADD COLUMN IF NOT EXISTS manager_approval_threshold_minor BIGINT
        CHECK (manager_approval_threshold_minor IS NULL OR manager_approval_threshold_minor >= 0),
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Extend units with created_at and race-safe uniqueness on buildings & unit names.
ALTER TABLE units
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE UNIQUE INDEX IF NOT EXISTS buildings_property_name_uq
    ON buildings (property_id, lower(name));

CREATE UNIQUE INDEX IF NOT EXISTS units_building_unit_name_uq
    ON units (building_id, lower(unit_name));

CREATE INDEX IF NOT EXISTS properties_org_created_idx
    ON properties (organisation_id, created_at ASC);

CREATE INDEX IF NOT EXISTS organisation_members_org_status_idx
    ON organisation_members (organisation_id, status);

-- Allow Funding Pots to start in PENDING_APPROVAL when a MANAGER's bulk top-up exceeds threshold.
ALTER TABLE pots DROP CONSTRAINT IF EXISTS pots_status_check;
ALTER TABLE pots ADD CONSTRAINT pots_status_check CHECK (
    status IN ('PENDING_APPROVAL', 'OPEN', 'FUNDED', 'ALLOCATING', 'SETTLED', 'EXPIRED', 'CANCELLED', 'REFUNDING', 'REFUNDED')
);

-- Unit-meter attachment and audited operator ownership verification.
CREATE TABLE IF NOT EXISTS unit_meters (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id     UUID        NOT NULL REFERENCES organisations(id),
    property_id         UUID        NOT NULL REFERENCES properties(id),
    unit_id             UUID        NOT NULL REFERENCES units(id),
    meter_id            UUID        NOT NULL REFERENCES meters(id),
    status              TEXT        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DETACHED')),
    verified            BOOLEAN     NOT NULL DEFAULT false,
    verification_method TEXT        NOT NULL DEFAULT '' CHECK (verification_method IN ('', 'ACCOUNT_NAME_MATCH', 'EVIDENCE_APPROVED')),
    evidence_reference  TEXT        NOT NULL DEFAULT '',
    disco_account_name  TEXT        NOT NULL DEFAULT '',
    verified_by_user_id UUID        REFERENCES users(id),
    verified_at         TIMESTAMPTZ,
    verification_reason TEXT        NOT NULL DEFAULT '',
    created_by_user_id  UUID        NOT NULL REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS unit_meters_active_unit_uq
    ON unit_meters (unit_id) WHERE status = 'ACTIVE';

CREATE INDEX IF NOT EXISTS unit_meters_org_status_idx
    ON unit_meters (organisation_id, status, created_at ASC);

-- LANDLORD_BULK Funding Pot metadata and per-unit allocation line items.
CREATE TABLE IF NOT EXISTS bulk_topups (
    pot_id                  UUID        PRIMARY KEY REFERENCES pots(id) ON DELETE CASCADE,
    organisation_id         UUID        NOT NULL REFERENCES organisations(id),
    created_by_user_id      UUID        NOT NULL REFERENCES users(id),
    creator_role            TEXT        NOT NULL CHECK (creator_role IN ('OWNER', 'MANAGER', 'VIEWER')),
    approval_status         TEXT        NOT NULL CHECK (approval_status IN ('NOT_REQUIRED', 'PENDING_APPROVAL', 'APPROVED')),
    approved_by_user_id     UUID        REFERENCES users(id),
    approved_at             TIMESTAMPTZ,
    contribution_id         UUID        REFERENCES pot_contributions(id),
    electricity_total_minor BIGINT      NOT NULL CHECK (electricity_total_minor > 0),
    fee_total_minor         BIGINT      NOT NULL CHECK (fee_total_minor >= 0),
    total_minor             BIGINT      NOT NULL CHECK (total_minor = electricity_total_minor + fee_total_minor),
    currency                CHAR(3)     NOT NULL,
    idempotency_key         TEXT        NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS bulk_topups_org_idempotency_uq
    ON bulk_topups (organisation_id, idempotency_key) WHERE idempotency_key <> '';

CREATE INDEX IF NOT EXISTS bulk_topups_org_created_idx
    ON bulk_topups (organisation_id, created_at DESC);

CREATE TABLE IF NOT EXISTS bulk_topup_items (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pot_id          UUID        NOT NULL REFERENCES bulk_topups(pot_id) ON DELETE CASCADE,
    organisation_id UUID        NOT NULL REFERENCES organisations(id),
    property_id     UUID        NOT NULL REFERENCES properties(id),
    unit_id         UUID        NOT NULL REFERENCES units(id),
    unit_meter_id   UUID        NOT NULL REFERENCES unit_meters(id),
    meter_id        UUID        NOT NULL REFERENCES meters(id),
    amount_minor    BIGINT      NOT NULL CHECK (amount_minor > 0),
    fee_minor       BIGINT      NOT NULL CHECK (fee_minor >= 0),
    total_minor     BIGINT      NOT NULL CHECK (total_minor = amount_minor + fee_minor),
    currency        CHAR(3)     NOT NULL,
    transaction_id  UUID        REFERENCES electricity_transactions(id),
    allocation_id   UUID        REFERENCES pot_allocations(id),
    failure_reason  TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (pot_id, unit_id)
);

CREATE INDEX IF NOT EXISTS bulk_topup_items_org_prop_idx
    ON bulk_topup_items (organisation_id, property_id, unit_id, created_at ASC);
