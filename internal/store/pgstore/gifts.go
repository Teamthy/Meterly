package pgstore

import (
	"context"
	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/store"
)

type giftRepo struct{ *repos }

func (r giftRepo) Create(ctx context.Context, g gifts.Gift) error {
	tag, err := r.db.Exec(ctx, `INSERT INTO gift_transactions (transaction_id,purchaser_user_id,recipient_name,recipient_contact_encrypted,contact_key_version,notification_requested,created_at)
 VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,''),$6,$7)
 ON CONFLICT (transaction_id) DO UPDATE SET recipient_contact_encrypted=EXCLUDED.recipient_contact_encrypted,
 contact_key_version=EXCLUDED.contact_key_version,notification_requested=EXCLUDED.notification_requested
 WHERE gift_transactions.purchaser_user_id=EXCLUDED.purchaser_user_id AND gift_transactions.recipient_contact_encrypted IS NULL`, g.TransactionID, g.PurchaserID, g.RecipientName, nilIfEmpty(g.ContactCiphertext), g.ContactKeyVersion, g.NotificationRequested, g.CreatedAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return store.ErrConflict
	}
	return nil
}
func (r giftRepo) Get(ctx context.Context, id uuid.UUID) (gifts.Gift, error) {
	var g gifts.Gift
	err := r.db.QueryRow(ctx, `SELECT transaction_id,purchaser_user_id::text,coalesce(recipient_name,''),recipient_contact_encrypted,coalesce(contact_key_version,''),notification_requested,created_at FROM gift_transactions WHERE transaction_id=$1`, id).Scan(&g.TransactionID, &g.PurchaserID, &g.RecipientName, &g.ContactCiphertext, &g.ContactKeyVersion, &g.NotificationRequested, &g.CreatedAt)
	return g, mapErr(err)
}
