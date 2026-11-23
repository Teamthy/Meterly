-- 0008: Allow token-free payment receipt / refund email deliveries to guest
-- Pay-for-Me payers (recipient_kind = 'PAYER').

ALTER TABLE notification_deliveries
    DROP CONSTRAINT IF EXISTS notification_deliveries_recipient_kind_check,
    ADD CONSTRAINT notification_deliveries_recipient_kind_check
        CHECK (recipient_kind IN ('PURCHASER', 'GIFT_RECIPIENT', 'PAYER'));
