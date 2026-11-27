// Package payrequests models Pay-for-Me payment request links (M7 / F1).
//
// Security invariants:
//   - Link codes and guest status codes carry 128 bits of cryptographic
//     randomness encoded in unpadded RFC 4648 base32; only their domain-
//     separated HMAC-SHA256 digests are indexed in storage.
//   - Request notes, guest payer emails, guest payer names and replay status
//     codes are AES-256-GCM encrypted bound to their owning resource IDs.
//   - Request notes and payer names reject embedded URLs or bare domains to
//     prevent phishing on public checkout pages.
package payrequests

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/secure"
)

type AmountMode string

const (
	ModeFixed AmountMode = "FIXED"
	ModeRange AmountMode = "RANGE"
)

type Status string

const (
	StatusOpen      Status = "OPEN"
	StatusPaid      Status = "PAID"
	StatusExpired   Status = "EXPIRED"
	StatusCancelled Status = "CANCELLED"
)

const (
	DefaultTTL        = 72 * time.Hour
	MaxTTL            = 7 * 24 * time.Hour
	MaxNoteRunes      = 140
	MaxPayerNameRunes = 80
	MaxPaymentsCap    = 100
	CodeLength        = 26 // 16 bytes in unpadded RFC 4648 base32
)

var (
	ErrInvalidNote      = errors.New("payrequests: note must be at most 140 characters and must not contain URLs or links")
	ErrInvalidPayerName = errors.New("payrequests: payer_name must be at most 80 characters and must not contain URLs or links")
	ErrInvalidTTL       = errors.New("payrequests: expiry must be positive and at most 7 days")
	ErrInvalidMaxPays   = errors.New("payrequests: max_payments must be between 1 and 100")
	ErrInvalidMode      = errors.New("payrequests: amount_mode must be FIXED or RANGE")
)

var (
	codeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	// Detects schemes (http://, https://, etc.), www., mailto:/tel:, or bare domains (example.com, meterly.ng, bit.ly/x).
	urlLikeRe = regexp.MustCompile(`(?i)(://|\bwww\.|\bmailto:|\btel:|\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.[a-z]{2,24}\b)`)
)

