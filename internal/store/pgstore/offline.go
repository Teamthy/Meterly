package pgstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/store"
)

type offlineRepo struct{ *repos }

func (r offlineRepo) FindUserIDByPhoneHMAC(ctx context.Context, phoneHMAC string) (string, error) {
	if phoneHMAC == "" {
		return "", store.ErrNotFound
	}
	var uid string
	err := r.db.QueryRow(ctx, `
		SELECT id::text
		FROM users
		WHERE phone_hmac = $1
		ORDER BY created_at ASC, id ASC
		LIMIT 1`, phoneHMAC).Scan(&uid)
	if err != nil {
		return "", mapErr(err)
	}
	return uid, nil
}

func (r offlineRepo) GetPhoneLink(ctx context.Context, phoneHMAC string) (*conversation.PhoneLink, error) {
	return r.getPhoneLink(ctx, phoneHMAC, false)
}

func (r offlineRepo) GetPhoneLinkForUpdate(ctx context.Context, phoneHMAC string) (*conversation.PhoneLink, error) {
	return r.getPhoneLink(ctx, phoneHMAC, true)
}

func (r offlineRepo) getPhoneLink(ctx context.Context, phoneHMAC string, lock bool) (*conversation.PhoneLink, error) {
	q := `SELECT phone_hmac, user_id::text, status, verified_at, last_active_at, created_at, updated_at
		FROM phone_channel_links WHERE phone_hmac = $1`
	if lock {
		q += " FOR UPDATE"
	}
	var (
		l      conversation.PhoneLink
		status string
	)
	err := r.db.QueryRow(ctx, q, phoneHMAC).Scan(
		&l.PhoneHMAC, &l.UserID, &status, &l.VerifiedAt, &l.LastActiveAt, &l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	l.Status = conversation.PhoneLinkStatus(status)
	return &l, nil
}

func (r offlineRepo) UpsertPhoneLink(ctx context.Context, l *conversation.PhoneLink) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO phone_channel_links (phone_hmac, user_id, status, verified_at, last_active_at, created_at, updated_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT (phone_hmac) DO UPDATE SET
			user_id        = EXCLUDED.user_id,
			status         = EXCLUDED.status,
			verified_at    = EXCLUDED.verified_at,
			last_active_at = EXCLUDED.last_active_at,
			updated_at     = EXCLUDED.updated_at`,
		l.PhoneHMAC, l.UserID, string(l.Status), l.VerifiedAt,
		l.LastActiveAt.UTC(), l.CreatedAt.UTC(), l.UpdatedAt.UTC(),
	)
	return mapErr(err)
}

func (r offlineRepo) CreateOTP(ctx context.Context, otp *conversation.ChannelOTP) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO channel_otps (id, phone_hmac, target_user_id, code_hmac, attempts, consumed_at, expires_at, created_at)
		VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8)`,
		otp.ID, otp.PhoneHMAC, otp.TargetUserID, otp.CodeHMAC, otp.Attempts,
		otp.ConsumedAt, otp.ExpiresAt.UTC(), otp.CreatedAt.UTC(),
	)
	return mapErr(err)
}

