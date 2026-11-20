-- Meterly core schema (spec §5). PostgreSQL 15+.
-- Money is BIGINT minor units (kobo). Timestamps are TIMESTAMPTZ (UTC).
-- Meter numbers and tokens are stored encrypted; lookups use keyed HMACs.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto; -- gen_random_uuid()

-- ---------------------------------------------------------------- identity
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id   TEXT UNIQUE,                 -- OIDC subject
    email         TEXT,
    phone         TEXT,
    first_name    TEXT,
    last_name     TEXT,
    status        TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','SUSPENDED','CLOSED')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_uq ON users (lower(email)) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_phone_uq ON users (phone) WHERE phone IS NOT NULL;

CREATE TABLE organisations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                TEXT NOT NULL,
    legal_name          TEXT,
    registration_number TEXT,
    organisation_type   TEXT NOT NULL CHECK (organisation_type IN ('ESTATE','BUSINESS','ENTERPRISE','LANDLORD')),
    verification_status TEXT NOT NULL DEFAULT 'PENDING',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    scope       TEXT NOT NULL CHECK (scope IN ('SYSTEM','ORGANISATION','RESOURCE')),
    description TEXT,
    UNIQUE (scope, name)
);

CREATE TABLE permissions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key         TEXT NOT NULL UNIQUE,
    description TEXT
);

CREATE TABLE role_permissions (
    role_id       UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id UUID NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE organisation_members (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id UUID NOT NULL REFERENCES organisations(id),
    user_id         UUID NOT NULL REFERENCES users(id),
    role_id         UUID NOT NULL REFERENCES roles(id),
    status          TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organisation_id, user_id)
);

-- ---------------------------------------------------------------- property
CREATE TABLE properties (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id UUID REFERENCES organisations(id),
    owner_user_id   UUID REFERENCES users(id),
    name            TEXT NOT NULL,
    address         TEXT,
    city            TEXT,
    state           TEXT,
    country         CHAR(2) NOT NULL DEFAULT 'NG',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (organisation_id IS NOT NULL OR owner_user_id IS NOT NULL)
);

CREATE TABLE buildings (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id),
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE units (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    building_id      UUID NOT NULL REFERENCES buildings(id),
    unit_name        TEXT NOT NULL,
    occupancy_status TEXT
);

-- ---------------------------------------------------------------- meters
CREATE TABLE distribution_companies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    code          TEXT NOT NULL UNIQUE,
    market_status TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE meter_manufacturers (
    id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name    TEXT NOT NULL UNIQUE,
    country TEXT
);

CREATE TABLE meter_models (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    manufacturer_id UUID NOT NULL REFERENCES meter_manufacturers(id),
    model_name      TEXT NOT NULL,
    meter_type      TEXT,
    protocol        TEXT,
    UNIQUE (manufacturer_id, model_name)
);

CREATE TABLE meters (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    meter_number_encrypted BYTEA NOT NULL,
    meter_number_hash      TEXT  NOT NULL UNIQUE,   -- HMAC(disco:number)
    meter_number_masked    TEXT  NOT NULL,
    key_version            TEXT  NOT NULL,
    disco_id               UUID NOT NULL REFERENCES distribution_companies(id),
    meter_model_id         UUID REFERENCES meter_models(id),
    property_id            UUID REFERENCES properties(id),
    unit_id                UUID REFERENCES units(id),
    meter_type             TEXT,
    validation_status      TEXT NOT NULL DEFAULT 'UNVERIFIED',
    validated_at           TIMESTAMPTZ,
    status                 TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE meter_access_grants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    meter_id   UUID NOT NULL REFERENCES meters(id),
    user_id    UUID NOT NULL REFERENCES users(id),
    permission TEXT NOT NULL CHECK (permission IN ('VIEW','PURCHASE','MANAGE')),
    label      TEXT,
    granted_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX meter_access_active_uq ON meter_access_grants (meter_id, user_id) WHERE revoked_at IS NULL;

CREATE TABLE providers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    provider_type TEXT NOT NULL CHECK (provider_type IN ('PAYMENT','VENDING','AMI','NOTIFICATION')),
    status        TEXT NOT NULL DEFAULT 'ACTIVE',
    environment   TEXT NOT NULL CHECK (environment IN ('SANDBOX','PRODUCTION')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, environment)
);