// PaymentRequest is a requester's shareable link asking someone else to pay
// for electricity on a saved meter.
type PaymentRequest struct {
	ID             uuid.UUID      `json:"id"`
	RequesterID    string         `json:"-"`
	MeterID        uuid.UUID      `json:"meter_id"`
	AmountMode     AmountMode     `json:"amount_mode"`
	AmountMinor    int64          `json:"amount_minor,omitempty"`
	MinAmountMinor int64          `json:"min_amount_minor,omitempty"`
	MaxAmountMinor int64          `json:"max_amount_minor,omitempty"`
	Currency       money.Currency `json:"currency"`
	NoteCiphertext []byte         `json:"-"`
	NoteKeyVersion string         `json:"-"`
	Status         Status         `json:"status"`
	MaxPayments    int            `json:"max_payments"`
	PaymentsCount  int            `json:"payments_count"`
	CodeHMAC       string         `json:"-"`
	ExpiresAt      time.Time      `json:"expires_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// EffectiveStatus returns EXPIRED when an OPEN request has passed ExpiresAt.
func (p *PaymentRequest) EffectiveStatus(now time.Time) Status {
	if p.Status == StatusOpen && now.After(p.ExpiresAt) {
		return StatusExpired
	}
	return p.Status
}

// Payer records guest payer metadata for a transaction initiated through a
// Pay-for-Me link. No plaintext email, name or status code is stored.
type Payer struct {
	TransactionID        uuid.UUID
	PaymentRequestID     uuid.UUID
	EmailCiphertext      []byte
	EmailKeyVersion      string
	PayerNameCiphertext  []byte
	PayerNameKeyVersion  string
	StatusCodeHMAC       string
	StatusCodeCiphertext []byte
	StatusCodeKeyVersion string
	SlotReleased         bool
	CreatedAt            time.Time
}

// GenerateCode returns a 128-bit cryptographically random unpadded base32 code.
func GenerateCode() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	return codeEncoding.EncodeToString(raw[:]), nil
}

// NormalizeCode canonicalizes a link or status code and validates its format.
func NormalizeCode(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if len(s) != CodeLength {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')) {
			return "", false
		}
	}
	return s, true
}

// LinkCodeHMAC computes the domain-separated HMAC of a shareable link code.
func LinkCodeHMAC(box *secure.Box, rawCode string) (string, bool) {
	norm, ok := NormalizeCode(rawCode)
	if !ok || box == nil {
		return "", false
	}
	return box.Hash("payrequest-code:" + norm), true
}

// StatusCodeHMAC computes the domain-separated HMAC of a guest payer status code.
func StatusCodeHMAC(box *secure.Box, rawCode string) (string, bool) {
	norm, ok := NormalizeCode(rawCode)
	if !ok || box == nil {
		return "", false
	}
	return box.Hash("payer-status-code:" + norm), true
}

// ConstantTimeEqualHMAC compares two HMAC hex digests in constant time.
func ConstantTimeEqualHMAC(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ValidateNote sanitizes and validates an optional requester note (<= 140 runes, no URLs).
func ValidateNote(raw string) (string, error) {
	cleaned := cleanSingleLine(raw)
	if cleaned == "" {
		return "", nil
	}
	if utf8.RuneCountInString(cleaned) > MaxNoteRunes {
		return "", ErrInvalidNote
	}
	if ContainsURL(cleaned) {
		return "", ErrInvalidNote
	}
	return cleaned, nil
}

// ValidatePayerName sanitizes and validates an optional guest payer display name.
func ValidatePayerName(raw string) (string, error) {
	cleaned := cleanSingleLine(raw)
	if cleaned == "" {
		return "", nil
	}
	if utf8.RuneCountInString(cleaned) > MaxPayerNameRunes {
		return "", ErrInvalidPayerName
	}
	if ContainsURL(cleaned) {
		return "", ErrInvalidPayerName
	}
	return cleaned, nil
}

// ContainsURL reports whether s contains a URL scheme or domain-like token.
func ContainsURL(s string) bool {
	return urlLikeRe.MatchString(s)
}

func cleanSingleLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) {
			if r == '\n' || r == '\r' || r == '\t' {
				if !lastSpace {
					b.WriteByte(' ')
					lastSpace = true
				}
			}
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func SealNote(box *secure.Box, requestID uuid.UUID, note string) ([]byte, string, error) {
	if note == "" {
		return nil, "", nil
	}
	ct, err := box.EncryptBound(note, []byte("payrequest-note:"+requestID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenNote(box *secure.Box, requestID uuid.UUID, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	return box.DecryptBound(ct, []byte("payrequest-note:"+requestID.String()))
}

func SealPayerEmail(box *secure.Box, txnID uuid.UUID, email string) ([]byte, string, error) {
	ct, err := box.EncryptBound(email, []byte("payer-email:"+txnID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenPayerEmail(box *secure.Box, txnID uuid.UUID, ct []byte) (string, error) {
	return box.DecryptBound(ct, []byte("payer-email:"+txnID.String()))
}

func SealPayerName(box *secure.Box, txnID uuid.UUID, name string) ([]byte, string, error) {
	if name == "" {
		return nil, "", nil
	}
	ct, err := box.EncryptBound(name, []byte("payer-name:"+txnID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenPayerName(box *secure.Box, txnID uuid.UUID, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	return box.DecryptBound(ct, []byte("payer-name:"+txnID.String()))
}

func SealStatusCode(box *secure.Box, txnID uuid.UUID, statusCode string) ([]byte, string, error) {
	ct, err := box.EncryptBound(statusCode, []byte("payer-status-code:"+txnID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenStatusCode(box *secure.Box, txnID uuid.UUID, ct []byte) (string, error) {
	return box.DecryptBound(ct, []byte("payer-status-code:"+txnID.String()))
}