func (r offlineRepo) GetLatestOTPForUpdate(ctx context.Context, phoneHMAC string) (*conversation.ChannelOTP, error) {
	var o conversation.ChannelOTP
	err := r.db.QueryRow(ctx, `
		SELECT id, phone_hmac, target_user_id::text, code_hmac, attempts, consumed_at, expires_at, created_at
		FROM channel_otps
		WHERE phone_hmac = $1
		ORDER BY created_at DESC, id DESC
		LIMIT 1
		FOR UPDATE`, phoneHMAC).Scan(
		&o.ID, &o.PhoneHMAC, &o.TargetUserID, &o.CodeHMAC,
		&o.Attempts, &o.ConsumedAt, &o.ExpiresAt, &o.CreatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

func (r offlineRepo) UpdateOTP(ctx context.Context, otp *conversation.ChannelOTP) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE channel_otps
		SET attempts = $2, consumed_at = $3
		WHERE id = $1`,
		otp.ID, otp.Attempts, otp.ConsumedAt,
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const sessionSelectCols = `
	id, channel, provider_session_id, phone_hmac, user_id::text, state, step,
	meter_options, selected_meter_id, selected_meter_masked, selected_disco,
	amount_minor, fee_minor, total_minor, post_otp_action,
	last_raw_input, last_reply, has_cached_turn, last_inbound_msg_ts,
	expires_at, created_at, updated_at`

func scanConversationSession(scan func(dest ...any) error) (*conversation.Session, error) {
	var (
		s          conversation.Session
		ch, st     string
		optionsRaw []byte
	)
	if err := scan(
		&s.ID, &ch, &s.ProviderSessionID, &s.PhoneHMAC, &s.UserID, &st, &s.Step,
		&optionsRaw, &s.SelectedMeterID, &s.SelectedMeterMasked, &s.SelectedDisco,
		&s.AmountMinor, &s.FeeMinor, &s.TotalMinor, &s.PostOTPAction,
		&s.LastRawInput, &s.LastReply, &s.HasCachedTurn, &s.LastInboundMsgTS,
		&s.ExpiresAt, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	s.Channel = conversation.Channel(ch)
	s.State = conversation.State(st)
	if len(optionsRaw) > 0 {
		_ = json.Unmarshal(optionsRaw, &s.MeterOptions)
	}
	return &s, nil
}

func (r offlineRepo) CreateSession(ctx context.Context, s *conversation.Session) error {
	optsJSON, err := json.Marshal(s.MeterOptions)
	if err != nil {
		return err
	}
	if string(optsJSON) == "null" {
		optsJSON = []byte("[]")
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO conversation_sessions (
			id, channel, provider_session_id, phone_hmac, user_id, state, step,
			meter_options, selected_meter_id, selected_meter_masked, selected_disco,
			amount_minor, fee_minor, total_minor, post_otp_action,
			last_raw_input, last_reply, has_cached_turn, last_inbound_msg_ts,
			expires_at, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5::uuid, $6, $7,
			$8::jsonb, $9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19,
			$20, $21, $22
		)`,
		s.ID, string(s.Channel), s.ProviderSessionID, s.PhoneHMAC, s.UserID, string(s.State), s.Step,
		string(optsJSON), s.SelectedMeterID, s.SelectedMeterMasked, s.SelectedDisco,
		s.AmountMinor, s.FeeMinor, s.TotalMinor, s.PostOTPAction,
		s.LastRawInput, s.LastReply, s.HasCachedTurn, s.LastInboundMsgTS.UTC(),
		s.ExpiresAt.UTC(), s.CreatedAt.UTC(), s.UpdatedAt.UTC(),
	)
	return mapErr(err)
}

func (r offlineRepo) GetSessionByProviderIDForUpdate(ctx context.Context, ch conversation.Channel, providerSessionID string) (*conversation.Session, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+sessionSelectCols+`
		FROM conversation_sessions
		WHERE channel = $1 AND provider_session_id = $2
		FOR UPDATE`, string(ch), providerSessionID)
	return scanConversationSession(row.Scan)
}

func (r offlineRepo) GetActiveSessionForPhoneForUpdate(ctx context.Context, ch conversation.Channel, phoneHMAC string, now time.Time) (*conversation.Session, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+sessionSelectCols+`
		FROM conversation_sessions
		WHERE channel = $1
		  AND phone_hmac = $2
		  AND state NOT IN ('COMPLETED', 'CANCELLED', 'EXPIRED')
		  AND expires_at > $3
		ORDER BY updated_at DESC, id DESC
		LIMIT 1
		FOR UPDATE`, string(ch), phoneHMAC, now.UTC())
	return scanConversationSession(row.Scan)
}

