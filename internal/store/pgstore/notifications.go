package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
)

func (r userRepo) UpsertContacts(ctx context.Context, c notifications.UserContacts) error {
	uid, err := uuid.Parse(c.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	updAt := c.UpdatedAt.UTC()
	if c.UpdatedAt.IsZero() {
		updAt = time.Now().UTC()
	}
	hasEmail := len(c.EmailCiphertext) > 0
	hasPhone := len(c.PhoneCiphertext) > 0
	hasName := c.DisplayName != ""

	tag, err := r.db.Exec(ctx, `
		UPDATE users SET
			email_ciphertext  = CASE WHEN $2 THEN $3 ELSE email_ciphertext END,
			email_hmac        = CASE WHEN $2 THEN $4 ELSE email_hmac END,
			email_masked      = CASE WHEN $2 THEN $5 ELSE email_masked END,
			email_key_version = CASE WHEN $2 THEN $6 ELSE email_key_version END,
			phone_ciphertext  = CASE WHEN $7 THEN $8 ELSE phone_ciphertext END,
			phone_hmac        = CASE WHEN $7 THEN $9 ELSE phone_hmac END,
			phone_masked      = CASE WHEN $7 THEN $10 ELSE phone_masked END,
			phone_key_version = CASE WHEN $7 THEN $11 ELSE phone_key_version END,
			display_name      = CASE WHEN $12 THEN $13 ELSE display_name END,
			updated_at        = $14
		WHERE id = $1`,
		uid,
		hasEmail, c.EmailCiphertext, c.EmailHMAC, c.EmailMasked, c.EmailKeyVersion,
		hasPhone, c.PhoneCiphertext, c.PhoneHMAC, c.PhoneMasked, c.PhoneKeyVersion,
		hasName, c.DisplayName,
		updAt,
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r userRepo) Contacts(ctx context.Context, userID string) (notifications.UserContacts, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return notifications.UserContacts{}, store.ErrNotFound
	}
	var c notifications.UserContacts
	err = r.db.QueryRow(ctx, `
		SELECT id::text,
		       email_ciphertext, coalesce(email_hmac, ''), email_masked, email_key_version,
		       phone_ciphertext, coalesce(phone_hmac, ''), phone_masked, phone_key_version,
		       display_name, updated_at
		FROM users WHERE id = $1`, uid).Scan(
		&c.UserID,
		&c.EmailCiphertext, &c.EmailHMAC, &c.EmailMasked, &c.EmailKeyVersion,
		&c.PhoneCiphertext, &c.PhoneHMAC, &c.PhoneMasked, &c.PhoneKeyVersion,
		&c.DisplayName, &c.UpdatedAt,
	)
	if err != nil {
		return notifications.UserContacts{}, mapErr(err)
	}
	c.UpdatedAt = c.UpdatedAt.UTC()
	return c, nil
}

// ---- notifications ------------------------------------------------------------

type notifRepo struct{ *repos }

func isFKViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (r notifRepo) GetPreferences(ctx context.Context, userID string) (notifications.Preferences, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return notifications.Preferences{}, store.ErrNotFound
	}
	var exists bool
	if err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, uid).Scan(&exists); err != nil {
		return notifications.Preferences{}, mapErr(err)
	}
	if !exists {
		return notifications.Preferences{}, store.ErrNotFound
	}
	prefs := notifications.DefaultPreferences()
	rows, err := r.db.Query(ctx, `SELECT channel, enabled FROM notification_preferences WHERE user_id = $1`, uid)
	if err != nil {
		return notifications.Preferences{}, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var en bool
		if err := rows.Scan(&ch, &en); err != nil {
			return notifications.Preferences{}, err
		}
		switch notifications.Channel(ch) {
		case notifications.SMS:
			prefs.SMS = en
		case notifications.Email:
			prefs.Email = en
		case notifications.WhatsApp:
			prefs.WhatsApp = en
		}
	}
	return prefs, rows.Err()
}

func (r notifRepo) SetPreferences(ctx context.Context, userID string, p notifications.Preferences, now time.Time) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return r.atomic(ctx, func(db DB) error {
		entries := []struct {
			ch notifications.Channel
			en bool
		}{
			{notifications.SMS, p.SMS},
			{notifications.Email, p.Email},
			{notifications.WhatsApp, p.WhatsApp},
		}
		for _, e := range entries {
			_, err := db.Exec(ctx, `
				INSERT INTO notification_preferences (user_id, channel, enabled, updated_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_id, channel)
				DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`,
				uid, string(e.ch), e.en, now)
			if err != nil {
				if isFKViolation(err) {
					return store.ErrNotFound
				}
				return mapErr(err)
			}
		}
		return nil
	})
}

