-- Quote pricing snapshot (DISCO tariff + pack) so confirmation/checkout can
-- be served from stored values, and idempotent low-unit alert cooldown.

ALTER TABLE quotes
    ADD COLUMN IF NOT EXISTS pack_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tariff_band TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS kobo_per_kwh BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS estimated_units_milli BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS credit_minor BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS discount_minor BIGINT NOT NULL DEFAULT 0 CHECK (discount_minor >= 0);

CREATE TABLE IF NOT EXISTS meter_low_unit_alerts (
    meter_id UUID NOT NULL REFERENCES meters(id),
    user_id UUID NOT NULL REFERENCES users(id),
    last_notified_at TIMESTAMPTZ NOT NULL,
    last_remaining_milli_kwh BIGINT NOT NULL,
    PRIMARY KEY (meter_id, user_id)
);
