// Package conversation implements the channel-agnostic state machine, domain
// types, OTP/SIM-swap verification rules, daily spend/resend bounds, and USSD
// 182-character formatting for Meterly Offline (USSD and WhatsApp channels).
package conversation

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/secure"
)

type Channel string

const (
	ChannelUSSD     Channel = "USSD"
	ChannelWhatsApp Channel = "WHATSAPP"
)

func (c Channel) Valid() bool {
	return c == ChannelUSSD || c == ChannelWhatsApp
}

type State string

const (
	StateMenu             State = "MENU"
	StateAwaitOTP         State = "AWAIT_OTP"
	StateAwaitMeterChoice State = "AWAIT_METER_CHOICE"
	StateAwaitDisco       State = "AWAIT_DISCO"
	StateAwaitMeterNumber State = "AWAIT_METER_NUMBER"
	StateAwaitAmount      State = "AWAIT_AMOUNT"
	StateAwaitConfirm     State = "AWAIT_CONFIRM"
	StateCompleted        State = "COMPLETED"
	StateCancelled        State = "CANCELLED"
	StateExpired          State = "EXPIRED"
)

func (s State) Terminal() bool {
	return s == StateCompleted || s == StateCancelled || s == StateExpired
}

type PhoneLinkStatus string

const (
	LinkUnverified       PhoneLinkStatus = "UNVERIFIED"
	LinkVerified         PhoneLinkStatus = "VERIFIED"
	LinkReverifyRequired PhoneLinkStatus = "REVERIFY_REQUIRED"
)

const (
	MaxUSSDChars                   = 182
	USSDSessionTTL                 = 180 * time.Second
	WhatsAppSessionTTL             = 30 * time.Minute
	OTPTTL                         = 10 * time.Minute
	OTPMaxAttempts                 = 5
	MaxDailyTokenResends           = 3
	DefaultReverifyAfter           = 30 * 24 * time.Hour
	DefaultUnverifiedDailyCapMinor = 500_000    // ₦5,000/day before OTP verification
	DefaultVerifiedDailyCapMinor   = 20_000_000 // ₦200,000/day after verification
)

var (
	ErrInvalidAmount      = errors.New("conversation: invalid Naira amount")
	ErrResendLimitReached = errors.New("conversation: daily token resend limit reached")
	ErrDailyCapExceeded   = errors.New("conversation: daily channel purchase limit exceeded")
	ErrInvalidOTP         = errors.New("conversation: invalid or expired verification code")
)

var lagosLoc = time.FixedZone("WAT", 1*60*60)

func init() {
	if loc, err := time.LoadLocation("Africa/Lagos"); err == nil {
		lagosLoc = loc
	}
}

// LagosDayBounds returns [startUTC, nextDayStartUTC) for at's calendar date in Africa/Lagos.
func LagosDayBounds(at time.Time) (startUTC, endUTC time.Time) {
	local := at.In(lagosLoc)
	startLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, lagosLoc)
	endLocal := startLocal.AddDate(0, 0, 1)
	return startLocal.UTC(), endLocal.UTC()
}

// MeterOption stores masked meter selection metadata in a conversation session
// (never the raw meter number).
type MeterOption struct {
	MeterID uuid.UUID `json:"meter_id"`
	Masked  string    `json:"masked"`
	Disco   string    `json:"disco"`
	Label   string    `json:"label,omitempty"`
}

