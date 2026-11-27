package notifications

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

type RecipientKind string

const (
	RecipientPurchaser      RecipientKind = "PURCHASER"
	RecipientGift           RecipientKind = "GIFT_RECIPIENT"
	RecipientPayer          RecipientKind = "PAYER"
	RecipientPotContributor RecipientKind = "POT_CONTRIBUTOR"
	RecipientTokenShare     RecipientKind = "TOKEN_SHARE"
)

type Status string

const (
	StatusPending    Status = "PENDING"
	StatusSending    Status = "SENDING"
	StatusSent       Status = "SENT"
	StatusFailed     Status = "FAILED"
	StatusUnknown    Status = "UNKNOWN"
	StatusSuppressed Status = "SUPPRESSED"
)

func (s Status) Terminal() bool {
	return s == StatusSent || s == StatusFailed || s == StatusSuppressed
}

const MaxAttempts = 5

var backoffSteps = [...]time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
}

// Backoff returns the retry delay after the given 1-based attempt number.
func Backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return backoffSteps[0]
	}
	idx := attempt - 1
	if idx >= len(backoffSteps) {
		idx = len(backoffSteps) - 1
	}
	return backoffSteps[idx]
}

var ErrInvalidStatusTransition = errors.New("notifications: invalid status transition")

// Delivery is the durable metadata record of one notification job. It never
// holds the rendered message body, raw token or plaintext destination.
type Delivery struct {
	ID              uuid.UUID     `json:"id"`
	EventID         uuid.UUID     `json:"event_id"`
	TransactionID   uuid.UUID     `json:"transaction_id"`
	UserID          string        `json:"-"` // set for account-level notifications without a transaction
	RecipientKind   RecipientKind `json:"recipient_kind"`
	RecipientSlot   string        `json:"-"`
	Channel         Channel       `json:"channel"`
	DestinationHMAC string        `json:"-"`
	TemplateID      string        `json:"template_id"`
	TemplateVersion string        `json:"template_version"`
	Locale          string        `json:"locale"`
	Status          Status        `json:"status"`
	Attempts        int           `json:"attempts"`
	NextAttemptAt   time.Time     `json:"next_attempt_at"`
	ProviderName    string        `json:"provider_name,omitempty"`
	ProviderRef     string        `json:"provider_ref,omitempty"`
	LastErrorCode   string        `json:"last_error_code,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	SentAt          *time.Time    `json:"sent_at,omitempty"`
}

func (d *Delivery) TokenBearing() bool {
	return IsTokenBearing(d.TemplateID)
}

func allowedTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusSending || to == StatusSuppressed || to == StatusFailed
	case StatusSending:
		return to == StatusSent || to == StatusPending || to == StatusFailed || to == StatusUnknown
	case StatusUnknown:
		return to == StatusSent || to == StatusFailed || to == StatusSending || to == StatusSuppressed
	default:
		return false
	}
}

func (d *Delivery) Transition(to Status, now time.Time) error {
	if !allowedTransition(d.Status, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidStatusTransition, d.Status, to)
	}
	d.Status = to
	d.UpdatedAt = now.UTC()
	return nil
}

// OperatorView is the redacted view returned on GET /v1/admin/transactions/{id}.
// It never exposes message content, raw destinations or destination HMACs.
type OperatorView struct {
	ID              uuid.UUID     `json:"id"`
	RecipientKind   RecipientKind `json:"recipient_kind"`
	Channel         Channel       `json:"channel"`
	TemplateID      string        `json:"template_id"`
	TemplateVersion string        `json:"template_version"`
	Status          Status        `json:"status"`
	Attempts        int           `json:"attempts"`
	LastErrorCode   string        `json:"last_error_code,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	NextAttemptAt   time.Time     `json:"next_attempt_at"`
	SentAt          *time.Time    `json:"sent_at,omitempty"`
}

func (d Delivery) RedactedView() OperatorView {
	return OperatorView{
		ID:              d.ID,
		RecipientKind:   d.RecipientKind,
		Channel:         d.Channel,
		TemplateID:      d.TemplateID,
		TemplateVersion: d.TemplateVersion,
		Status:          d.Status,
		Attempts:        d.Attempts,
		LastErrorCode:   d.LastErrorCode,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
		NextAttemptAt:   d.NextAttemptAt,
		SentAt:          d.SentAt,
	}
}

