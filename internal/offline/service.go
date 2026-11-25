// Package offline wires the channel-agnostic conversation.Engine to Meterly's
// persistent Store, Orchestrator, Meter Registry, SMS/WhatsApp transports,
// per-phone rate limiters, and transport-authenticated USSD and WhatsApp
// Cloud API webhook adapters.
package offline

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/ratelimit"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

var (
	ErrPhoneRateLimited = errors.New("offline: phone rate limit exceeded")
	ErrOTPLockedOut     = errors.New("offline: too many invalid verification attempts")
)

// WhatsAppInboundParser authenticates GET challenge handshakes and POST
// X-Hub-Signature-256 webhooks from Meta WhatsApp Cloud API.
type WhatsAppInboundParser interface {
	VerifyChallenge(mode, verifyToken, challenge string) (string, error)
	ParseInboundMessages(h http.Header, body []byte) ([]whatsapp.InboundMessage, error)
}

type Config struct {
	Store                   store.Store
	Orch                    *orchestrator.Orchestrator
	Box                     *secure.Box
	SMSProvider             notifications.Provider
	WhatsAppProvider        notifications.Provider
	USSDAdapter             *ussd.Adapter
	WhatsAppParser          WhatsAppInboundParser
	MinAmountMinor          int64
	MaxAmountMinor          int64
	FeeMinor                int64
	UnverifiedDailyCapMinor int64
	VerifiedDailyCapMinor   int64
	ReverifyAfter           time.Duration
	PhoneMsgsPerMinute      int
	Now                     func() time.Time
	Log                     *slog.Logger
}

type Service struct {
	st           store.Store
	orch         *orchestrator.Orchestrator
	box          *secure.Box
	smsProv      notifications.Provider
	waProv       notifications.Provider
	ussdAdp      *ussd.Adapter
	waParser     WhatsAppInboundParser
	engine       *conversation.Engine
	engineCfg    conversation.EngineConfig
	phoneLimiter ratelimit.Limiter
	now          func() time.Time
	log          *slog.Logger
}

func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Orch == nil || cfg.Box == nil {
		return nil, errors.New("offline: store, orchestrator and secure.Box are required")
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	perMin := cfg.PhoneMsgsPerMinute
	if perMin <= 0 {
		perMin = 30
	}
	ecfg := conversation.EngineConfig{
		MinAmountMinor:          cfg.MinAmountMinor,
		MaxAmountMinor:          cfg.MaxAmountMinor,
		FeeMinor:                cfg.FeeMinor,
		UnverifiedDailyCapMinor: cfg.UnverifiedDailyCapMinor,
		VerifiedDailyCapMinor:   cfg.VerifiedDailyCapMinor,
		ReverifyAfter:           cfg.ReverifyAfter,
		Now:                     nowFn,
	}
	if ecfg.ReverifyAfter <= 0 {
		ecfg.ReverifyAfter = conversation.DefaultReverifyAfter
	}
	if ecfg.UnverifiedDailyCapMinor <= 0 {
		ecfg.UnverifiedDailyCapMinor = conversation.DefaultUnverifiedDailyCapMinor
	}
	if ecfg.VerifiedDailyCapMinor <= 0 {
		ecfg.VerifiedDailyCapMinor = conversation.DefaultVerifiedDailyCapMinor
	}
	s := &Service{
		st:           cfg.Store,
		orch:         cfg.Orch,
		box:          cfg.Box,
		smsProv:      cfg.SMSProvider,
		waProv:       cfg.WhatsAppProvider,
		ussdAdp:      cfg.USSDAdapter,
		waParser:     cfg.WhatsAppParser,
		engineCfg:    ecfg,
		phoneLimiter: ratelimit.New(perMin, max(10, perMin/2)),
		now:          nowFn,
		log:          log,
	}
	s.engine = conversation.NewEngine(ecfg, s)
	return s, nil
}