// Session persists per-conversation state across USSD hops or WhatsApp turns.
type Session struct {
	ID                  uuid.UUID     `json:"id"`
	Channel             Channel       `json:"channel"`
	ProviderSessionID   string        `json:"provider_session_id"`
	PhoneHMAC           string        `json:"-"`
	UserID              string        `json:"user_id"`
	State               State         `json:"state"`
	Step                int           `json:"step"`
	MeterOptions        []MeterOption `json:"meter_options,omitempty"`
	SelectedMeterID     *uuid.UUID    `json:"selected_meter_id,omitempty"`
	SelectedMeterMasked string        `json:"selected_meter_masked,omitempty"`
	SelectedDisco       string        `json:"selected_disco,omitempty"`
	AmountMinor         int64         `json:"amount_minor,omitempty"`
	FeeMinor            int64         `json:"fee_minor,omitempty"`
	TotalMinor          int64         `json:"total_minor,omitempty"`
	PostOTPAction       string        `json:"post_otp_action,omitempty"` // "", "BUY", "LAST_TOKEN"
	LastRawInput        string        `json:"-"`
	LastReply           string        `json:"-"`
	HasCachedTurn       bool          `json:"-"`
	LastInboundMsgTS    time.Time     `json:"last_inbound_msg_ts"`
	ExpiresAt           time.Time     `json:"expires_at"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
}

func (s *Session) Expired(now time.Time) bool {
	return s.Terminal() || !now.Before(s.ExpiresAt)
}

func (s *Session) Terminal() bool {
	return s.State.Terminal()
}

// PhoneLink binds a normalized E.164 phone HMAC to a Meterly user account and
// tracks verification & SIM-swap inactivity timestamps.
type PhoneLink struct {
	PhoneHMAC    string          `json:"-"`
	UserID       string          `json:"user_id"`
	Status       PhoneLinkStatus `json:"status"`
	VerifiedAt   *time.Time      `json:"verified_at,omitempty"`
	LastActiveAt time.Time       `json:"last_active_at"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// NeedsReverification returns true when a previously verified link has been
// inactive for longer than maxInactivity (SIM-swap mitigation).
func (l *PhoneLink) NeedsReverification(now time.Time, maxInactivity time.Duration) bool {
	if l == nil || l.Status != LinkVerified {
		return false
	}
	if maxInactivity <= 0 {
		maxInactivity = DefaultReverifyAfter
	}
	if l.LastActiveAt.IsZero() {
		return false
	}
	return now.Sub(l.LastActiveAt) > maxInactivity
}

func (l *PhoneLink) IsVerified(now time.Time, maxInactivity time.Duration) bool {
	if l == nil || l.Status != LinkVerified {
		return false
	}
	return !l.NeedsReverification(now, maxInactivity)
}

// ChannelOTP stores a hashed, time-bounded 6-digit SMS verification code.
type ChannelOTP struct {
	ID           uuid.UUID  `json:"id"`
	PhoneHMAC    string     `json:"-"`
	TargetUserID string     `json:"target_user_id"`
	CodeHMAC     string     `json:"-"`
	Attempts     int        `json:"attempts"`
	ConsumedAt   *time.Time `json:"consumed_at,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// OfflinePurchase links an Orchestrator electricity transaction to the offline
// channel session that initiated it so daily channel caps and post-vend token
// delivery over SMS / WhatsApp can be resolved deterministically.
type OfflinePurchase struct {
	TransactionID     uuid.UUID      `json:"transaction_id"`
	UserID            string         `json:"user_id"`
	Channel           Channel        `json:"channel"`
	PhoneHMAC         string         `json:"-"`
	ProviderSessionID string         `json:"provider_session_id"`
	AmountMinor       int64          `json:"amount_minor"`
	Currency          money.Currency `json:"currency"`
	CreatedAt         time.Time      `json:"created_at"`
}

// TokenResend records an audited caller-initiated token resend for daily rate enforcement.
type TokenResend struct {
	ID            uuid.UUID `json:"id"`
	UserID        string    `json:"user_id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	Channel       Channel   `json:"channel"`
	PhoneHMAC     string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
}

// GenerateOTPCode returns a cryptographically random 6-digit decimal string.
func GenerateOTPCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(b[:]) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// OTPCodeHMAC computes a domain-separated HMAC for a 6-digit OTP bound to its row ID.
func OTPCodeHMAC(box *secure.Box, otpID uuid.UUID, code string) string {
	return box.Hash("channel-otp:" + otpID.String() + ":" + strings.TrimSpace(code))
}

// VerifyOTPCode compares a candidate 6-digit code in constant time.
func VerifyOTPCode(box *secure.Box, otpID uuid.UUID, candidate, expectedHMAC string) bool {
	c := strings.TrimSpace(candidate)
	if len(c) != 6 || expectedHMAC == "" {
		return false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return false
		}
	}
	got := OTPCodeHMAC(box, otpID, c)
	return subtle.ConstantTimeCompare([]byte(got), []byte(expectedHMAC)) == 1
}

