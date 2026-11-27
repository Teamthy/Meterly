package orchestrator

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const (
	opRecordTokenCopy = "record_token_copy"
	opRecordTokenSend = "record_token_send"
)

// RecordTokenCopy records a client-reported copy action, not actual delivery.
// Only the purchaser can record it, and never for a still-remote transaction.
// Reservation, record and audit are atomic; retries with the same key return
// the original metadata. The STS token itself is never accessed or recorded.
func (o *Orchestrator) RecordTokenCopy(ctx context.Context, user string, id uuid.UUID, key string) (tokenshares.Share, error) {
	var result tokenshares.Share
	if user == "" || id == uuid.Nil || len(key) < 16 || len(key) > 128 {
		return result, errors.New("invalid copy request")
	}
	now := o.Now().UTC()
	err := o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Transactions().GetForUpdate(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && t.CustomerID != user) {
			return tx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.DeliveryMethod == tx.MethodRemote {
			return ErrCreditOnMeter
		}
		if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
			return ErrTokenUnavailable
		}
		requestHash := idempotency.HashRequest([]byte(id.String() + ":COPY"))
		existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{Scope: user, Operation: opRecordTokenCopy, Key: key, RequestHash: requestHash, CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL)})
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RequestHash != requestHash {
				return idempotency.ErrConflict
			}
			if existing.ResultRef == "" {
				return idempotency.ErrInProgress
			}
			sid, err := uuid.Parse(existing.ResultRef)
			if err != nil {
				return err
			}
			result, err = r.TokenShares().GetCopy(ctx, sid, id, user)
			return err
		}
		result = tokenshares.Share{ID: uuid.New(), TransactionID: id, UserID: user, Channel: tokenshares.Copy, SharedAt: now}
		if err := r.TokenShares().AddCopy(ctx, result); err != nil {
			return err
		}
		actor := audit.Actor{UserID: user, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("token.copy_recorded", resourceTxn, id, map[string]any{"channel": "COPY"})); err != nil {
			return err
		}
		if err := emit(ctx, r, "token.copy.recorded", id, map[string]any{"channel": "COPY"}); err != nil {
			return err
		}
		return r.Idempotency().Complete(ctx, user, opRecordTokenCopy, key, result.ID.String())
	})
	return result, err
}

// RecordTokenSend enqueues a token-bearing SMS, email or WhatsApp delivery.
// The STS token is not read here; the notification worker decrypts it in
// memory at send time. Destination ciphertext is bound to the share id.
// An empty destination sends to the purchaser's verified contact for that
// channel. A different destination is a third-party share and requires
// FeatureGiftSMS. COPY is rejected.
func (o *Orchestrator) RecordTokenSend(ctx context.Context, user string, id uuid.UUID, key, channel, destination string) (tokenshares.Share, error) {
	var result tokenshares.Share
	if user == "" || id == uuid.Nil || len(key) < 16 || len(key) > 128 {
		return result, errors.New("invalid token send request")
	}
	channel, err := tokenshares.ParseChannel(channel)
	if err != nil || !tokenshares.IsSendChannel(channel) {
		return result, tokenshares.ErrUnsupportedChannel
	}
	now := o.Now().UTC()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Transactions().GetForUpdate(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && t.CustomerID != user) {
			return tx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.DeliveryMethod == tx.MethodRemote {
			return ErrCreditOnMeter
		}
		if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
			return ErrTokenUnavailable
		}
		norm, masked, selfHMAC, err := o.resolveShareDestination(ctx, r, user, channel, destination)
		if err != nil {
			return err
		}
		destHMAC := notifications.DestinationHMAC(o.box, norm)
		if destHMAC != selfHMAC && !o.featureGiftSMS {
			return ErrTokenShareDisabled
		}
		ch := notifications.Channel(channel)
		optedOut, err := r.Notifications().IsOptedOut(ctx, destHMAC, ch)
		if err != nil {
			return err
		}
		if optedOut {
			return ErrTokenShareOptedOut
		}
		start, end := tokenshares.LagosDayBounds(now)
		n, err := r.TokenShares().CountSends(ctx, user, start, end)
		if err != nil {
			return err
		}
		if n >= tokenshares.MaxSendsPerDay {
			return ErrTokenShareRateLimited
		}
		requestHash := idempotency.HashRequest([]byte(id.String() + ":" + channel + ":" + destHMAC))
		existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{Scope: user, Operation: opRecordTokenSend, Key: key, RequestHash: requestHash, CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL)})
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RequestHash != requestHash {
				return idempotency.ErrConflict
			}
			if existing.ResultRef == "" {
				return idempotency.ErrInProgress
			}
			sid, err := uuid.Parse(existing.ResultRef)
			if err != nil {
				return err
			}
			result, err = r.TokenShares().GetShare(ctx, sid)
			if err != nil {
				return err
			}
			result.DestCipher = nil
			return nil
		}
		shareID := uuid.New()
		ct, err := o.box.EncryptBound(norm, tokenshares.DestAAD(shareID))
		if err != nil {
			return err
		}
		result = tokenshares.Share{
			ID: shareID, TransactionID: id, UserID: user, Channel: channel, SharedAt: now,
			DestHMAC: destHMAC, DestCipher: ct, DestKeyVer: o.box.KeyVersion, DestMasked: masked,
		}
		if err := r.TokenShares().AddShare(ctx, result); err != nil {
			return err
		}
		d := &notifications.Delivery{
			EventID:         shareID,
			TransactionID:   id,
			UserID:          user,
			RecipientKind:   notifications.RecipientTokenShare,
			RecipientSlot:   shareID.String(),
			Channel:         ch,
			DestinationHMAC: destHMAC,
			TemplateID:      notifications.TemplateTokenShare,
			TemplateVersion: notifications.CurrentTemplateVersion,
			Locale:          notifications.LocaleEN,
			Status:          notifications.StatusPending,
			NextAttemptAt:   now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := r.Notifications().CreateDelivery(ctx, d); err != nil {
			return err
		}
		actor := audit.Actor{UserID: user, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("token.share.recorded", resourceTxn, id, map[string]any{"channel": channel})); err != nil {
			return err
		}
		if err := emit(ctx, r, "token.share.recorded", id, map[string]any{"channel": channel}); err != nil {
			return err
		}
		if err := r.Idempotency().Complete(ctx, user, opRecordTokenSend, key, result.ID.String()); err != nil {
			return err
		}
		result.DestCipher = nil
		return nil
	})
	return result, err
}

