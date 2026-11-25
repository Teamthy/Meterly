package conversation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
)

var ErrNoSavedToken = errors.New("conversation: no completed token purchase found")

// PurchaseRequest describes an offline channel purchase confirmed by the caller.
type PurchaseRequest struct {
	Channel           Channel
	ProviderSessionID string
	Step              int
	UserID            string
	Phone             string
	PhoneHMAC         string
	Verified          bool
	MeterID           uuid.UUID
	MeterMasked       string
	Disco             string
	AmountMinor       int64
	Now               time.Time
}

// PurchaseResult holds the checkout link initialized via Orchestrator.
type PurchaseResult struct {
	TransactionID uuid.UUID
	CheckoutURL   string
	TotalMinor    int64
	FeeMinor      int64
}

// ResendResult holds metadata of an audited caller token resend.
type ResendResult struct {
	TransactionID uuid.UUID
	MeterMasked   string
	AmountMinor   int64
	ResendsToday  int
}

// CallerIdentity reflects the resolved user and verification tier for a caller phone.
type CallerIdentity struct {
	UserID   string
	Phone    string
	HMAC     string
	Link     *PhoneLink
	Verified bool
}

// Backend abstracts persistence, meter registration, Orchestrator purchase
// execution, OTP SMS dispatch, and audited token resends so the state machine
// remains pure, deterministic, and testable with a fake clock.
type Backend interface {
	ResolveCaller(ctx context.Context, rawPhone string, now time.Time) (CallerIdentity, error)
	RecordOptOut(ctx context.Context, ch Channel, rawPhone, phoneHMAC string, now time.Time) error
	CheckWhatsAppMessageOnce(ctx context.Context, msgID, phoneHMAC string, now time.Time) (firstTime bool, err error)

	LoadOrCreateUSSDSession(ctx context.Context, sessionID, phoneHMAC, userID string, now time.Time) (sess *Session, created bool, err error)
	LoadActiveWhatsAppSession(ctx context.Context, phoneHMAC string, now time.Time) (*Session, error)
	CreateWhatsAppSession(ctx context.Context, phoneHMAC, userID string, now time.Time) (*Session, error)
	SaveSession(ctx context.Context, sess *Session) error

	ListSavedMeters(ctx context.Context, userID string) ([]MeterOption, error)
	RegisterMeter(ctx context.Context, userID, disco, meterNumber string) (MeterOption, error)
	TodayPhoneSpendMinor(ctx context.Context, phoneHMAC string, now time.Time) (int64, error)

	IssueOTP(ctx context.Context, rawPhone, phoneHMAC, targetUserID string, now time.Time) error
	VerifyOTP(ctx context.Context, rawPhone, phoneHMAC, candidate string, now time.Time) (verifiedUserID string, ok bool, lockedOut bool, err error)

	ExecutePurchase(ctx context.Context, req PurchaseRequest) (PurchaseResult, error)
	ResendLastToken(ctx context.Context, ch Channel, rawPhone, phoneHMAC, userID string, now time.Time) (ResendResult, error)
}

type EngineConfig struct {
	MinAmountMinor          int64
	MaxAmountMinor          int64
	FeeMinor                int64
	UnverifiedDailyCapMinor int64
	VerifiedDailyCapMinor   int64
	ReverifyAfter           time.Duration
	USSDSessionTTL          time.Duration
	WhatsAppSessionTTL      time.Duration
	Now                     func() time.Time
}

