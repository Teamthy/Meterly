-- 0028: Partner platform part 1 (B4). Identity and access only — no purchases.
-- Keys: prefix + HMAC hash. The plaintext secret is never stored.

CREATE TABLE IF NOT EXISTS partners (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation             TEXT        NOT NULL,
    status                   TEXT        NOT NULL CHECK (status IN ('PENDING', 'ACTIVE', 'SUSPENDED')),
    environment              TEXT        NOT NULL CHECK (environment IN ('SANDBOX', 'PRODUCTION')),
    contact_ref              TEXT        NOT NULL DEFAULT '',
    daily_value_cap_minor    BIGINT      NOT NULL DEFAULT 0 CHECK (daily_value_cap_minor >= 0),
    rate_limit_per_minute    INT         NOT NULL DEFAULT 60 CHECK (rate_limit_per_minute >= 1 AND rate_limit_per_minute <= 10000),
    created_by               UUID        NOT NULL REFERENCES users(id),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    proposed_activation_by   UUID        REFERENCES users(id),
    proposed_activation_at   TIMESTAMPTZ,
    activated_by             UUID        REFERENCES users(id),
    activated_at             TIMESTAMPTZ,
    suspended_by             UUID        REFERENCES users(id),
    suspended_at             TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS partners_status_idx ON partners (status);
CREATE INDEX IF NOT EXISTS partners_environment_idx ON partners (environment);

CREATE TABLE IF NOT EXISTS partner_api_keys (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    partner_id    UUID        NOT NULL REFERENCES partners(id),
    prefix        TEXT        NOT NULL UNIQUE,
    secret_hash   TEXT        NOT NULL UNIQUE,
    environment   TEXT        NOT NULL CHECK (environment IN ('SANDBOX', 'PRODUCTION')),
    scopes        TEXT[]      NOT NULL CHECK (cardinality(scopes) >= 1),
    created_by    UUID        NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    revoked_by    UUID        REFERENCES users(id),
    overlap_until TIMESTAMPTZ,
    rotated_from  UUID        REFERENCES partner_api_keys(id)
);
CREATE INDEX IF NOT EXISTS partner_api_keys_partner_idx ON partner_api_keys (partner_id);

CREATE TABLE IF NOT EXISTS partner_usage (
    partner_id     UUID   NOT NULL REFERENCES partners(id),
    day            DATE   NOT NULL,
    request_count  BIGINT NOT NULL DEFAULT 0 CHECK (request_count >= 0),
    value_minor    BIGINT NOT NULL DEFAULT 0 CHECK (value_minor >= 0),
    PRIMARY KEY (partner_id, day)
);
