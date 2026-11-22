-- Records the encryption key version and purchaser's notification request.
-- This flag does not establish recipient consent and must not trigger sending.
ALTER TABLE gift_transactions
    ADD COLUMN contact_key_version TEXT,
    ADD COLUMN notification_requested BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE gift_transactions ADD CONSTRAINT gift_contact_metadata_check CHECK (
    (recipient_contact_encrypted IS NULL AND contact_key_version IS NULL AND notification_requested = false)
    OR (recipient_contact_encrypted IS NOT NULL AND octet_length(recipient_contact_encrypted) > 0
        AND contact_key_version IS NOT NULL AND length(contact_key_version) > 0 AND notification_requested = true)
) NOT VALID;