func (c EngineConfig) withDefaults() EngineConfig {
	if c.MinAmountMinor <= 0 {
		c.MinAmountMinor = 50_000 // ₦500
	}
	if c.MaxAmountMinor <= 0 {
		c.MaxAmountMinor = 20_000_000 // ₦200,000
	}
	if c.FeeMinor <= 0 {
		c.FeeMinor = 10_000 // ₦100
	}
	if c.UnverifiedDailyCapMinor <= 0 {
		c.UnverifiedDailyCapMinor = DefaultUnverifiedDailyCapMinor
	}
	if c.VerifiedDailyCapMinor <= 0 {
		c.VerifiedDailyCapMinor = DefaultVerifiedDailyCapMinor
	}
	if c.ReverifyAfter <= 0 {
		c.ReverifyAfter = DefaultReverifyAfter
	}
	if c.USSDSessionTTL <= 0 {
		c.USSDSessionTTL = USSDSessionTTL
	}
	if c.WhatsAppSessionTTL <= 0 {
		c.WhatsAppSessionTTL = WhatsAppSessionTTL
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type Engine struct {
	cfg EngineConfig
	b   Backend
}

func NewEngine(cfg EngineConfig, b Backend) *Engine {
	return &Engine{cfg: cfg.withDefaults(), b: b}
}

type USSDTurnInput struct {
	SessionID   string
	ServiceCode string
	PhoneNumber string
	RawText     string
}

type WhatsAppTurnInput struct {
	MessageID   string
	PhoneNumber string
	Text        string
	Timestamp   time.Time
}

type WhatsAppTurnOutput struct {
	Reply             string
	Duplicate         bool
	IgnoredOutOfOrder bool
	OptedOut          bool
}

// HandleUSSD processes a single Africa's Talking USSD hop and returns a
// response starting with "CON " or "END " guaranteed <= 182 characters.
func (e *Engine) HandleUSSD(ctx context.Context, in USSDTurnInput) (string, error) {
	now := e.cfg.Now().UTC()
	caller, err := e.b.ResolveCaller(ctx, in.PhoneNumber, now)
	if err != nil {
		return "", err
	}

	latest := ExtractUSSDLatestInput(in.RawText)
	if notifications.IsStopKeyword(latest) {
		if err := e.b.RecordOptOut(ctx, ChannelUSSD, caller.Phone, caller.HMAC, now); err != nil {
			return "", err
		}
		return FormatUSSDResponse(false, "You have opted out of Meterly SMS alerts."), nil
	}

	sess, created, err := e.b.LoadOrCreateUSSDSession(ctx, in.SessionID, caller.HMAC, caller.UserID, now)
	if err != nil {
		return "", err
	}

	// Duplicate aggregator callback: return exact cached reply without re-running step.
	if !created && sess.HasCachedTurn && sess.LastRawInput == in.RawText {
		return sess.LastReply, nil
	}

	// Expired or already-terminated USSD session: never create a transaction after timeout.
	if !created && sess.Expired(now) {
		if !sess.Terminal() {
			sess.State = StateExpired
			sess.UpdatedAt = now
			_ = e.b.SaveSession(ctx, sess)
		}
		return FormatUSSDResponse(false, "Session timed out. Dial again to start. No payment was created."), nil
	}

	sess.UserID = caller.UserID
	var (
		cont bool
		body string
	)
	if created && latest == "" {
		cont = true
		body = renderUSSDMenu(caller)
	} else {
		sess.Step++
		cont, body, err = e.step(ctx, sess, caller, latest, now)
		if err != nil {
			return "", err
		}
	}

	reply := FormatUSSDResponse(cont, body)
	sess.LastRawInput = in.RawText
	sess.LastReply = reply
	sess.HasCachedTurn = true
	sess.UpdatedAt = now
	if cont {
		sess.ExpiresAt = now.Add(e.cfg.USSDSessionTTL)
	}
	if err := e.b.SaveSession(ctx, sess); err != nil {
		return "", err
	}
	return reply, nil
}

// HandleWhatsApp processes a verified inbound WhatsApp user message.
func (e *Engine) HandleWhatsApp(ctx context.Context, in WhatsAppTurnInput) (WhatsAppTurnOutput, error) {
	now := e.cfg.Now().UTC()
	caller, err := e.b.ResolveCaller(ctx, in.PhoneNumber, now)
	if err != nil {
		return WhatsAppTurnOutput{}, err
	}

	if strings.TrimSpace(in.MessageID) != "" {
		first, err := e.b.CheckWhatsAppMessageOnce(ctx, in.MessageID, caller.HMAC, now)
		if err != nil {
			return WhatsAppTurnOutput{}, err
		}
		if !first {
			return WhatsAppTurnOutput{Duplicate: true}, nil
		}
	}

	trimmed := strings.TrimSpace(in.Text)
	if notifications.IsStopKeyword(trimmed) {
		if err := e.b.RecordOptOut(ctx, ChannelWhatsApp, caller.Phone, caller.HMAC, now); err != nil {
			return WhatsAppTurnOutput{}, err
		}
		if sess, _ := e.b.LoadActiveWhatsAppSession(ctx, caller.HMAC, now); sess != nil {
			sess.State = StateCancelled
			sess.UpdatedAt = now
			_ = e.b.SaveSession(ctx, sess)
		}
		return WhatsAppTurnOutput{OptedOut: true}, nil
	}

	sess, err := e.b.LoadActiveWhatsAppSession(ctx, caller.HMAC, now)
	if err != nil {
		return WhatsAppTurnOutput{}, err
	}

	// Out-of-order WhatsApp message: ignore older timestamp so an active prompt is not corrupted.
	if sess != nil && !in.Timestamp.IsZero() && !sess.LastInboundMsgTS.IsZero() && in.Timestamp.Before(sess.LastInboundMsgTS) {
		return WhatsAppTurnOutput{IgnoredOutOfOrder: true}, nil
	}

	lower := strings.ToLower(trimmed)
	if sess != nil && (lower == "reset" || lower == "menu" || lower == "hi" || lower == "hello" || lower == "buy" || lower == "token" || lower == "last token") {
		if sess.State != StateMenu {
			sess.State = StateCancelled
			sess.UpdatedAt = now
			if err := e.b.SaveSession(ctx, sess); err != nil {
				return WhatsAppTurnOutput{}, err
			}
			sess = nil
		}
	}

	if sess == nil {
		sess, err = e.b.CreateWhatsAppSession(ctx, caller.HMAC, caller.UserID, now)
		if err != nil {
			return WhatsAppTurnOutput{}, err
		}
	}

	sess.UserID = caller.UserID
	sess.Step++
	if !in.Timestamp.IsZero() && in.Timestamp.After(sess.LastInboundMsgTS) {
		sess.LastInboundMsgTS = in.Timestamp
	}

	// Map friendly WhatsApp keywords at the root menu onto canonical menu options.
	effectiveInput := trimmed
	if sess.State == StateMenu {
		switch lower {
		case "buy":
			effectiveInput = "2"
		case "my meter":
			effectiveInput = "1"
		case "token", "last token":
			effectiveInput = "3"
		case "help":
			effectiveInput = "4"
		case "verify", "link":
			effectiveInput = "5"
		case "hi", "hello", "menu", "start":
			sess.ExpiresAt = now.Add(e.cfg.WhatsAppSessionTTL)
			sess.UpdatedAt = now
			body := renderWhatsAppMenu(caller)
			sess.LastReply = body
			sess.HasCachedTurn = true
			if err := e.b.SaveSession(ctx, sess); err != nil {
				return WhatsAppTurnOutput{}, err
			}
			return WhatsAppTurnOutput{Reply: body}, nil
		}
	}

	cont, body, err := e.step(ctx, sess, caller, effectiveInput, now)
	if err != nil {
		return WhatsAppTurnOutput{}, err
	}
	sess.LastRawInput = trimmed
	sess.LastReply = body
	sess.HasCachedTurn = true
	sess.UpdatedAt = now
	if cont {
		sess.ExpiresAt = now.Add(e.cfg.WhatsAppSessionTTL)
	}
	if err := e.b.SaveSession(ctx, sess); err != nil {
		return WhatsAppTurnOutput{}, err
	}
	return WhatsAppTurnOutput{Reply: body}, nil
}

func renderUSSDMenu(caller CallerIdentity) string {
	if caller.Link != nil && caller.Link.Status == LinkReverifyRequired {
		return "Meterly\n1. Buy for my meter\n2. Buy for saved meter\n3. Last token\n4. Help\n5. Re-verify phone"
	}
	if !caller.Verified {
		return "Meterly\n1. Buy for my meter\n2. Buy for saved meter\n3. Last token\n4. Help\n5. Verify phone"
	}
	return "Meterly\n1. Buy for my meter\n2. Buy for saved meter\n3. Last token\n4. Help"
}

func renderWhatsAppMenu(caller CallerIdentity) string {
	base := "Meterly Power:\n1. Buy for my meter\n2. Buy for saved meter\n3. Last token\n4. Help"
	if !caller.Verified {
		base += "\n5. Verify phone (SMS code)"
	}
	return base + "\nReply with a number or 'buy'."
}

func (e *Engine) step(ctx context.Context, sess *Session, caller CallerIdentity, input string, now time.Time) (cont bool, reply string, err error) {
	in := strings.TrimSpace(input)
	switch sess.State {
	case StateMenu:
		return e.handleMenu(ctx, sess, caller, in, now)
	case StateAwaitOTP:
		return e.handleOTP(ctx, sess, caller, in, now)
	case StateAwaitMeterChoice:
		return e.handleMeterChoice(ctx, sess, in)
	case StateAwaitDisco:
		return e.handleDisco(sess, in)
	case StateAwaitMeterNumber:
		return e.handleMeterNumber(ctx, sess, caller, in)
	case StateAwaitAmount:
		return e.handleAmount(ctx, sess, caller, in, now)
	case StateAwaitConfirm:
		return e.handleConfirm(ctx, sess, caller, in, now)
	default:
		sess.State = StateCancelled
		return false, "Session ended. Start again.", nil
	}
}

func (e *Engine) handleMenu(ctx context.Context, sess *Session, caller CallerIdentity, in string, now time.Time) (bool, string, error) {
	switch in {
	case "1": // Buy for my meter
		meters, err := e.b.ListSavedMeters(ctx, caller.UserID)
		if err != nil {
			return false, "", err
		}
		if len(meters) == 1 {
			m := meters[0]
			sess.SelectedMeterID = &m.MeterID
			sess.SelectedMeterMasked = notifications.ShortMaskedMeter(m.Masked)
			sess.SelectedDisco = m.Disco
			sess.State = StateAwaitAmount
			return true, fmt.Sprintf("Meter %s (%s)\nEnter amount in Naira:", sess.SelectedMeterMasked, m.Disco), nil
		}
		if len(meters) > 1 {
			sess.MeterOptions = meters
			sess.State = StateAwaitMeterChoice
			return true, formatMeterChoicePrompt(sess.Channel, meters), nil
		}
		sess.State = StateAwaitDisco
		return true, renderDiscoPrompt(), nil

	case "2": // Buy for saved meter (or WhatsApp "buy")
		meters, err := e.b.ListSavedMeters(ctx, caller.UserID)
		if err != nil {
			return false, "", err
		}
		if len(meters) == 0 {
			sess.State = StateAwaitDisco
			return true, "No saved meters yet.\n" + renderDiscoPrompt(), nil
		}
		sess.MeterOptions = meters
		sess.State = StateAwaitMeterChoice
		return true, formatMeterChoicePrompt(sess.Channel, meters), nil

	case "3": // Last token (caller's own purchases only, max 3/day, requires verified non-stale phone)
		if !caller.Verified {
			if err := e.b.IssueOTP(ctx, caller.Phone, caller.HMAC, caller.UserID, now); err != nil {
				return false, "", err
			}
			sess.State = StateAwaitOTP
			sess.PostOTPAction = "LAST_TOKEN"
			if caller.Link != nil && caller.Link.Status == LinkReverifyRequired {
				return true, "Inactive phone: enter 6-digit SMS code to re-verify:", nil
			}
			return true, "Verify your phone first. Enter 6-digit SMS code:", nil
		}
		return e.runLastToken(ctx, sess, caller, now)

	case "4": // Help
		sess.State = StateCompleted
		return false, "Meterly: Buy power via *code# or WhatsApp ('buy'). Token arrives by SMS/chat after payment. Reply STOP to opt out.", nil

	case "5": // Verify phone via SMS OTP
		if err := e.b.IssueOTP(ctx, caller.Phone, caller.HMAC, caller.UserID, now); err != nil {
			return false, "", err
		}
		sess.State = StateAwaitOTP
		sess.PostOTPAction = ""
		return true, "Enter the 6-digit code sent to your phone by SMS:", nil

	default:
		if sess.Channel == ChannelWhatsApp {
			return true, renderWhatsAppMenu(caller), nil
		}
		return true, "Invalid option.\n" + renderUSSDMenu(caller), nil
	}
}

func (e *Engine) handleOTP(ctx context.Context, sess *Session, caller CallerIdentity, in string, now time.Time) (bool, string, error) {
	verifiedUID, ok, lockedOut, err := e.b.VerifyOTP(ctx, caller.Phone, caller.HMAC, in, now)
	if err != nil {
		return false, "", err
	}
	if lockedOut {
		sess.State = StateCancelled
		return false, "Too many wrong codes. Dial or chat again to request a new code.", nil
	}
	if !ok {
		return true, "Incorrect or expired 6-digit code. Try again:", nil
	}
	sess.UserID = verifiedUID
	caller.UserID = verifiedUID
	caller.Verified = true
	action := sess.PostOTPAction
	sess.PostOTPAction = ""

	switch action {
	case "LAST_TOKEN":
		return e.runLastToken(ctx, sess, caller, now)
	case "BUY_AMOUNT":
		sess.State = StateAwaitAmount
		return true, fmt.Sprintf("Phone verified. Enter amount in Naira for %s (%s):", sess.SelectedMeterMasked, sess.SelectedDisco), nil
	default:
		sess.State = StateMenu
		if sess.Channel == ChannelWhatsApp {
			return true, "Phone verified!\n" + renderWhatsAppMenu(caller), nil
		}
		return true, "Phone verified!\n" + renderUSSDMenu(caller), nil
	}
}

func (e *Engine) runLastToken(ctx context.Context, sess *Session, caller CallerIdentity, now time.Time) (bool, string, error) {
	res, err := e.b.ResendLastToken(ctx, sess.Channel, caller.Phone, caller.HMAC, caller.UserID, now)
	if errors.Is(err, ErrNoSavedToken) {
		sess.State = StateCompleted
		return false, "No completed token purchase found on your account.", nil
	}
	if errors.Is(err, ErrResendLimitReached) {
		sess.State = StateCompleted
		return false, "Daily limit reached (max 3 token resends/day). Try again tomorrow.", nil
	}
	if err != nil {
		return false, "", err
	}
	sess.State = StateCompleted
	if sess.Channel == ChannelUSSD {
		return false, fmt.Sprintf("Last token for %s sent by SMS (%d/3 today).", res.MeterMasked, res.ResendsToday), nil
	}
	return false, fmt.Sprintf("Resent your latest token for meter %s (%d/%d today).", res.MeterMasked, res.ResendsToday, MaxDailyTokenResends), nil
}

func (e *Engine) handleMeterChoice(ctx context.Context, sess *Session, in string) (bool, string, error) {
	if in == "0" || strings.EqualFold(in, "new") {
		sess.State = StateAwaitDisco
		return true, renderDiscoPrompt(), nil
	}
	idx, err := strconv.Atoi(in)
	if err != nil || idx < 1 || idx > len(sess.MeterOptions) {
		return true, "Invalid selection.\n" + formatMeterChoicePrompt(sess.Channel, sess.MeterOptions), nil
	}
	chosen := sess.MeterOptions[idx-1]
	sess.SelectedMeterID = &chosen.MeterID
	sess.SelectedMeterMasked = notifications.ShortMaskedMeter(chosen.Masked)
	sess.SelectedDisco = chosen.Disco
	sess.State = StateAwaitAmount
	return true, fmt.Sprintf("Meter %s (%s)\nEnter amount in Naira:", sess.SelectedMeterMasked, chosen.Disco), nil
}

func (e *Engine) handleDisco(sess *Session, in string) (bool, string, error) {
	disco, ok := ResolveDisco(in)
	if !ok {
		return true, "Invalid DisCo.\n" + renderDiscoPrompt(), nil
	}
	sess.SelectedDisco = disco
	sess.State = StateAwaitMeterNumber
	return true, fmt.Sprintf("Enter 11-13 digit %s meter number:", disco), nil
}

func (e *Engine) handleMeterNumber(ctx context.Context, sess *Session, caller CallerIdentity, in string) (bool, string, error) {
	m, err := e.b.RegisterMeter(ctx, caller.UserID, sess.SelectedDisco, in)
	if err != nil {
		return true, fmt.Sprintf("Could not validate %s meter. Enter a valid 11-13 digit meter number:", sess.SelectedDisco), nil
	}
	sess.SelectedMeterID = &m.MeterID
	sess.SelectedMeterMasked = notifications.ShortMaskedMeter(m.Masked)
	sess.SelectedDisco = m.Disco
	sess.State = StateAwaitAmount
	return true, fmt.Sprintf("Meter %s (%s)\nEnter amount in Naira:", sess.SelectedMeterMasked, m.Disco), nil
}

func (e *Engine) handleAmount(ctx context.Context, sess *Session, caller CallerIdentity, in string, now time.Time) (bool, string, error) {
	if strings.EqualFold(in, "verify") {
		if err := e.b.IssueOTP(ctx, caller.Phone, caller.HMAC, caller.UserID, now); err != nil {
			return false, "", err
		}
		sess.State = StateAwaitOTP
		sess.PostOTPAction = "BUY_AMOUNT"
		return true, "Enter 6-digit SMS code to unlock full daily limit:", nil
	}

	amtMinor, err := ParseNairaAmountMinor(in)
	if err != nil || amtMinor < e.cfg.MinAmountMinor || amtMinor > e.cfg.MaxAmountMinor {
		return true, fmt.Sprintf("Enter whole Naira between N%d and N%d:",
			e.cfg.MinAmountMinor/100, e.cfg.MaxAmountMinor/100), nil
	}

	spentToday, err := e.b.TodayPhoneSpendMinor(ctx, caller.HMAC, now)
	if err != nil {
		return false, "", err
	}
	capMinor := e.cfg.UnverifiedDailyCapMinor
	if caller.Verified {
		capMinor = e.cfg.VerifiedDailyCapMinor
	}
	if spentToday+amtMinor > capMinor {
		remNaira := maxInt64(0, (capMinor-spentToday)/100)
		if !caller.Verified {
			return true, fmt.Sprintf("Unverified cap N%d/day (N%d left). Enter smaller amount or reply VERIFY:",
				capMinor/100, remNaira), nil
		}
		sess.State = StateCancelled
		return false, fmt.Sprintf("Daily channel limit N%d reached (N%d left today).", capMinor/100, remNaira), nil
	}

	sess.AmountMinor = amtMinor
	sess.FeeMinor = e.cfg.FeeMinor
	sess.TotalMinor = amtMinor + e.cfg.FeeMinor
	sess.State = StateAwaitConfirm
	return true, fmt.Sprintf("Buy N%d (+N%d fee = N%d) for %s (%s)?\n1. Confirm\n2. Cancel",
		amtMinor/100, e.cfg.FeeMinor/100, sess.TotalMinor/100, sess.SelectedMeterMasked, sess.SelectedDisco), nil
}

func (e *Engine) handleConfirm(ctx context.Context, sess *Session, caller CallerIdentity, in string, now time.Time) (bool, string, error) {
	lower := strings.ToLower(in)
	switch lower {
	case "2", "cancel", "no", "n":
		sess.State = StateCancelled
		return false, "Purchase cancelled. No payment was created.", nil
	case "1", "confirm", "yes", "y":
		if sess.SelectedMeterID == nil || sess.AmountMinor <= 0 {
			sess.State = StateCancelled
			return false, "Session invalid. Start again.", nil
		}
		res, err := e.b.ExecutePurchase(ctx, PurchaseRequest{
			Channel:           sess.Channel,
			ProviderSessionID: sess.ProviderSessionID,
			Step:              sess.Step,
			UserID:            caller.UserID,
			Phone:             caller.Phone,
			PhoneHMAC:         caller.HMAC,
			Verified:          caller.Verified,
			MeterID:           *sess.SelectedMeterID,
			MeterMasked:       sess.SelectedMeterMasked,
			Disco:             sess.SelectedDisco,
			AmountMinor:       sess.AmountMinor,
			Now:               now,
		})
		if errors.Is(err, ErrDailyCapExceeded) {
			sess.State = StateCancelled
			return false, "Daily channel purchase limit exceeded. No charge was made.", nil
		}
		if err != nil {
			return false, "", err
		}
		sess.State = StateCompleted
		if sess.Channel == ChannelUSSD {
			return false, fmt.Sprintf("Payment link sent by SMS for N%d (%s). Token arrives by SMS once paid.",
				res.TotalMinor/100, sess.SelectedMeterMasked), nil
		}
		return false, fmt.Sprintf("Pay N%d for meter %s (%s): %s\nYour token arrives here once vended.",
			res.TotalMinor/100, sess.SelectedMeterMasked, sess.SelectedDisco, res.CheckoutURL), nil
	default:
		return true, fmt.Sprintf("Reply 1 to confirm N%d for %s or 2 to cancel:",
			sess.TotalMinor/100, sess.SelectedMeterMasked), nil
	}
}

func renderDiscoPrompt() string {
	return "Select DisCo:\n1.IKEDC 2.EKEDC\n3.AEDC 4.IBEDC\n5.EEDC 6.PHED\n7.KEDCO 8.KAEDCO"
}

func formatMeterChoicePrompt(ch Channel, opts []MeterOption) string {
	var b strings.Builder
	b.WriteString("Select meter:\n")
	maxShow := len(opts)
	if ch == ChannelUSSD && maxShow > 4 {
		maxShow = 4
	}
	for i := 0; i < maxShow; i++ {
		m := opts[i]
		short := notifications.ShortMaskedMeter(m.Masked)
		lbl := strings.TrimSpace(m.Label)
		if ch == ChannelUSSD && len(lbl) > 8 {
			lbl = lbl[:8]
		}
		if lbl != "" {
			fmt.Fprintf(&b, "%d. %s (%s)\n", i+1, lbl, short)
		} else {
			fmt.Fprintf(&b, "%d. %s %s\n", i+1, short, m.Disco)
		}
	}
	b.WriteString("0. New meter")
	return b.String()
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