// HMACWhatsAppParser provides Meta-compatible X-Hub-Signature-256 + GET
// challenge verification when WHATSAPP_PROVIDER=mock in local/dev mode.
type HMACWhatsAppParser struct {
	AppSecret   string
	VerifyToken string
}

func (p HMACWhatsAppParser) VerifyChallenge(mode, verifyToken, challenge string) (string, error) {
	if p.VerifyToken == "" || mode != "subscribe" || challenge == "" {
		return "", providers.ErrBadSignature
	}
	if subtle.ConstantTimeCompare([]byte(verifyToken), []byte(p.VerifyToken)) != 1 {
		return "", providers.ErrBadSignature
	}
	return challenge, nil
}

func (p HMACWhatsAppParser) ParseInboundMessages(h http.Header, body []byte) ([]whatsapp.InboundMessage, error) {
	if p.AppSecret == "" {
		return nil, providers.ErrBadSignature
	}
	sigHdr := strings.TrimSpace(h.Get("X-Hub-Signature-256"))
	if !strings.HasPrefix(sigHdr, "sha256=") {
		return nil, providers.ErrBadSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHdr, "sha256="))
	if err != nil || len(got) == 0 {
		return nil, providers.ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(p.AppSecret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return nil, providers.ErrBadSignature
	}
	var payload struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []struct {
						ID        string `json:"id"`
						From      string `json:"from"`
						Timestamp string `json:"timestamp"`
						Text      struct {
							Body string `json:"body"`
						} `json:"text"`
						Button struct {
							Text string `json:"text"`
						} `json:"button"`
						Interactive struct {
							ButtonReply struct {
								Title string `json:"title"`
							} `json:"button_reply"`
						} `json:"interactive"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("whatsapp: malformed JSON")
	}
	var out []whatsapp.InboundMessage
	for _, ent := range payload.Entry {
		for _, chg := range ent.Changes {
			for _, m := range chg.Value.Messages {
				rawFrom := strings.TrimSpace(m.From)
				if rawFrom != "" && !strings.HasPrefix(rawFrom, "+") {
					rawFrom = "+" + rawFrom
				}
				norm, err := notifications.NormalizePhone(rawFrom)
				if err != nil {
					continue
				}
				txt := strings.TrimSpace(m.Text.Body)
				if txt == "" {
					txt = strings.TrimSpace(m.Button.Text)
				}
				if txt == "" {
					txt = strings.TrimSpace(m.Interactive.ButtonReply.Title)
				}
				var ts time.Time
				if sec, pErr := strconv.ParseInt(strings.TrimSpace(m.Timestamp), 10, 64); pErr == nil && sec > 0 {
					ts = time.Unix(sec, 0).UTC()
				}
				out = append(out, whatsapp.InboundMessage{
					ID:        strings.TrimSpace(m.ID),
					From:      norm,
					Text:      txt,
					Timestamp: ts,
				})
			}
		}
	}
	return out, nil
}

// HandleUSSDCallback authenticates an Africa's Talking USSD HTTP callback,
// enforces per-phone rate limits on the HMAC, and returns a <= 182-char reply.
func (s *Service) HandleUSSDCallback(ctx context.Context, remoteAddr string, h http.Header, body []byte) (string, error) {
	if s.ussdAdp == nil {
		return "", providers.ErrBadSignature
	}
	in, err := s.ussdAdp.ParseRequest(remoteAddr, h, body)
	if err != nil {
		return "", err
	}
	phoneHMAC := notifications.DestinationHMAC(s.box, in.PhoneNumber)
	if ok, _ := s.phoneLimiter.Allow("ussd:" + phoneHMAC); !ok {
		return conversation.FormatUSSDResponse(false, "Too many requests. Please wait a minute and try again."), nil
	}
	return s.engine.HandleUSSD(ctx, in)
}

// VerifyWhatsAppChallenge validates Meta's GET webhook subscription handshake.
func (s *Service) VerifyWhatsAppChallenge(mode, verifyToken, challenge string) (string, error) {
	if s.waParser == nil {
		return "", providers.ErrBadSignature
	}
	return s.waParser.VerifyChallenge(mode, verifyToken, challenge)
}

// HandleWhatsAppWebhook verifies X-Hub-Signature-256, processes each inbound
// user message through the state machine, and dispatches replies inside the
// 24-hour customer service window (or via template if outside).
func (s *Service) HandleWhatsAppWebhook(ctx context.Context, h http.Header, body []byte) error {
	if s.waParser == nil {
		return providers.ErrBadSignature
	}
	msgs, err := s.waParser.ParseInboundMessages(h, body)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, msg := range msgs {
		phoneHMAC := notifications.DestinationHMAC(s.box, msg.From)
		if ok, _ := s.phoneLimiter.Allow("wa:" + phoneHMAC); !ok {
			continue
		}
		out, err := s.engine.HandleWhatsApp(ctx, conversation.WhatsAppTurnInput{
			MessageID:   msg.ID,
			PhoneNumber: msg.From,
			Text:        msg.Text,
			Timestamp:   msg.Timestamp,
		})
		if err != nil {
			s.log.Warn("whatsapp turn failed", "err", err)
			continue
		}
		if out.Duplicate || out.IgnoredOutOfOrder || out.OptedOut || strings.TrimSpace(out.Reply) == "" {
			continue
		}
		_ = s.sendWhatsAppDirect(ctx, msg.From, phoneHMAC, out.Reply, "", nil, now)
	}
	return nil
}

// StartPhoneLink sends a 6-digit SMS OTP so an authenticated user can link a
// phone number to their Meterly account for USSD & WhatsApp purchases.
func (s *Service) StartPhoneLink(ctx context.Context, userID, rawPhone string) (string, error) {
	normPhone, err := notifications.NormalizePhone(rawPhone)
	if err != nil {
		return "", err
	}
	phoneHMAC := notifications.DestinationHMAC(s.box, normPhone)
	if ok, _ := s.phoneLimiter.Allow("otp:" + phoneHMAC); !ok {
		return "", ErrPhoneRateLimited
	}
	now := s.now().UTC()
	if err := s.IssueOTP(ctx, normPhone, phoneHMAC, userID, now); err != nil {
		return "", err
	}
	return notifications.MaskPhone(normPhone), nil
}

// VerifyPhoneLink verifies a 6-digit SMS OTP and binds the phone to userID.
func (s *Service) VerifyPhoneLink(ctx context.Context, userID, rawPhone, code string) (string, error) {
	normPhone, err := notifications.NormalizePhone(rawPhone)
	if err != nil {
		return "", err
	}
	phoneHMAC := notifications.DestinationHMAC(s.box, normPhone)
	now := s.now().UTC()
	verifiedUID, ok, lockedOut, err := s.VerifyOTP(ctx, normPhone, phoneHMAC, code, now)
	if err != nil {
		return "", err
	}
	if lockedOut {
		return "", ErrOTPLockedOut
	}
	if !ok || verifiedUID != userID {
		return "", conversation.ErrInvalidOTP
	}
	return notifications.MaskPhone(normPhone), nil
}

// ---- conversation.Backend implementation -----------------------------------

func (s *Service) ResolveCaller(ctx context.Context, rawPhone string, now time.Time) (conversation.CallerIdentity, error) {
	normPhone, err := notifications.NormalizePhone(rawPhone)
	if err != nil {
		return conversation.CallerIdentity{}, err
	}
	phoneHMAC := notifications.DestinationHMAC(s.box, normPhone)
	var out conversation.CallerIdentity

	err = s.st.InTx(ctx, func(r store.Repos) error {
		link, lErr := r.Offline().GetPhoneLinkForUpdate(ctx, phoneHMAC)
		if errors.Is(lErr, store.ErrNotFound) {
			// Check whether an OIDC-verified phone from M5 already exists in users.
			oidcUID, fErr := r.Offline().FindUserIDByPhoneHMAC(ctx, phoneHMAC)
			if fErr == nil && oidcUID != "" {
				link = &conversation.PhoneLink{
					PhoneHMAC:    phoneHMAC,
					UserID:       oidcUID,
					Status:       conversation.LinkVerified,
					VerifiedAt:   &now,
					LastActiveAt: now,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
				if err := r.Offline().UpsertPhoneLink(ctx, link); err != nil {
					return err
				}
			} else if fErr != nil && !errors.Is(fErr, store.ErrNotFound) {
				return fErr
			} else {
				// First-time caller: provision a channel user record keyed by phone HMAC.
				uid, uErr := r.Users().EnsureByExternalID(ctx, "phone-hmac:"+phoneHMAC)
				if uErr != nil {
					return uErr
				}
				ct, h, masked, sErr := notifications.SealUserPhone(s.box, uid, normPhone)
				if sErr != nil {
					return sErr
				}
				if err := r.Users().UpsertContacts(ctx, notifications.UserContacts{
					UserID:          uid,
					PhoneCiphertext: ct,
					PhoneHMAC:       h,
					PhoneMasked:     masked,
					PhoneKeyVersion: s.box.KeyVersion,
					UpdatedAt:       now,
				}); err != nil {
					return err
				}
				_ = r.Notifications().SetPreferences(ctx, uid, notifications.Preferences{
					SMS: true, Email: true, WhatsApp: true,
				}, now)
				link = &conversation.PhoneLink{
					PhoneHMAC:    phoneHMAC,
					UserID:       uid,
					Status:       conversation.LinkUnverified,
					LastActiveAt: now,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
				if err := r.Offline().UpsertPhoneLink(ctx, link); err != nil {
					return err
				}
			}
		} else if lErr != nil {
			return lErr
		} else {
			if link.NeedsReverification(now, s.engineCfg.ReverifyAfter) {
				link.Status = conversation.LinkReverifyRequired
				link.UpdatedAt = now
				if err := r.Offline().UpsertPhoneLink(ctx, link); err != nil {
					return err
				}
			} else if link.Status == conversation.LinkVerified {
				link.LastActiveAt = now
				link.UpdatedAt = now
				if err := r.Offline().UpsertPhoneLink(ctx, link); err != nil {
					return err
				}
			}
		}

		out = conversation.CallerIdentity{
			UserID:   link.UserID,
			Phone:    normPhone,
			HMAC:     phoneHMAC,
			Link:     link,
			Verified: link.IsVerified(now, s.engineCfg.ReverifyAfter),
		}
		return nil
	})
	return out, err
}

func (s *Service) RecordOptOut(ctx context.Context, ch conversation.Channel, _ string, phoneHMAC string, now time.Time) error {
	notifCh := notifications.SMS
	if ch == conversation.ChannelWhatsApp {
		notifCh = notifications.WhatsApp
	}
	return s.st.InTx(ctx, func(r store.Repos) error {
		_, err := r.Notifications().AddOptOut(ctx, notifications.OptOut{
			DestinationHMAC: phoneHMAC,
			Channel:         notifCh,
			CreatedAt:       now,
		})
		return err
	})
}

func (s *Service) CheckWhatsAppMessageOnce(ctx context.Context, msgID, phoneHMAC string, now time.Time) (bool, error) {
	var first bool
	err := s.st.InTx(ctx, func(r store.Repos) error {
		ins, err := r.Offline().RecordProcessedMessage(ctx, conversation.ChannelWhatsApp, msgID, phoneHMAC, now)
		if err != nil {
			return err
		}
		first = ins
		if ins {
			if err := r.Notifications().RecordWhatsAppInbound(ctx, phoneHMAC, now); err != nil {
				return err
			}
		}
		return nil
	})
	return first, err
}

func (s *Service) LoadOrCreateUSSDSession(ctx context.Context, sessionID, phoneHMAC, userID string, now time.Time) (*conversation.Session, bool, error) {
	var (
		sess    *conversation.Session
		created bool
	)
	err := s.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Offline().GetSessionByProviderIDForUpdate(ctx, conversation.ChannelUSSD, sessionID)
		if err == nil {
			sess = cur
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		ttl := s.engineCfg.USSDSessionTTL
		if ttl <= 0 {
			ttl = conversation.USSDSessionTTL
		}
		fresh := &conversation.Session{
			ID:                uuid.New(),
			Channel:           conversation.ChannelUSSD,
			ProviderSessionID: sessionID,
			PhoneHMAC:         phoneHMAC,
			UserID:            userID,
			State:             conversation.StateMenu,
			Step:              0,
			ExpiresAt:         now.Add(ttl),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := r.Offline().CreateSession(ctx, fresh); err != nil {
			return err
		}
		sess = fresh
		created = true
		return nil
	})
	return sess, created, err
}

func (s *Service) LoadActiveWhatsAppSession(ctx context.Context, phoneHMAC string, now time.Time) (*conversation.Session, error) {
	var sess *conversation.Session
	err := s.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Offline().GetActiveSessionForPhoneForUpdate(ctx, conversation.ChannelWhatsApp, phoneHMAC, now)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		sess = cur
		return err
	})
	return sess, err
}

func (s *Service) CreateWhatsAppSession(ctx context.Context, phoneHMAC, userID string, now time.Time) (*conversation.Session, error) {
	ttl := s.engineCfg.WhatsAppSessionTTL
	if ttl <= 0 {
		ttl = conversation.WhatsAppSessionTTL
	}
	sess := &conversation.Session{
		ID:                uuid.New(),
		Channel:           conversation.ChannelWhatsApp,
		ProviderSessionID: uuid.NewString(),
		PhoneHMAC:         phoneHMAC,
		UserID:            userID,
		State:             conversation.StateMenu,
		Step:              0,
		ExpiresAt:         now.Add(ttl),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	err := s.st.InTx(ctx, func(r store.Repos) error {
		return r.Offline().CreateSession(ctx, sess)
	})
	return sess, err
}

func (s *Service) SaveSession(ctx context.Context, sess *conversation.Session) error {
	return s.st.InTx(ctx, func(r store.Repos) error {
		return r.Offline().UpdateSession(ctx, sess)
	})
}

func (s *Service) ListSavedMeters(ctx context.Context, userID string) ([]conversation.MeterOption, error) {
	listed, err := s.st.Repos().Meters().ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]conversation.MeterOption, 0, len(listed))
	for _, l := range listed {
		out = append(out, conversation.MeterOption{
			MeterID: l.ID,
			Masked:  l.Masked,
			Disco:   l.DiscoCode,
			Label:   l.Label,
		})
	}
	return out, nil
}

func (s *Service) RegisterMeter(ctx context.Context, userID, disco, meterNumber string) (conversation.MeterOption, error) {
	m, err := s.orch.Meters.Register(ctx, userID, strings.TrimSpace(meterNumber), strings.TrimSpace(disco), "")
	if err != nil {
		return conversation.MeterOption{}, err
	}
	return conversation.MeterOption{
		MeterID: m.ID,
		Masked:  m.Masked,
		Disco:   m.DiscoCode,
	}, nil
}

func (s *Service) TodayPhoneSpendMinor(ctx context.Context, phoneHMAC string, now time.Time) (int64, error) {
	startUTC, endUTC := conversation.LagosDayBounds(now)
	return s.st.Repos().Offline().SumPhoneSpendMinor(ctx, phoneHMAC, startUTC, endUTC)
}

func (s *Service) IssueOTP(ctx context.Context, rawPhone, phoneHMAC, targetUserID string, now time.Time) error {
	code, err := conversation.GenerateOTPCode()
	if err != nil {
		return err
	}
	otpID := uuid.New()
	otp := &conversation.ChannelOTP{
		ID:           otpID,
		PhoneHMAC:    phoneHMAC,
		TargetUserID: targetUserID,
		CodeHMAC:     conversation.OTPCodeHMAC(s.box, otpID, code),
		Attempts:     0,
		ExpiresAt:    now.Add(conversation.OTPTTL),
		CreatedAt:    now,
	}
	err = s.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Offline().CreateOTP(ctx, otp); err != nil {
			return err
		}
		actor := audit.Actor{UserID: targetUserID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("offline.otp.issued", "channel_otp", otp.ID, map[string]any{
			"expires_at": otp.ExpiresAt.Format(time.RFC3339),
		}))
	})
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Meterly verification code: %s. Expires in 10 minutes. Never share this code.", code)
	return s.sendSMSDirect(ctx, rawPhone, phoneHMAC, body, otp.ID)
}

func (s *Service) VerifyOTP(ctx context.Context, rawPhone, phoneHMAC, candidate string, now time.Time) (string, bool, bool, error) {
	var (
		verifiedUID string
		matched     bool
		lockedOut   bool
	)
	err := s.st.InTx(ctx, func(r store.Repos) error {
		otp, err := r.Offline().GetLatestOTPForUpdate(ctx, phoneHMAC)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if otp.ConsumedAt != nil || !now.Before(otp.ExpiresAt) {
			return nil
		}
		if otp.Attempts >= conversation.OTPMaxAttempts {
			lockedOut = true
			return nil
		}
		otp.Attempts++
		if !conversation.VerifyOTPCode(s.box, otp.ID, candidate, otp.CodeHMAC) {
			if otp.Attempts >= conversation.OTPMaxAttempts {
				lockedOut = true
			}
			return r.Offline().UpdateOTP(ctx, otp)
		}

		otp.ConsumedAt = &now
		if err := r.Offline().UpdateOTP(ctx, otp); err != nil {
			return err
		}

		ct, h, masked, sErr := notifications.SealUserPhone(s.box, otp.TargetUserID, rawPhone)
		if sErr != nil {
			return sErr
		}
		if err := r.Users().UpsertContacts(ctx, notifications.UserContacts{
			UserID:          otp.TargetUserID,
			PhoneCiphertext: ct,
			PhoneHMAC:       h,
			PhoneMasked:     masked,
			PhoneKeyVersion: s.box.KeyVersion,
			UpdatedAt:       now,
		}); err != nil {
			return err
		}
		link := &conversation.PhoneLink{
			PhoneHMAC:    phoneHMAC,
			UserID:       otp.TargetUserID,
			Status:       conversation.LinkVerified,
			VerifiedAt:   &now,
			LastActiveAt: now,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := r.Offline().UpsertPhoneLink(ctx, link); err != nil {
			return err
		}
		verifiedUID = otp.TargetUserID
		matched = true
		actor := audit.Actor{UserID: otp.TargetUserID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("offline.phone.verified", "channel_otp", otp.ID, map[string]any{
			"status": string(conversation.LinkVerified),
		}))
	})
	return verifiedUID, matched, lockedOut, err
}

func (s *Service) ExecutePurchase(ctx context.Context, req conversation.PurchaseRequest) (conversation.PurchaseResult, error) {
	capMinor := s.engineCfg.UnverifiedDailyCapMinor
	if req.Verified {
		capMinor = s.engineCfg.VerifiedDailyCapMinor
	}
	startUTC, endUTC := conversation.LagosDayBounds(req.Now)

	// Enforce per-phone daily channel purchase cap under user row lock before quoting.
	if err := s.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, req.UserID); err != nil {
			return err
		}
		spent, err := r.Offline().SumPhoneSpendMinor(ctx, req.PhoneHMAC, startUTC, endUTC)
		if err != nil {
			return err
		}
		if spent+req.AmountMinor > capMinor {
			return conversation.ErrDailyCapExceeded
		}
		return nil
	}); err != nil {
		return conversation.PurchaseResult{}, err
	}

	q, err := s.orch.CreateQuote(ctx, req.UserID, req.MeterID, req.AmountMinor, orchestrator.PrefAuto)
	if err != nil {
		return conversation.PurchaseResult{}, err
	}
	email := s.resolveChargeEmail(ctx, req.UserID, req.PhoneHMAC)
	idemKey := conversation.IdempotencyKey(req.Channel, req.ProviderSessionID, req.Step)
	reqHash := s.box.Hash(fmt.Sprintf("offline-purchase:%s:%s:%d", req.Channel, req.MeterID, req.AmountMinor))

	t, _, err := s.orch.CreateTransaction(ctx, orchestrator.CreateRequest{
		CustomerID:     req.UserID,
		QuoteID:        q.ID,
		Email:          email,
		IdempotencyKey: idemKey,
		RequestHash:    reqHash,
	})
	if err != nil {
		return conversation.PurchaseResult{}, err
	}

	if err := s.st.InTx(ctx, func(r store.Repos) error {
		pErr := r.Offline().RecordPurchase(ctx, &conversation.OfflinePurchase{
			TransactionID:     t.ID,
			UserID:            req.UserID,
			Channel:           req.Channel,
			PhoneHMAC:         req.PhoneHMAC,
			ProviderSessionID: req.ProviderSessionID,
			AmountMinor:       req.AmountMinor,
			Currency:          money.NGN,
			CreatedAt:         req.Now,
		})
		if errors.Is(pErr, store.ErrDuplicate) {
			return nil
		}
		return pErr
	}); err != nil {
		return conversation.PurchaseResult{}, err
	}

	if req.Channel == conversation.ChannelUSSD {
		smsBody := fmt.Sprintf("Meterly: Complete %s payment for meter %s (%s): %s",
			notifications.FormatNairaMinor(t.Total.Minor), req.MeterMasked, req.Disco, t.CheckoutURL)
		_ = s.sendSMSDirect(ctx, req.Phone, req.PhoneHMAC, smsBody, t.ID)
	}

	return conversation.PurchaseResult{
		TransactionID: t.ID,
		CheckoutURL:   t.CheckoutURL,
		TotalMinor:    t.Total.Minor,
		FeeMinor:      t.Fee.Minor,
	}, nil
}

func (s *Service) ResendLastToken(ctx context.Context, ch conversation.Channel, rawPhone, phoneHMAC, userID string, now time.Time) (conversation.ResendResult, error) {
	startUTC, endUTC := conversation.LagosDayBounds(now)
	var (
		res       conversation.ResendResult
		msg       notifications.Content
		tplParams []string
		inWindow  bool
	)

	err := s.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, userID); err != nil {
			return err
		}
		cnt, err := r.Offline().CountTokenResends(ctx, userID, startUTC, endUTC)
		if err != nil {
			return err
		}
		if cnt >= conversation.MaxDailyTokenResends {
			return conversation.ErrResendLimitReached
		}

		txns, err := r.Transactions().ListForCustomer(ctx, userID)
		if err != nil {
			return err
		}
		sort.Slice(txns, func(i, j int) bool {
			return txns[i].CreatedAt.After(txns[j].CreatedAt)
		})

		var latest *tx.Transaction
		for _, item := range txns {
			// Strictly caller's own purchases only (never delegated/other users' purchases).
			if item.CustomerID == userID && item.Vending == tx.VendConfirmed && len(item.TokenCiphertext) > 0 {
				latest = item
				break
			}
		}
		if latest == nil {
			return conversation.ErrNoSavedToken
		}

		rawToken, err := s.box.Decrypt(latest.TokenCiphertext)
		if err != nil {
			return err
		}
		m, err := r.Meters().Get(ctx, latest.MeterID)
		if err != nil {
			return err
		}
		content, params, _, err := notifications.RenderOfflineToken(notifications.LocaleEN, latest.Amount.Minor, m.Masked, rawToken)
		if err != nil {
			return err
		}

		resendID := uuid.New()
		if err := r.Offline().RecordTokenResend(ctx, &conversation.TokenResend{
			ID:            resendID,
			UserID:        userID,
			TransactionID: latest.ID,
			Channel:       ch,
			PhoneHMAC:     phoneHMAC,
			CreatedAt:     now,
		}); err != nil {
			return err
		}
		if ch == conversation.ChannelWhatsApp {
			inWindow, _ = r.Notifications().HasActiveWhatsAppWindow(ctx, phoneHMAC, now)
		}

		res = conversation.ResendResult{
			TransactionID: latest.ID,
			MeterMasked:   notifications.ShortMaskedMeter(m.Masked),
			AmountMinor:   latest.Amount.Minor,
			ResendsToday:  cnt + 1,
		}
		msg = content
		tplParams = params

		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("offline.token.resent", "transaction", latest.ID, map[string]any{
			"channel":            string(ch),
			"resend_count_today": cnt + 1,
		}))
	})
	if err != nil {
		return conversation.ResendResult{}, err
	}

	if ch == conversation.ChannelUSSD {
		if err := s.sendSMSDirect(ctx, rawPhone, phoneHMAC, msg.Body, res.TransactionID); err != nil {
			return conversation.ResendResult{}, err
		}
	} else {
		tplName := ""
		if !inWindow {
			tplName = notifications.WhatsAppTemplateName(notifications.TemplateOfflineToken)
		}
		if err := s.sendWhatsAppDirect(ctx, rawPhone, phoneHMAC, msg.Body, tplName, tplParams, now); err != nil {
			return conversation.ResendResult{}, err
		}
	}
	return res, nil
}

func (s *Service) resolveChargeEmail(ctx context.Context, userID, phoneHMAC string) string {
	if c, err := s.st.Repos().Users().Contacts(ctx, userID); err == nil && len(c.EmailCiphertext) > 0 {
		if plain, decErr := notifications.OpenUserEmail(s.box, userID, c.EmailCiphertext); decErr == nil && plain != "" {
			return plain
		}
	}
	return conversation.SyntheticChargeEmail(phoneHMAC)
}

func (s *Service) sendSMSDirect(ctx context.Context, normPhone, phoneHMAC, body string, refID uuid.UUID) error {
	if s.smsProv == nil {
		return nil
	}
	if optedOut, err := s.st.Repos().Notifications().IsOptedOut(ctx, phoneHMAC, notifications.SMS); err != nil || optedOut {
		return err
	}
	delivID := uuid.New()
	_, err := s.smsProv.SendDelivery(ctx, notifications.SendRequest{
		DeliveryID:     delivID,
		IdempotencyKey: "offline-sms:" + delivID.String(),
		Locale:         notifications.LocaleEN,
		Message: notifications.Message{
			EventID:       refID,
			TransactionID: refID,
			Channel:       notifications.SMS,
			Destination:   normPhone,
			Body:          body,
		},
	})
	return err
}

func (s *Service) sendWhatsAppDirect(ctx context.Context, normPhone, phoneHMAC, body, tplName string, tplParams []string, now time.Time) error {
	if s.waProv == nil {
		return nil
	}
	if optedOut, err := s.st.Repos().Notifications().IsOptedOut(ctx, phoneHMAC, notifications.WhatsApp); err != nil || optedOut {
		return err
	}
	inWindow := true
	if tplName != "" {
		inWindow = false
	} else {
		inWindow, _ = s.st.Repos().Notifications().HasActiveWhatsAppWindow(ctx, phoneHMAC, now)
	}
	delivID := uuid.New()
	_, err := s.waProv.SendDelivery(ctx, notifications.SendRequest{
		DeliveryID:                  delivID,
		IdempotencyKey:              "offline-wa:" + delivID.String(),
		Locale:                      notifications.LocaleEN,
		WhatsAppTemplateName:        tplName,
		TemplateParams:              tplParams,
		WithinCustomerServiceWindow: inWindow,
		PreferTextInWindow:          tplName == "",
		Message: notifications.Message{
			EventID:     delivID,
			Channel:     notifications.WhatsApp,
			Destination: normPhone,
			Body:        body,
		},
	})
	return err
}