// Preferences controls transactional notifications per channel for an account holder.
type Preferences struct {
	SMS      bool `json:"sms"`
	Email    bool `json:"email"`
	WhatsApp bool `json:"whatsapp"`
}

// DefaultPreferences enables SMS and email for the account holder and leaves
// WhatsApp opt-in per WhatsApp Business Policy.
func DefaultPreferences() Preferences {
	return Preferences{SMS: true, Email: true, WhatsApp: false}
}

func (p Preferences) Enabled(ch Channel) bool {
	switch ch {
	case SMS:
		return p.SMS
	case Email:
		return p.Email
	case WhatsApp:
		return p.WhatsApp
	default:
		return false
	}
}

// OptOut records a global opt-out keyed by HMAC of the normalised destination.
type OptOut struct {
	DestinationHMAC string
	Channel         Channel
	CreatedAt       time.Time
}

// UserContacts holds encrypted, bound contact details for a user.
type UserContacts struct {
	UserID          string
	EmailCiphertext []byte
	EmailHMAC       string
	EmailMasked     string
	EmailKeyVersion string
	PhoneCiphertext []byte
	PhoneHMAC       string
	PhoneMasked     string
	PhoneKeyVersion string
	DisplayName     string
	UpdatedAt       time.Time
}

var (
	e164Re           = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	ErrInvalidEmail  = errors.New("notifications: invalid email address")
	ErrInvalidPhone  = errors.New("notifications: invalid E.164 phone number")
	ErrMissingUserID = errors.New("notifications: user ID is required")
)

func NormalizeEmail(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if len(s) < 3 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at >= len(s)-1 || !strings.Contains(s[at+1:], ".") {
		return "", ErrInvalidEmail
	}
	return s, nil
}

func NormalizePhone(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !e164Re.MatchString(s) {
		return "", ErrInvalidPhone
	}
	return s, nil
}

// DestinationHMAC computes the domain-separated HMAC of a normalised E.164
// phone or lowercase email address, shared across users, opt_outs and
// notification_deliveries.
func DestinationHMAC(box *secure.Box, normalized string) string {
	return box.Hash("destination:" + normalized)
}

func MaskEmail(normalized string) string {
	at := strings.LastIndex(normalized, "@")
	if at <= 0 {
		return "***"
	}
	local, domain := normalized[:at], normalized[at+1:]
	if len(local) <= 1 {
		return "***@" + domain
	}
	return local[:1] + "***@" + domain
}

func MaskPhone(normalized string) string {
	return secure.Mask(normalized, 4)
}

func SanitizeDisplayName(raw string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		n++
		if n >= 64 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func userEmailContext(userID string) []byte { return []byte("user-email:" + userID) }
func userPhoneContext(userID string) []byte { return []byte("user-phone:" + userID) }

func SealUserEmail(box *secure.Box, userID, rawEmail string) (ct []byte, hmacHex, masked string, err error) {
	if userID == "" {
		return nil, "", "", ErrMissingUserID
	}
	norm, err := NormalizeEmail(rawEmail)
	if err != nil {
		return nil, "", "", err
	}
	ct, err = box.EncryptBound(norm, userEmailContext(userID))
	if err != nil {
		return nil, "", "", err
	}
	return ct, DestinationHMAC(box, norm), MaskEmail(norm), nil
}

func OpenUserEmail(box *secure.Box, userID string, ct []byte) (string, error) {
	if userID == "" {
		return "", ErrMissingUserID
	}
	return box.DecryptBound(ct, userEmailContext(userID))
}

func SealUserPhone(box *secure.Box, userID, rawPhone string) (ct []byte, hmacHex, masked string, err error) {
	if userID == "" {
		return nil, "", "", ErrMissingUserID
	}
	norm, err := NormalizePhone(rawPhone)
	if err != nil {
		return nil, "", "", err
	}
	ct, err = box.EncryptBound(norm, userPhoneContext(userID))
	if err != nil {
		return nil, "", "", err
	}
	return ct, DestinationHMAC(box, norm), MaskPhone(norm), nil
}

func OpenUserPhone(box *secure.Box, userID string, ct []byte) (string, error) {
	if userID == "" {
		return "", ErrMissingUserID
	}
	return box.DecryptBound(ct, userPhoneContext(userID))
}