CREATE TABLE provider_integrations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id       UUID NOT NULL REFERENCES providers(id),
    integration_name  TEXT NOT NULL,
    configuration_ref TEXT NOT NULL,   -- secrets-manager reference, never the secret
    status            TEXT NOT NULL DEFAULT 'ACTIVE',
    timeout_ms        INTEGER NOT NULL DEFAULT 20000,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE meter_capabilities (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    meter_id         UUID NOT NULL REFERENCES meters(id),
    capability       TEXT NOT NULL CHECK (capability IN ('TOKEN_VENDING','REMOTE_CREDIT','REMOTE_READING','BALANCE_MONITORING')),
    supported        BOOLEAN NOT NULL,
    authorised       BOOLEAN NOT NULL DEFAULT false,
    reachable        BOOLEAN,           -- NULL = not observable
    confirmed        BOOLEAN NOT NULL DEFAULT false,
    provider_id      UUID REFERENCES providers(id),
    confidence       TEXT,
    last_verified_at TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    UNIQUE (meter_id, capability, provider_id)
);

-- ---------------------------------------------------------------- beneficiaries & households
CREATE TABLE beneficiaries (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id        UUID NOT NULL REFERENCES users(id),
    meter_id             UUID NOT NULL REFERENCES meters(id),
    display_name         TEXT NOT NULL,
    relationship         TEXT,
    notification_channel TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_user_id, meter_id)
);

CREATE TABLE households (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID NOT NULL REFERENCES users(id),
    name          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE household_members (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households(id),
    user_id      UUID NOT NULL REFERENCES users(id),
    role         TEXT NOT NULL CHECK (role IN ('OWNER','ADMINISTRATOR','MEMBER','VIEWER','RECIPIENT')),
    status       TEXT NOT NULL DEFAULT 'INVITED',
    UNIQUE (household_id, user_id)
);

-- ---------------------------------------------------------------- transactions
CREATE TABLE quotes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     UUID NOT NULL REFERENCES users(id),
    meter_id        UUID NOT NULL REFERENCES meters(id),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    fee_minor       BIGINT NOT NULL CHECK (fee_minor >= 0),
    total_minor     BIGINT NOT NULL CHECK (total_minor = amount_minor + fee_minor),
    currency        CHAR(3) NOT NULL DEFAULT 'NGN',
    delivery_method TEXT NOT NULL CHECK (delivery_method IN ('TOKEN','REMOTE')),
    provider_id     UUID REFERENCES providers(id),
    expires_at      TIMESTAMPTZ NOT NULL,
    status          TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','USED','EXPIRED'))
);

