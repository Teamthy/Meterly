-- 0030: Partner platform part 3 (B6). Outbound partner webhooks.
-- Org-scoped webhook_endpoints in 0001_init cannot be reused (FK organisations).

CREATE TABLE IF NOT EXISTS partner_webhook_endpoints (
    id                      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    partner_id              UUID         NOT NULL REFERENCES partners(id),
    url                     TEXT         NOT NULL,
    description             TEXT         NOT NULL DEFAULT '',
    events                  TEXT[]       NOT NULL CHECK (cardinality(events) >= 1),
    secret_ciphertext       BYTEA        NOT NULL,
    secret_key_version      TEXT         NOT NULL DEFAULT '',
    prev_secret_ciphertext  BYTEA,
    prev_secret_key_version TEXT         NOT NULL DEFAULT '',
    overlap_until           TIMESTAMPTZ,
    include_token           BOOLEAN      NOT NULL DEFAULT false,
    enabled                 BOOLEAN      NOT NULL DEFAULT true,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS partner_webhook_endpoints_partner_idx
    ON partner_webhook_endpoints (partner_id, created_at DESC);

CREATE TABLE IF NOT EXISTS partner_webhook_deliveries (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id       UUID         NOT NULL REFERENCES partner_webhook_endpoints(id),
    partner_id        UUID         NOT NULL REFERENCES partners(id),
    event_id          UUID         NOT NULL,
    event_type        TEXT         NOT NULL,
    transaction_id    UUID         REFERENCES electricity_transactions(id),
    payload           JSONB        NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT         NOT NULL CHECK (status IN ('PENDING', 'DELIVERING', 'DELIVERED', 'FAILED', 'DEAD_LETTER')),
    attempt_count     INTEGER      NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    first_attempt_at  TIMESTAMPTZ,
    last_attempt_at   TIMESTAMPTZ,
    response_code     INTEGER      NOT NULL DEFAULT 0,
    error_class       TEXT         NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, event_id)
);
CREATE INDEX IF NOT EXISTS partner_webhook_deliveries_due_idx
    ON partner_webhook_deliveries (next_attempt_at, id)
    WHERE status IN ('PENDING', 'FAILED', 'DELIVERING');
CREATE INDEX IF NOT EXISTS partner_webhook_deliveries_endpoint_idx
    ON partner_webhook_deliveries (endpoint_id, created_at DESC);
