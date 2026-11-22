-- These records are held candidates, not sendable delivery jobs. No contact,
-- content, token, channel, or consent information is stored here.
CREATE TABLE notification_candidates (
    event_id UUID PRIMARY KEY REFERENCES outbox_events(id) ON DELETE RESTRICT,
    transaction_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index supports bounded backfill without scanning unrelated events.
CREATE INDEX outbox_notification_candidate_idx ON outbox_events (created_at, id)
 WHERE aggregate_type = 'electricity_transaction' AND event_type IN (
    'payment.confirmed', 'token.available', 'electricity.delivery.confirmed',
    'electricity.delivery.failed', 'electricity.delivery.unknown', 'electricity.refund.completed'
 );