func (r notifRepo) AddOptOut(ctx context.Context, o notifications.OptOut) (bool, error) {
	if o.DestinationHMAC == "" || o.Channel == "" {
		return false, errors.New("notifications: opt-out requires destination_hmac and channel")
	}
	at := o.CreatedAt.UTC()
	if o.CreatedAt.IsZero() {
		at = time.Now().UTC()
	}
	tag, err := r.db.Exec(ctx, `
		INSERT INTO opt_outs (destination_hmac, channel, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (destination_hmac, channel) DO NOTHING`,
		o.DestinationHMAC, string(o.Channel), at)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r notifRepo) IsOptedOut(ctx context.Context, destinationHMAC string, ch notifications.Channel) (bool, error) {
	if destinationHMAC == "" {
		return false, nil
	}
	var exists bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM opt_outs WHERE destination_hmac = $1 AND channel = $2)`,
		destinationHMAC, string(ch)).Scan(&exists)
	return exists, mapErr(err)
}

func (r notifRepo) RecordWhatsAppInbound(ctx context.Context, destinationHMAC string, at time.Time) error {
	if destinationHMAC == "" {
		return errors.New("notifications: destination_hmac required")
	}
	ts := at.UTC()
	_, err := r.db.Exec(ctx, `
		INSERT INTO whatsapp_service_windows (destination_hmac, last_inbound_at, updated_at)
		VALUES ($1, $2, $2)
		ON CONFLICT (destination_hmac)
		DO UPDATE SET
			last_inbound_at = GREATEST(whatsapp_service_windows.last_inbound_at, EXCLUDED.last_inbound_at),
			updated_at      = EXCLUDED.updated_at`,
		destinationHMAC, ts)
	return mapErr(err)
}

func (r notifRepo) HasActiveWhatsAppWindow(ctx context.Context, destinationHMAC string, now time.Time) (bool, error) {
	if destinationHMAC == "" {
		return false, nil
	}
	var last time.Time
	err := r.db.QueryRow(ctx, `SELECT last_inbound_at FROM whatsapp_service_windows WHERE destination_hmac = $1`, destinationHMAC).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapErr(err)
	}
	age := now.UTC().Sub(last.UTC())
	return age >= 0 && age < 24*time.Hour, nil
}

const deliveryCols = `id, event_id, transaction_id, recipient_kind, recipient_slot, channel, destination_hmac,
	template_id, template_version, locale, status, attempts, next_attempt_at,
	provider_name, provider_ref, last_error_code, created_at, updated_at, sent_at`

func scanDelivery(row pgx.Row) (*notifications.Delivery, error) {
	var d notifications.Delivery
	var kind, ch, st string
	var sentAt *time.Time
	if err := row.Scan(
		&d.ID, &d.EventID, &d.TransactionID, &kind, &d.RecipientSlot, &ch, &d.DestinationHMAC,
		&d.TemplateID, &d.TemplateVersion, &d.Locale, &st, &d.Attempts, &d.NextAttemptAt,
		&d.ProviderName, &d.ProviderRef, &d.LastErrorCode, &d.CreatedAt, &d.UpdatedAt, &sentAt,
	); err != nil {
		return nil, mapErr(err)
	}
	d.RecipientKind = notifications.RecipientKind(kind)
	d.Channel = notifications.Channel(ch)
	d.Status = notifications.Status(st)
	d.NextAttemptAt = d.NextAttemptAt.UTC()
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if sentAt != nil {
		t := sentAt.UTC()
		d.SentAt = &t
	}
	return &d, nil
}

func (r notifRepo) CreateDelivery(ctx context.Context, d *notifications.Delivery) (bool, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	} else {
		d.CreatedAt = d.CreatedAt.UTC()
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = d.CreatedAt
	} else {
		d.UpdatedAt = d.UpdatedAt.UTC()
	}
	if d.NextAttemptAt.IsZero() {
		d.NextAttemptAt = d.CreatedAt
	} else {
		d.NextAttemptAt = d.NextAttemptAt.UTC()
	}
	if d.Locale == "" {
		d.Locale = notifications.LocaleEN
	}

	insertedRow, err := scanDelivery(r.db.QueryRow(ctx, `
		INSERT INTO notification_deliveries (
			id, event_id, transaction_id, recipient_kind, recipient_slot, channel, destination_hmac,
			template_id, template_version, locale, status, attempts, next_attempt_at,
			provider_name, provider_ref, last_error_code, created_at, updated_at, sent_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (event_id, recipient_kind, channel, recipient_slot) DO NOTHING
		RETURNING `+deliveryCols,
		d.ID, d.EventID, d.TransactionID, string(d.RecipientKind), d.RecipientSlot, string(d.Channel), d.DestinationHMAC,
		d.TemplateID, d.TemplateVersion, d.Locale, string(d.Status), d.Attempts, d.NextAttemptAt,
		d.ProviderName, d.ProviderRef, d.LastErrorCode, d.CreatedAt, d.UpdatedAt, d.SentAt,
	))
	if err == nil {
		*d = *insertedRow
		return true, nil
	}
	if isFKViolation(err) {
		return false, store.ErrNotFound
	}
	if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	existing, err := scanDelivery(r.db.QueryRow(ctx, `
		SELECT `+deliveryCols+` FROM notification_deliveries
		WHERE event_id = $1 AND recipient_kind = $2 AND channel = $3 AND recipient_slot = $4`,
		d.EventID, string(d.RecipientKind), string(d.Channel), d.RecipientSlot))
	if err != nil {
		return false, err
	}
	*d = *existing
	return false, nil
}

func (r notifRepo) GetDelivery(ctx context.Context, id uuid.UUID) (*notifications.Delivery, error) {
	return scanDelivery(r.db.QueryRow(ctx, `SELECT `+deliveryCols+` FROM notification_deliveries WHERE id = $1`, id))
}

func (r notifRepo) GetDeliveryForUpdate(ctx context.Context, id uuid.UUID) (*notifications.Delivery, error) {
	if !r.inTx {
		return nil, errors.New("delivery lock requires transaction")
	}
	return scanDelivery(r.db.QueryRow(ctx, `SELECT `+deliveryCols+` FROM notification_deliveries WHERE id = $1 FOR UPDATE`, id))
}

func (r notifRepo) UpdateDelivery(ctx context.Context, d *notifications.Delivery) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE notification_deliveries SET
			destination_hmac = $2,
			status           = $3,
			attempts         = $4,
			next_attempt_at  = $5,
			provider_name    = $6,
			provider_ref     = $7,
			last_error_code  = $8,
			updated_at       = $9,
			sent_at          = $10
		WHERE id = $1`,
		d.ID, d.DestinationHMAC, string(d.Status), d.Attempts, d.NextAttemptAt.UTC(),
		d.ProviderName, d.ProviderRef, d.LastErrorCode, d.UpdatedAt.UTC(), d.SentAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r notifRepo) ListDueDeliveries(ctx context.Context, now, staleSendingBefore time.Time, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id FROM notification_deliveries
		WHERE (status = 'PENDING' AND next_attempt_at <= $1)
		   OR (status = 'UNKNOWN' AND next_attempt_at <= $1
		       AND ((template_id <> $2 AND attempts < 2) OR provider_ref <> ''))
		   OR ($3::timestamptz IS NOT NULL AND status = 'SENDING' AND updated_at <= $3)
		ORDER BY next_attempt_at, id
		LIMIT $4`,
		now.UTC(), notifications.TemplateGiftTokenSMS, nullTime(staleSendingBefore), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (r notifRepo) ForTransaction(ctx context.Context, txnID uuid.UUID) ([]notifications.Delivery, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+deliveryCols+` FROM notification_deliveries
		WHERE transaction_id = $1 ORDER BY created_at, id`, txnID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []notifications.Delivery
	for rows.Next() {
		var d notifications.Delivery
		var kind, ch, st string
		var sentAt *time.Time
		if err := rows.Scan(
			&d.ID, &d.EventID, &d.TransactionID, &kind, &d.RecipientSlot, &ch, &d.DestinationHMAC,
			&d.TemplateID, &d.TemplateVersion, &d.Locale, &st, &d.Attempts, &d.NextAttemptAt,
			&d.ProviderName, &d.ProviderRef, &d.LastErrorCode, &d.CreatedAt, &d.UpdatedAt, &sentAt,
		); err != nil {
			return nil, err
		}
		d.RecipientKind = notifications.RecipientKind(kind)
		d.Channel = notifications.Channel(ch)
		d.Status = notifications.Status(st)
		d.NextAttemptAt = d.NextAttemptAt.UTC()
		d.CreatedAt = d.CreatedAt.UTC()
		d.UpdatedAt = d.UpdatedAt.UTC()
		if sentAt != nil {
			t := sentAt.UTC()
			d.SentAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r notifRepo) PurgeOldDestinationHMACs(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE notification_deliveries
		SET destination_hmac = '', updated_at = now()
		WHERE destination_hmac <> ''
		  AND status IN ('SENT', 'FAILED', 'SUPPRESSED')
		  AND created_at < $1`,
		cutoff.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}
