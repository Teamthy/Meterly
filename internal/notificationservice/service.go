// Package notificationservice turns committed allowlisted outbox events into
// durable notification_deliveries, executes outbound sends outside database
// transactions with strict UNKNOWN semantics, handles signature-verified
// inbound STOP webhooks and purges aged destination HMACs.
//
// Privacy invariants:
//   - Raw tokens, meter numbers, phone numbers, email addresses and rendered
//     bodies are never stored in notification_deliveries, audit metadata or
//     logs.
//   - Token-bearing gift SMS decrypts the STS token only in worker memory at
//     send time and is never automatically resent once an attempt ends in
//     UNKNOWN.
package notificationservice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const (
	DefaultHMACRetention     = 365 * 24 * time.Hour
	DefaultStaleSendingAfter = 5 * time.Minute
)

var ErrProviderNotFound = errors.New("notificationservice: unknown inbound provider")

type Config struct {
	Store             store.Store
	Box               *secure.Box
	Providers         map[notifications.Channel]notifications.Provider
	InboundVerifiers  map[string]notifications.InboundVerifier
	FeatureGiftSMS    bool
	HMACRetention     time.Duration
	StaleSendingAfter time.Duration
	Now               func() time.Time
	Log               *slog.Logger
}

type Service struct {
	st                store.Store
	box               *secure.Box
	providers         map[notifications.Channel]notifications.Provider
	verifiers         map[string]notifications.InboundVerifier
	featureGiftSMS    bool
	hmacRetention     time.Duration
	staleSendingAfter time.Duration
	now               func() time.Time
	log               *slog.Logger
}

func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Box == nil {
		return nil, errors.New("notificationservice: store and secure.Box are required")
	}
	provs := map[notifications.Channel]notifications.Provider{}
	for ch, p := range cfg.Providers {
		if p != nil {
			provs[ch] = p
		}
	}
	vers := map[string]notifications.InboundVerifier{}
	for name, v := range cfg.InboundVerifiers {
		if v != nil {
			vers[strings.ToLower(strings.TrimSpace(name))] = v
		}
	}
	ret := cfg.HMACRetention
	if ret <= 0 {
		ret = DefaultHMACRetention
	}
	stale := cfg.StaleSendingAfter
	if stale <= 0 {
		stale = DefaultStaleSendingAfter
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		st:                cfg.Store,
		box:               cfg.Box,
		providers:         provs,
		verifiers:         vers,
		featureGiftSMS:    cfg.FeatureGiftSMS,
		hmacRetention:     ret,
		staleSendingAfter: stale,
		now:               nowFn,
		log:               log,
	}, nil
}

// SetFeatureGiftSMS updates the gift SMS gate (used in tests and config reloads).
func (s *Service) SetFeatureGiftSMS(enabled bool) {
	s.featureGiftSMS = enabled
}

// Sink returns an outbox.Sink that atomically enqueues notification_deliveries
// inside the outbox publisher's database transaction.
func (s *Service) Sink() outbox.Sink {
	return s.HandleEvent
}

// HandleEvent turns an allowlisted transaction outbox event into idempotent
// notification_deliveries rows.
func (s *Service) HandleEvent(ctx context.Context, e events.Event) error {
	if e.AggregateType != "electricity_transaction" {
		return nil
	}
	if r, ok := store.ReposFromContext(ctx); ok {
		return s.enqueueInRepos(ctx, r, e)
	}
	return s.st.InTx(ctx, func(r store.Repos) error {
		return s.enqueueInRepos(ctx, r, e)
	})
}

