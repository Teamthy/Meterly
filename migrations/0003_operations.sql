-- Indexes for operator search, the review queue and audit lookups.
CREATE INDEX electricity_transactions_review_idx ON electricity_transactions (created_at DESC) WHERE review_reason <> '';
CREATE INDEX electricity_transactions_meter_idx  ON electricity_transactions (meter_id, created_at DESC);
CREATE INDEX electricity_transactions_created_idx ON electricity_transactions (created_at DESC);
CREATE INDEX audit_events_resource_idx ON audit_events (resource_type, resource_id, id);
CREATE INDEX audit_events_actor_idx    ON audit_events (actor_user_id, created_at DESC);

-- Operator kill switch per provider (status already exists: ACTIVE/DISABLED).
ALTER TABLE providers ADD CONSTRAINT providers_status_check CHECK (status IN ('ACTIVE', 'DISABLED'));
