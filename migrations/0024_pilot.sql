-- 0024: pilot allow-list (P4). PILOT_MODE=true restricts purchases to
-- listed users and meters, and (when any DISCO row exists) listed DisCos.
-- Caps live in process config. Every mutation is audited in audit_events.

CREATE TABLE IF NOT EXISTS pilot_allowlist (
    kind        TEXT        NOT NULL CHECK (kind IN ('USER', 'METER', 'DISCO')),
    value       TEXT        NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (kind, value)
);
CREATE INDEX IF NOT EXISTS pilot_allowlist_kind_idx ON pilot_allowlist (kind);
