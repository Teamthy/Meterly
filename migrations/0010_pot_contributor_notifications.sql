-- Migration 0010: Allow multiple opted-in contributors on a settled funding pot
-- to receive distinct per-contributor notifications for the same event, using
-- an immutable non-PII recipient_slot (contribution UUID) unaffected by 365-day
-- destination_hmac retention purges.

ALTER TABLE notification_deliveries
    ADD COLUMN IF NOT EXISTS recipient_slot TEXT NOT NULL DEFAULT '';

ALTER TABLE notification_deliveries
    DROP CONSTRAINT IF EXISTS notification_deliveries_event_id_recipient_kind_channel_key,
    ADD CONSTRAINT notification_deliveries_event_kind_channel_slot_key
        UNIQUE (event_id, recipient_kind, channel, recipient_slot);
