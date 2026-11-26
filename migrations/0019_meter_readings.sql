-- Observed remote kWh samples. Consumption rate and time-left are computed
-- from these rows in the API; the web app must not invent them.

CREATE TABLE IF NOT EXISTS meter_reading_samples (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    meter_id   UUID NOT NULL REFERENCES meters(id) ON DELETE CASCADE,
    milli_kwh  BIGINT NOT NULL CHECK (milli_kwh >= 0),
    read_at    TIMESTAMPTZ NOT NULL,
    provider   TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (meter_id, read_at)
);

CREATE INDEX IF NOT EXISTS meter_reading_samples_meter_time_idx
    ON meter_reading_samples (meter_id, read_at DESC);