CREATE TABLE electricity_transactions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     UUID NOT NULL REFERENCES users(id),
    meter_id        UUID NOT NULL REFERENCES meters(id),
    beneficiary_id  UUID REFERENCES beneficiaries(id),
    quote_id        UUID NOT NULL UNIQUE REFERENCES quotes(id),   -- one transaction per quote
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    fee_minor       BIGINT NOT NULL CHECK (fee_minor >= 0),
    total_minor     BIGINT NOT NULL CHECK (total_minor = amount_minor + fee_minor),
    currency        CHAR(3) NOT NULL DEFAULT 'NGN',
    payment_status  TEXT NOT NULL CHECK (payment_status IN ('CREATED','PENDING','CONFIRMED','FAILED','REFUND_PENDING','REFUNDED')),
    vending_status  TEXT NOT NULL CHECK (vending_status IN ('NOT_STARTED','PENDING','CONFIRMED','FAILED','UNKNOWN')),
    delivery_status TEXT NOT NULL CHECK (delivery_status IN ('NOT_STARTED','PENDING','ACCEPTED','CONFIRMED','FAILED','UNKNOWN')),
    delivery_method TEXT NOT NULL CHECK (delivery_method IN ('TOKEN','REMOTE')),
    is_gift         BOOLEAN NOT NULL DEFAULT false,
    version         INTEGER NOT NULL DEFAULT 1,     -- optimistic concurrency
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Vending can only start after payment is confirmed.
    CHECK (vending_status = 'NOT_STARTED' OR payment_status IN ('CONFIRMED','REFUND_PENDING','REFUNDED')),
    -- Delivery can only start after vending is confirmed.
    CHECK (delivery_status = 'NOT_STARTED' OR vending_status = 'CONFIRMED')
);
CREATE INDEX electricity_transactions_customer_idx ON electricity_transactions (customer_id, created_at DESC);
CREATE INDEX electricity_transactions_recon_idx ON electricity_transactions (id)
    WHERE vending_status = 'UNKNOWN' OR delivery_status IN ('UNKNOWN','ACCEPTED');

CREATE TABLE transaction_state_events (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id UUID NOT NULL REFERENCES electricity_transactions(id),
    dimension      TEXT NOT NULL CHECK (dimension IN ('payment','vending','delivery')),
    from_state     TEXT NOT NULL,
    to_state       TEXT NOT NULL,
    note           TEXT,
    actor          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE idempotency_keys (
    scope        TEXT NOT NULL,
    operation    TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    result_ref   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, operation, key)
);

CREATE TABLE payments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id     UUID NOT NULL REFERENCES electricity_transactions(id),
    provider_id        UUID NOT NULL REFERENCES providers(id),
    provider_reference TEXT NOT NULL,
    amount_minor       BIGINT NOT NULL,
    currency           CHAR(3) NOT NULL,
    status             TEXT NOT NULL,
    verified_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, provider_reference)
);

CREATE TABLE vending_records (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id     UUID NOT NULL REFERENCES electricity_transactions(id),
    provider_id        UUID NOT NULL REFERENCES providers(id),
    idempotency_key    TEXT NOT NULL UNIQUE,
    provider_reference TEXT,
    status             TEXT NOT NULL,
    token_ciphertext   BYTEA,
    token_key_version  TEXT,
    units_kwh          NUMERIC(14,3),
    confirmed_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, provider_reference)
);