// SyntheticChargeEmail derives a deterministic, non-PII Paystack checkout email
// when a phone-only user does not have a verified email address on file.
func SyntheticChargeEmail(phoneHMAC string) string {
	prefix := strings.TrimSpace(phoneHMAC)
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	if prefix == "" {
		prefix = "guest"
	}
	return "offline+" + prefix + "@meterly.ng"
}

// IdempotencyKey builds the deterministic purchase idempotency key mandated by
// the M11 specification: `<ussd|whatsapp>:<sessionId>:<step>`.
func IdempotencyKey(ch Channel, sessionID string, step int) string {
	kind := strings.ToLower(string(ch))
	return fmt.Sprintf("%s:%s:%d", kind, strings.TrimSpace(sessionID), step)
}

// FormatUSSDResponse prefixes "CON " or "END " and strictly enforces the
// 182-character/byte Africa's Talking USSD limit without splitting UTF-8 runes.
func FormatUSSDResponse(cont bool, body string) string {
	prefix := "END "
	if cont {
		prefix = "CON "
	}
	clean := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	full := prefix + clean
	if len(full) <= MaxUSSDChars && utf8.RuneCountInString(full) <= MaxUSSDChars {
		return full
	}
	budget := MaxUSSDChars - len(prefix) - 1 // reserve 1 byte for '…' or truncation safety
	if budget < 0 {
		return strings.TrimSpace(prefix)
	}
	var b strings.Builder
	b.WriteString(prefix)
	used := 0
	for _, r := range clean {
		rl := utf8.RuneLen(r)
		if rl < 0 {
			continue
		}
		if used+rl > budget {
			break
		}
		b.WriteRune(r)
		used += rl
	}
	out := strings.TrimRight(b.String(), " \n\t")
	if len(out)+1 <= MaxUSSDChars {
		out += "."
	}
	return out
}

// ExtractUSSDLatestInput extracts the newest hop from Africa's Talking
// cumulative `*`-separated `text` parameter (e.g. "" -> "", "1" -> "1",
// "1*2*5000" -> "5000").
func ExtractUSSDLatestInput(rawText string) string {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "*")
	return strings.TrimSpace(parts[len(parts)-1])
}

// ParseNairaAmountMinor parses user-entered Naira ("5000", "N5,000", "₦2500")
// into minor units (kobo). Only whole Naira amounts are accepted on USSD/chat.
func ParseNairaAmountMinor(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "₦")
	s = strings.TrimPrefix(s, "NGN")
	s = strings.TrimPrefix(s, "ngn")
	s = strings.TrimPrefix(s, "N")
	s = strings.TrimPrefix(s, "n")
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || len(s) > 9 {
		return 0, ErrInvalidAmount
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, ErrInvalidAmount
		}
	}
	naira, err := strconv.ParseInt(s, 10, 64)
	if err != nil || naira <= 0 {
		return 0, ErrInvalidAmount
	}
	return naira * 100, nil
}

var supportedDiscos = []string{
	"IKEDC",
	"EKEDC",
	"AEDC",
	"IBEDC",
	"EEDC",
	"PHED",
	"KEDCO",
	"KAEDCO",
}

func SupportedDiscos() []string {
	cp := make([]string, len(supportedDiscos))
	copy(cp, supportedDiscos)
	return cp
}

// ResolveDisco maps either a 1-based menu index ("1".."8") or DisCo code
// ("ikedc", "EKEDC") to a canonical uppercase DisCo code.
func ResolveDisco(input string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(input))
	if s == "" {
		return "", false
	}
	if idx, err := strconv.Atoi(s); err == nil {
		if idx >= 1 && idx <= len(supportedDiscos) {
			return supportedDiscos[idx-1], true
		}
		return "", false
	}
	for _, code := range supportedDiscos {
		if s == code {
			return code, true
		}
	}
	return "", false
}