func (r offlineRepo) UpdateSession(ctx context.Context, s *conversation.Session) error {
	optsJSON, err := json.Marshal(s.MeterOptions)
	if err != nil {
		return err
	}
	if string(optsJSON) == "null" {
		optsJSON = []byte("[]")
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE conversation_sessions
		SET user_id               = $2::uuid,
		    state                 = $3,
		    step                  = $4,
		    meter_options         = $5::jsonb,
		    selected_meter_id     = $6,
		    selected_meter_masked = $7,
		    selected_disco        = $8,
		    amount_minor          = $9,
		    fee_minor             = $10,
		    total_minor           = $11,
		    post_otp_action       = $12,
		    last_raw_input        = $13,
		    last_reply            = $14,
		    has_cached_turn       = $15,
		    last_inbound_msg_ts   = $16,
		    expires_at            = $17,
		    updated_at            = $18
		WHERE id = $1`,
		s.ID, s.UserID, string(s.State), s.Step,
		string(optsJSON), s.SelectedMeterID, s.SelectedMeterMasked, s.SelectedDisco,
		s.AmountMinor, s.FeeMinor, s.TotalMinor, s.PostOTPAction,
		s.LastRawInput, s.LastReply, s.HasCachedTurn, s.LastInboundMsgTS.UTC(),
		s.ExpiresAt.UTC(), s.UpdatedAt.UTC(),
	)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r offlineRepo) RecordProcessedMessage(ctx context.Context, ch conversation.Channel, messageID, phoneHMAC string, at time.Time) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO processed_channel_messages (channel, message_id, phone_hmac, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (channel, message_id) DO NOTHING`,
		string(ch), messageID, phoneHMAC, at.UTC(),
	)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r offlineRepo) RecordPurchase(ctx context.Context, p *conversation.OfflinePurchase) error {
	cur := strings.TrimSpace(string(p.Currency))
	if cur == "" {
		cur = string(money.NGN)
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO offline_purchases (
			transaction_id, user_id, channel, phone_hmac, provider_session_id,
			amount_minor, currency, created_at
		) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8)`,
		p.TransactionID, p.UserID, string(p.Channel), p.PhoneHMAC,
		p.ProviderSessionID, p.AmountMinor, cur, p.CreatedAt.UTC(),
	)
	return mapErr(err)
}

func (r offlineRepo) GetPurchase(ctx context.Context, txnID uuid.UUID) (*conversation.OfflinePurchase, error) {
	var (
		p      conversation.OfflinePurchase
		ch, cu string
	)
	err := r.db.QueryRow(ctx, `
		SELECT transaction_id, user_id::text, channel, phone_hmac, provider_session_id,
		       amount_minor, currency, created_at
		FROM offline_purchases
		WHERE transaction_id = $1`, txnID).Scan(
		&p.TransactionID, &p.UserID, &ch, &p.PhoneHMAC, &p.ProviderSessionID,
		&p.AmountMinor, &cu, &p.CreatedAt,
	)
	if err != nil {
		return nil, mapErr(err)
	}
	p.Channel = conversation.Channel(ch)
	p.Currency = money.Currency(strings.TrimSpace(cu))
	return &p, nil
}

func (r offlineRepo) SumPhoneSpendMinor(ctx context.Context, phoneHMAC string, startUTC, endUTC time.Time) (int64, error) {
	var sum int64
	err := r.db.QueryRow(ctx, `
		SELECT coalesce(sum(op.amount_minor), 0)
		FROM offline_purchases op
		JOIN electricity_transactions t ON t.id = op.transaction_id
		WHERE op.phone_hmac = $1
		  AND op.created_at >= $2
		  AND op.created_at < $3
		  AND t.payment_status NOT IN ('FAILED', 'REFUNDED')`,
		phoneHMAC, startUTC.UTC(), endUTC.UTC(),
	).Scan(&sum)
	if err != nil {
		return 0, mapErr(err)
	}
	return sum, nil
}

func (r offlineRepo) RecordTokenResend(ctx context.Context, tr *conversation.TokenResend) error {
	id := tr.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO offline_token_resends (id, user_id, transaction_id, channel, phone_hmac, created_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
		id, tr.UserID, tr.TransactionID, string(tr.Channel), tr.PhoneHMAC, tr.CreatedAt.UTC(),
	)
	return mapErr(err)
}

func (r offlineRepo) CountTokenResends(ctx context.Context, userID string, startUTC, endUTC time.Time) (int, error) {
	var cnt int
	err := r.db.QueryRow(ctx, `
		SELECT count(*)
		FROM offline_token_resends
		WHERE user_id = $1::uuid
		  AND created_at >= $2
		  AND created_at < $3`,
		userID, startUTC.UTC(), endUTC.UTC(),
	).Scan(&cnt)
	if err != nil {
		return 0, mapErr(err)
	}
	return cnt, nil
}