CREATE TABLE delivery_records (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id     UUID NOT NULL REFERENCES electricity_transactions(id),
    provider_id        UUID REFERENCES providers(id),
    idempotency_key    TEXT NOT NULL UNIQUE,
    provider_reference TEXT,
    delivery_method    TEXT NOT NULL CHECK (delivery_method IN ('TOKEN','REMOTE')),
    status             TEXT NOT NULL,
    accepted_at        TIMESTAMPTZ,
    confirmed_at       TIMESTAMPTZ,
    failure_code       TEXT,
    failure_message    TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refunds (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id     UUID NOT NULL REFERENCES electricity_transactions(id),
    payment_id         UUID NOT NULL REFERENCES payments(id),
    amount_minor       BIGINT NOT NULL CHECK (amount_minor > 0),
    reason             TEXT NOT NULL,
    status             TEXT NOT NULL,
    provider_reference TEXT,
    approved_by        UUID REFERENCES users(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE gift_transactions (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id              UUID NOT NULL UNIQUE REFERENCES electricity_transactions(id),
    purchaser_user_id           UUID NOT NULL REFERENCES users(id),
    recipient_name              TEXT,
    recipient_contact_encrypted BYTEA,
    recipient_user_id           UUID REFERENCES users(id),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Never copy raw tokens into the share/delivery tables.
CREATE TABLE token_shares (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vending_record_id        UUID NOT NULL REFERENCES vending_records(id),
    shared_by_user_id        UUID NOT NULL REFERENCES users(id),
    channel                  TEXT NOT NULL,
    recipient_reference_hash TEXT,
    shared_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at               TIMESTAMPTZ
);

CREATE TABLE token_delivery_attempts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vending_record_id  UUID NOT NULL REFERENCES vending_records(id),
    channel            TEXT NOT NULL,
    destination_hash   TEXT NOT NULL,
    status             TEXT NOT NULL,
    provider_reference TEXT,
    attempted_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- webhooks
CREATE TABLE webhook_endpoints (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id UUID NOT NULL REFERENCES organisations(id),
    endpoint_url    TEXT NOT NULL,
    secret_ref      TEXT NOT NULL,
    events          TEXT[] NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id     UUID NOT NULL REFERENCES webhook_endpoints(id),
    event_id        UUID NOT NULL,
    status          TEXT NOT NULL,
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    response_code   INTEGER,
    UNIQUE (endpoint_id, event_id)
);

-- ---------------------------------------------------------------- ledger
CREATE TABLE ledger_accounts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_code TEXT NOT NULL UNIQUE,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL CHECK (account_type IN ('ASSET','LIABILITY','INCOME','EXPENSE','EQUITY')),
    currency     CHAR(3) NOT NULL
);

CREATE TABLE ledger_transactions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reference   TEXT NOT NULL UNIQUE,     -- makes posting idempotent
    source_type TEXT NOT NULL,
    source_id   UUID NOT NULL,
    description TEXT,
    posted_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ledger_transaction_id UUID NOT NULL REFERENCES ledger_transactions(id),
    ledger_account_id     UUID NOT NULL REFERENCES ledger_accounts(id),
    direction             TEXT NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount_minor          BIGINT NOT NULL CHECK (amount_minor > 0),
    currency              CHAR(3) NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (ledger_account_id);

-- Every ledger transaction must balance per currency, checked at commit.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger AS $$
DECLARE
    bad RECORD;
BEGIN
    SELECT currency, SUM(CASE direction WHEN 'DEBIT' THEN amount_minor ELSE -amount_minor END) AS net
      INTO bad
      FROM ledger_entries
     WHERE ledger_transaction_id = NEW.ledger_transaction_id
     GROUP BY currency
    HAVING SUM(CASE direction WHEN 'DEBIT' THEN amount_minor ELSE -amount_minor END) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'ledger transaction % does not balance (% off by %)', NEW.ledger_transaction_id, bad.currency, bad.net;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

-- Posted entries are immutable: corrections use reversal postings.
CREATE FUNCTION ledger_forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger entries are append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();

INSERT INTO ledger_accounts (account_code, account_name, account_type, currency) VALUES
    ('PSP_SETTLEMENT_RECEIVABLE', 'PSP settlement receivable', 'ASSET',     'NGN'),
    ('CUSTOMER_FUNDS_CLEARING',   'Customer funds clearing',   'LIABILITY', 'NGN'),
    ('PROVIDER_PAYABLE',          'Provider payable',          'LIABILITY', 'NGN'),
    ('REFUND_PAYABLE',            'Refund payable',            'LIABILITY', 'NGN'),
    ('PLATFORM_FEE_REVENUE',      'Platform fee revenue',      'INCOME',    'NGN');

-- ---------------------------------------------------------------- audit & outbox
CREATE TABLE audit_events (
    id                BIGSERIAL PRIMARY KEY,
    actor_user_id     UUID REFERENCES users(id),
    actor_type        TEXT NOT NULL CHECK (actor_type IN ('USER','SYSTEM','PROVIDER','OPERATOR')),
    action            TEXT NOT NULL,
    resource_type     TEXT NOT NULL,
    resource_id       UUID,
    correlation_id    UUID,
    metadata_redacted JSONB NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE outbox_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type TEXT NOT NULL,
    aggregate_id   UUID NOT NULL,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX outbox_unpublished_idx ON outbox_events (created_at) WHERE published_at IS NULL;

COMMIT;