func (o *Orchestrator) resolveShareDestination(ctx context.Context, r store.Repos, user, channel, destination string) (norm, masked, selfHMAC string, err error) {
	contacts, err := r.Users().Contacts(ctx, user)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", "", "", err
	}
	switch channel {
	case tokenshares.SMS, tokenshares.WhatsApp:
		if len(contacts.PhoneCiphertext) > 0 {
			self, oErr := notifications.OpenUserPhone(o.box, user, contacts.PhoneCiphertext)
			if oErr != nil {
				return "", "", "", oErr
			}
			selfHMAC = notifications.DestinationHMAC(o.box, self)
		}
		destination = strings.TrimSpace(destination)
		if destination == "" {
			if selfHMAC == "" {
				return "", "", "", ErrTokenShareContact
			}
			plain, oErr := notifications.OpenUserPhone(o.box, user, contacts.PhoneCiphertext)
			if oErr != nil {
				return "", "", "", oErr
			}
			return plain, notifications.MaskPhone(plain), selfHMAC, nil
		}
		norm, err = notifications.NormalizePhone(destination)
		if err != nil {
			return "", "", "", err
		}
		return norm, notifications.MaskPhone(norm), selfHMAC, nil
	case tokenshares.Email:
		if len(contacts.EmailCiphertext) > 0 {
			self, oErr := notifications.OpenUserEmail(o.box, user, contacts.EmailCiphertext)
			if oErr != nil {
				return "", "", "", oErr
			}
			selfHMAC = notifications.DestinationHMAC(o.box, self)
		}
		destination = strings.TrimSpace(destination)
		if destination == "" {
			if selfHMAC == "" {
				return "", "", "", ErrTokenShareContact
			}
			plain, oErr := notifications.OpenUserEmail(o.box, user, contacts.EmailCiphertext)
			if oErr != nil {
				return "", "", "", oErr
			}
			return plain, notifications.MaskEmail(plain), selfHMAC, nil
		}
		norm, err = notifications.NormalizeEmail(destination)
		if err != nil {
			return "", "", "", err
		}
		return norm, notifications.MaskEmail(norm), selfHMAC, nil
	default:
		return "", "", "", tokenshares.ErrUnsupportedChannel
	}
}

// ListTokenCopies returns the purchaser's share metadata (COPY and sends).
func (o *Orchestrator) ListTokenCopies(ctx context.Context, user string, id uuid.UUID) ([]tokenshares.Share, error) {
	t, err := o.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, tx.ErrNotFound) || (err == nil && t.CustomerID != user) {
		return nil, tx.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return o.st.Repos().TokenShares().ListShares(ctx, id, user)
}