func (s *Service) enqueueInRepos(ctx context.Context, r store.Repos, e events.Event) error {
	t, err := r.Transactions().Get(ctx, e.AggregateID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // synthetic/orphaned outbox event; nothing to deliver
	}
	if err != nil {
		return err
	}
	spec, ok, err := notifications.TemplateSpecFor(e, t.DeliveryMethod)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	now := s.now().UTC()
	var activeGiftSMSDestHMAC string

	if e.Type == "token.available" && t.IsGift && t.DeliveryMethod == tx.MethodToken {
		gift, gErr := r.Gifts().Get(ctx, t.ID)
		if gErr != nil && !errors.Is(gErr, store.ErrNotFound) {
			return gErr
		}
		if gErr == nil {
			giftDestHMAC, hasContact, decErr := s.giftDestinationHMAC(gift)
			if decErr != nil {
				return decErr
			}
			status := notifications.StatusPending
			errCode := ""
			switch {
			case !gift.NotificationRequested || !hasContact:
				status = notifications.StatusSuppressed
				errCode = "NOTIFICATION_NOT_REQUESTED"
			case !s.featureGiftSMS:
				status = notifications.StatusSuppressed
				errCode = "FEATURE_GIFT_SMS_DISABLED"
			default:
				optedOut, oErr := r.Notifications().IsOptedOut(ctx, giftDestHMAC, notifications.SMS)
				if oErr != nil {
					return oErr
				}
				if optedOut {
					status = notifications.StatusSuppressed
					errCode = "RECIPIENT_OPTED_OUT"
				} else {
					activeGiftSMSDestHMAC = giftDestHMAC
				}
			}
			gd := &notifications.Delivery{
				EventID:         e.ID,
				TransactionID:   t.ID,
				RecipientKind:   notifications.RecipientGift,
				Channel:         notifications.SMS,
				DestinationHMAC: giftDestHMAC,
				TemplateID:      notifications.TemplateGiftTokenSMS,
				TemplateVersion: notifications.CurrentTemplateVersion,
				Locale:          notifications.LocaleEN,
				Status:          status,
				NextAttemptAt:   now,
				LastErrorCode:   errCode,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if _, err := r.Notifications().CreateDelivery(ctx, gd); err != nil {
				return err
			}
		}
	}

	contacts, err := r.Users().Contacts(ctx, t.CustomerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	prefs, err := r.Notifications().GetPreferences(ctx, t.CustomerID)
	if errors.Is(err, store.ErrNotFound) {
		prefs = notifications.DefaultPreferences()
	} else if err != nil {
		return err
	}

	for _, ch := range []notifications.Channel{notifications.SMS, notifications.Email, notifications.WhatsApp} {
		destHMAC := contactHMACForChannel(contacts, ch)
		if destHMAC == "" {
			continue // no verified contact for this channel
		}
		if !prefs.Enabled(ch) {
			continue
		}
		status := notifications.StatusPending
		errCode := ""
		if ch == notifications.SMS && activeGiftSMSDestHMAC != "" && destHMAC == activeGiftSMSDestHMAC {
			status = notifications.StatusSuppressed
			errCode = "DUPLICATE_DESTINATION"
		} else {
			optedOut, err := r.Notifications().IsOptedOut(ctx, destHMAC, ch)
			if err != nil {
				return err
			}
			if optedOut {
				status = notifications.StatusSuppressed
				errCode = "RECIPIENT_OPTED_OUT"
			}
		}
		pd := &notifications.Delivery{
			EventID:         e.ID,
			TransactionID:   t.ID,
			RecipientKind:   notifications.RecipientPurchaser,
			Channel:         ch,
			DestinationHMAC: destHMAC,
			TemplateID:      spec.ID,
			TemplateVersion: spec.Version,
			Locale:          notifications.LocaleEN,
			Status:          status,
			NextAttemptAt:   now,
			LastErrorCode:   errCode,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if _, err := r.Notifications().CreateDelivery(ctx, pd); err != nil {
			return err
		}
	}
	return nil
}

func contactHMACForChannel(c notifications.UserContacts, ch notifications.Channel) string {
	switch ch {
	case notifications.SMS, notifications.WhatsApp:
		if len(c.PhoneCiphertext) == 0 {
			return ""
		}
		return c.PhoneHMAC
	case notifications.Email:
		if len(c.EmailCiphertext) == 0 {
			return ""
		}
		return c.EmailHMAC
	default:
		return ""
	}
}

func (s *Service) giftDestinationHMAC(g gifts.Gift) (string, bool, error) {
	if len(g.ContactCiphertext) == 0 {
		return "", false, nil
	}
	plain, err := s.box.DecryptBound(g.ContactCiphertext, []byte("gift-contact:"+g.TransactionID.String()))
	if err != nil {
		return "", false, err
	}
	norm, err := notifications.NormalizePhone(plain)
	if err != nil {
		return "", false, err
	}
	return notifications.DestinationHMAC(s.box, norm), true, nil
}

// ProcessBatch processes up to limit due notification deliveries. Safe to run
// across multiple concurrent workers.
func (s *Service) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	now := s.now().UTC()
	staleBefore := now.Add(-s.staleSendingAfter)
	ids, err := s.st.Repos().Notifications().ListDueDeliveries(ctx, now, staleBefore, limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		did, err := s.ProcessOne(ctx, id)
		if err != nil {
			s.log.Warn("notification delivery step failed", "delivery_id", id, "err", err)
			continue
		}
		if did {
			processed++
		}
	}
	return processed, nil
}

// ProcessOne advances a single notification delivery by one step, executing
// any provider network call strictly outside database transactions.
func (s *Service) ProcessOne(ctx context.Context, id uuid.UUID) (bool, error) {
	snap, err := s.st.Repos().Notifications().GetDelivery(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.now().UTC()
	staleBefore := now.Add(-s.staleSendingAfter)

	// Recover crashed in-flight SENDING jobs into UNKNOWN (never PENDING, so
	// token SMS cannot be resent after an interrupted attempt).
	if snap.Status == notifications.StatusSending && !snap.UpdatedAt.After(staleBefore) {
		var recovered bool
		err := s.st.InTx(ctx, func(r store.Repos) error {
			d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
			if err != nil {
				return err
			}
			if d.Status != notifications.StatusSending || d.UpdatedAt.After(staleBefore) {
				return nil
			}
			if err := d.Transition(notifications.StatusUnknown, now); err != nil {
				return err
			}
			d.LastErrorCode = "WORKER_INTERRUPTED"
			d.NextAttemptAt = now
			recovered = true
			return r.Notifications().UpdateDelivery(ctx, d)
		})
		if err != nil || !recovered {
			return recovered, err
		}
		snap, err = s.st.Repos().Notifications().GetDelivery(ctx, id)
		if err != nil {
			return true, err
		}
	}

	prov, hasProv := s.providers[snap.Channel]
	if !hasProv {
		return false, nil
	}

	// Reconcile UNKNOWN via provider status API when supported and provider_ref is known.
	provConfirmedNotFound := false
	if snap.Status == notifications.StatusUnknown && snap.ProviderRef != "" {
		if checker, ok := prov.(notifications.StatusChecker); ok {
			pStatus, pCode, chkErr := checker.CheckDeliveryStatus(ctx, idempotencyKey(snap.ID), snap.ProviderRef)
			if chkErr != nil {
				// Leave UNKNOWN and back off; never mark FAILED on an ambiguous status check.
				_ = s.st.InTx(ctx, func(r store.Repos) error {
					d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
					if err != nil || d.Status != notifications.StatusUnknown {
						return err
					}
					d.NextAttemptAt = s.now().UTC().Add(notifications.Backoff(maxInt(1, d.Attempts)))
					d.UpdatedAt = s.now().UTC()
					return r.Notifications().UpdateDelivery(ctx, d)
				})
				return true, nil
			}
			switch pStatus {
			case notifications.ProviderStatusSent:
				return s.settleUnknown(ctx, id, notifications.StatusSent, "")
			case notifications.ProviderStatusFailed:
				if pCode == "" {
					pCode = "PROVIDER_REPORTED_FAILED"
				}
				return s.settleUnknown(ctx, id, notifications.StatusFailed, pCode)
			case notifications.ProviderStatusPending:
				return true, s.st.InTx(ctx, func(r store.Repos) error {
					d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
					if err != nil || d.Status != notifications.StatusUnknown {
						return err
					}
					d.NextAttemptAt = s.now().UTC().Add(notifications.Backoff(maxInt(1, d.Attempts)))
					d.UpdatedAt = s.now().UTC()
					return r.Notifications().UpdateDelivery(ctx, d)
				})
			case notifications.ProviderStatusNotFound:
				provConfirmedNotFound = true
			}
		}
	}

	var (
		claimed    bool
		suppressed bool
		sendReq    notifications.SendRequest
	)
	claimNow := s.now().UTC()
	err = s.st.InTx(ctx, func(r store.Repos) error {
		d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if d.NextAttemptAt.After(claimNow) {
			return nil
		}
		switch d.Status {
		case notifications.StatusPending:
		case notifications.StatusUnknown:
			if d.TokenBearing() && !provConfirmedNotFound {
				return nil // never auto-resend token-bearing SMS from UNKNOWN
			}
			if !provConfirmedNotFound && d.Attempts >= 2 {
				return nil // non-token UNKNOWN allows at most one retry
			}
			if d.Attempts >= notifications.MaxAttempts {
				return nil
			}
		default:
			return nil
		}

		enabled, err := r.Providers().Enabled(ctx, prov.Name())
		if err != nil {
			return err
		}
		if !enabled {
			d.LastErrorCode = "PROVIDER_DISABLED"
			d.NextAttemptAt = claimNow.Add(notifications.Backoff(maxInt(1, d.Attempts)))
			d.UpdatedAt = claimNow
			return r.Notifications().UpdateDelivery(ctx, d)
		}

		req, supCode, permFailCode, prepErr := s.prepareSendInTx(ctx, r, d, claimNow)
		if prepErr != nil {
			return prepErr
		}
		if supCode != "" {
			if err := d.Transition(notifications.StatusSuppressed, claimNow); err != nil {
				return err
			}
			d.LastErrorCode = supCode
			suppressed = true
			return r.Notifications().UpdateDelivery(ctx, d)
		}
		if permFailCode != "" {
			if err := d.Transition(notifications.StatusFailed, claimNow); err != nil {
				return err
			}
			d.LastErrorCode = permFailCode
			suppressed = true
			return r.Notifications().UpdateDelivery(ctx, d)
		}

		if err := d.Transition(notifications.StatusSending, claimNow); err != nil {
			return err
		}
		d.Attempts++
		d.ProviderName = prov.Name()
		if err := r.Notifications().UpdateDelivery(ctx, d); err != nil {
			return err
		}
		sendReq = req
		claimed = true
		return nil
	})
	if err != nil || suppressed || !claimed {
		return suppressed, err
	}

	// Execute provider call strictly outside DB transaction.
	res, sendErr := prov.SendDelivery(ctx, sendReq)
	// Scrub in-memory plaintext body/destination immediately after send returns.
	sendReq.Message.Body = ""
	sendReq.Message.Destination = ""

	finishNow := s.now().UTC()
	err = s.st.InTx(ctx, func(r store.Repos) error {
		d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if d.Status != notifications.StatusSending {
			return nil
		}
		if sendErr == nil {
			if err := d.Transition(notifications.StatusSent, finishNow); err != nil {
				return err
			}
			d.ProviderRef = res.ProviderRef
			d.LastErrorCode = ""
			d.SentAt = &finishNow
			return r.Notifications().UpdateDelivery(ctx, d)
		}

		if providers.IsAmbiguous(sendErr) {
			if err := d.Transition(notifications.StatusUnknown, finishNow); err != nil {
				return err
			}
			d.LastErrorCode = "PROVIDER_AMBIGUOUS"
			if !d.TokenBearing() && d.Attempts < 2 {
				d.NextAttemptAt = finishNow.Add(notifications.Backoff(d.Attempts))
			}
			s.log.Warn("notification delivery outcome ambiguous; marked UNKNOWN",
				"delivery_id", d.ID, "channel", d.Channel, "recipient_kind", d.RecipientKind, "attempts", d.Attempts)
			return r.Notifications().UpdateDelivery(ctx, d)
		}

		if code, ok := notifications.IsPermanent(sendErr); ok {
			if err := d.Transition(notifications.StatusFailed, finishNow); err != nil {
				return err
			}
			d.LastErrorCode = code
			return r.Notifications().UpdateDelivery(ctx, d)
		}

		code, ok := notifications.IsRetryable(sendErr)
		if !ok || code == "" {
			code = "SEND_FAILED"
		}
		if d.Attempts >= notifications.MaxAttempts {
			if err := d.Transition(notifications.StatusFailed, finishNow); err != nil {
				return err
			}
			d.LastErrorCode = code
			return r.Notifications().UpdateDelivery(ctx, d)
		}
		if err := d.Transition(notifications.StatusPending, finishNow); err != nil {
			return err
		}
		d.LastErrorCode = code
		d.NextAttemptAt = finishNow.Add(notifications.Backoff(d.Attempts))
		return r.Notifications().UpdateDelivery(ctx, d)
	})
	return true, err
}

func (s *Service) settleUnknown(ctx context.Context, id uuid.UUID, to notifications.Status, errCode string) (bool, error) {
	now := s.now().UTC()
	var settled bool
	err := s.st.InTx(ctx, func(r store.Repos) error {
		d, err := r.Notifications().GetDeliveryForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if d.Status != notifications.StatusUnknown {
			return nil
		}
		if err := d.Transition(to, now); err != nil {
			return err
		}
		d.LastErrorCode = errCode
		if to == notifications.StatusSent {
			d.SentAt = &now
		}
		settled = true
		return r.Notifications().UpdateDelivery(ctx, d)
	})
	return settled, err
}

func (s *Service) prepareSendInTx(
	ctx context.Context,
	r store.Repos,
	d *notifications.Delivery,
	now time.Time,
) (req notifications.SendRequest, suppressCode, failCode string, err error) {
	t, err := r.Transactions().Get(ctx, d.TransactionID)
	if err != nil {
		return req, "", "", err
	}

	switch d.RecipientKind {
	case notifications.RecipientGift:
		if !s.featureGiftSMS {
			return req, "FEATURE_GIFT_SMS_DISABLED", "", nil
		}
		gift, err := r.Gifts().Get(ctx, t.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (!gift.NotificationRequested || len(gift.ContactCiphertext) == 0)) {
			return req, "NOTIFICATION_NOT_REQUESTED", "", nil
		}
		if err != nil {
			return req, "", "", err
		}
		plainPhone, err := s.box.DecryptBound(gift.ContactCiphertext, []byte("gift-contact:"+t.ID.String()))
		if err != nil {
			return req, "", "CONTACT_DECRYPT_FAILED", nil
		}
		normPhone, err := notifications.NormalizePhone(plainPhone)
		if err != nil {
			return req, "", "INVALID_RECIPIENT_PHONE", nil
		}
		destHMAC := notifications.DestinationHMAC(s.box, normPhone)
		optedOut, err := r.Notifications().IsOptedOut(ctx, destHMAC, d.Channel)
		if err != nil {
			return req, "", "", err
		}
		if optedOut {
			return req, "RECIPIENT_OPTED_OUT", "", nil
		}
		if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 {
			return req, "", "TOKEN_UNAVAILABLE", nil
		}
		rawToken, err := s.box.Decrypt(t.TokenCiphertext)
		if err != nil {
			return req, "", "TOKEN_DECRYPT_FAILED", nil
		}
		m, err := r.Meters().Get(ctx, t.MeterID)
		if err != nil {
			return req, "", "", err
		}
		senderName := ""
		if c, err := r.Users().Contacts(ctx, t.CustomerID); err == nil {
			senderName = c.DisplayName
		}
		content, ver, err := notifications.RenderGiftTokenSMS(d.Locale, t.Amount.Minor, m.Masked, rawToken, senderName)
		if err != nil {
			return req, "", "INVALID_TOKEN_FORMAT", nil
		}
		d.DestinationHMAC = destHMAC
		d.TemplateVersion = ver
		return notifications.SendRequest{
			DeliveryID:      d.ID,
			IdempotencyKey:  idempotencyKey(d.ID),
			TemplateID:      d.TemplateID,
			TemplateVersion: ver,
			Locale:          d.Locale,
			Message: notifications.Message{
				EventID:       d.EventID,
				TransactionID: d.TransactionID,
				Channel:       d.Channel,
				Destination:   normPhone,
				Subject:       content.Subject,
				Body:          content.Body,
			},
		}, "", "", nil

	case notifications.RecipientPurchaser:
		prefs, err := r.Notifications().GetPreferences(ctx, t.CustomerID)
		if errors.Is(err, store.ErrNotFound) {
			return req, "USER_NOT_FOUND", "", nil
		}
		if err != nil {
			return req, "", "", err
		}
		if !prefs.Enabled(d.Channel) {
			return req, "PREFERENCE_DISABLED", "", nil
		}
		contacts, err := r.Users().Contacts(ctx, t.CustomerID)
		if errors.Is(err, store.ErrNotFound) {
			return req, "CONTACT_MISSING", "", nil
		}
		if err != nil {
			return req, "", "", err
		}
		var dest string
		switch d.Channel {
		case notifications.SMS, notifications.WhatsApp:
			if len(contacts.PhoneCiphertext) == 0 {
				return req, "CONTACT_MISSING", "", nil
			}
			dest, err = notifications.OpenUserPhone(s.box, t.CustomerID, contacts.PhoneCiphertext)
			if err != nil {
				return req, "", "CONTACT_DECRYPT_FAILED", nil
			}
		case notifications.Email:
			if len(contacts.EmailCiphertext) == 0 {
				return req, "CONTACT_MISSING", "", nil
			}
			dest, err = notifications.OpenUserEmail(s.box, t.CustomerID, contacts.EmailCiphertext)
			if err != nil {
				return req, "", "CONTACT_DECRYPT_FAILED", nil
			}
		default:
			return req, "", "UNSUPPORTED_CHANNEL", nil
		}
		destHMAC := notifications.DestinationHMAC(s.box, dest)
		optedOut, err := r.Notifications().IsOptedOut(ctx, destHMAC, d.Channel)
		if err != nil {
			return req, "", "", err
		}
		if optedOut {
			return req, "RECIPIENT_OPTED_OUT", "", nil
		}
		content, ver, err := notifications.RenderPurchaserTemplate(d.TemplateID, d.Locale)
		if err != nil {
			return req, "", "UNKNOWN_TEMPLATE", nil
		}
		inWindow := false
		var waTpl string
		if d.Channel == notifications.WhatsApp {
			inWindow, err = r.Notifications().HasActiveWhatsAppWindow(ctx, destHMAC, now)
			if err != nil {
				return req, "", "", err
			}
			waTpl = notifications.WhatsAppTemplateName(d.TemplateID)
		}
		d.DestinationHMAC = destHMAC
		d.TemplateVersion = ver
		return notifications.SendRequest{
			DeliveryID:                  d.ID,
			IdempotencyKey:              idempotencyKey(d.ID),
			TemplateID:                  d.TemplateID,
			TemplateVersion:             ver,
			Locale:                      d.Locale,
			WhatsAppTemplateName:        waTpl,
			WithinCustomerServiceWindow: inWindow,
			Message: notifications.Message{
				EventID:       d.EventID,
				TransactionID: d.TransactionID,
				Channel:       d.Channel,
				Destination:   dest,
				Subject:       content.Subject,
				Body:          content.Body,
			},
		}, "", "", nil

	default:
		return req, "", "UNKNOWN_RECIPIENT_KIND", nil
	}
}

func idempotencyKey(deliveryID uuid.UUID) string {
	return "notif:" + deliveryID.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// VerifyInboundChallenge handles GET webhook verification handshakes (WhatsApp Cloud API).
func (s *Service) VerifyInboundChallenge(providerName, mode, verifyToken, challenge string) (string, error) {
	v, ok := s.verifiers[strings.ToLower(strings.TrimSpace(providerName))]
	if !ok {
		return "", ErrProviderNotFound
	}
	cv, ok := v.(notifications.ChallengeVerifier)
	if !ok {
		return "", ErrProviderNotFound
	}
	return cv.VerifyChallenge(mode, verifyToken, challenge)
}

// HandleInbound verifies a provider webhook signature and persists any STOP
// opt-outs and WhatsApp 24-hour customer service windows.
func (s *Service) HandleInbound(ctx context.Context, providerName string, h http.Header, body []byte, correlationID *uuid.UUID) error {
	v, ok := s.verifiers[strings.ToLower(strings.TrimSpace(providerName))]
	if !ok {
		return ErrProviderNotFound
	}
	in, err := v.ParseInbound(h, body)
	if err != nil {
		return err
	}
	if !in.Relevant {
		return nil
	}
	now := s.now().UTC()
	return s.st.InTx(ctx, func(r store.Repos) error {
		for _, phone := range in.WhatsAppInbounds {
			destHMAC := notifications.DestinationHMAC(s.box, phone)
			if err := r.Notifications().RecordWhatsAppInbound(ctx, destHMAC, now); err != nil {
				return err
			}
		}
		for _, opt := range in.OptOuts {
			destHMAC := notifications.DestinationHMAC(s.box, opt.Destination)
			inserted, err := r.Notifications().AddOptOut(ctx, notifications.OptOut{
				DestinationHMAC: destHMAC,
				Channel:         opt.Channel,
				CreatedAt:       now,
			})
			if err != nil {
				return err
			}
			if err := r.Audit().Add(ctx, audit.Event{
				ActorType:     audit.ActorProvider,
				Action:        "notification.opt_out.inbound",
				ResourceType:  "opt_out",
				CorrelationID: correlationID,
				CreatedAt:     now,
				Metadata: map[string]any{
					"provider": v.Name(),
					"channel":  string(opt.Channel),
					"inserted": inserted,
				},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// PurgeExpiredHMACs clears destination_hmac on settled notification_deliveries
// older than the configured retention period.
func (s *Service) PurgeExpiredHMACs(ctx context.Context) (int, error) {
	cutoff := s.now().UTC().Add(-s.hmacRetention)
	return s.st.Repos().Notifications().PurgeOldDestinationHMACs(ctx, cutoff)
}
