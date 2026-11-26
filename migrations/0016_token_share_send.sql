-- 0016: purchaser/share token send (FR-007). token_shares already stores
-- metadata only; we add bound destination ciphertext (never the STS token)
-- and allow TOKEN_SHARE notification_deliveries.

ALTER TABLE token_shares
    ADD COLUMN IF NOT EXISTS destination_ciphertext BYTEA,
    ADD COLUMN IF NOT EXISTS destination_key_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS destination_masked TEXT NOT NULL DEFAULT '';

ALTER TABLE notification_deliveries
    DROP CONSTRAINT IF EXISTS notification_deliveries_recipient_kind_check,
    ADD CONSTRAINT notification_deliveries_recipient_kind_check
        CHECK (recipient_kind IN ('PURCHASER', 'GIFT_RECIPIENT', 'PAYER', 'POT_CONTRIBUTOR', 'TOKEN_SHARE'));

CREATE INDEX IF NOT EXISTS token_shares_sender_day_idx
    ON token_shares (shared_by_user_id, shared_at DESC)
    WHERE channel <> 'COPY';
