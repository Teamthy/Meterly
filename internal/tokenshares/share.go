// Package tokenshares models metadata only; it never stores raw token data.
package tokenshares

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Share struct {
	ID            uuid.UUID `json:"share_id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	UserID        string    `json:"-"`
	Channel       string    `json:"channel"`
	SharedAt      time.Time `json:"recorded_at"`
	DestHMAC      string    `json:"-"`
	DestCipher    []byte    `json:"-"`
	DestKeyVer    string    `json:"-"`
	DestMasked    string    `json:"destination_masked,omitempty"`
}

const (
	// Copy records a purchaser's report of a local copy action. It does not
	// mean the token was actually copied, sent, received or entered into a meter.
	Copy     = "COPY"
	SMS      = "SMS"
	Email    = "EMAIL"
	WhatsApp = "WHATSAPP"

	// MaxSendsPerDay caps token-bearing SMS/email/WhatsApp shares per purchaser
	// per Africa/Lagos calendar day (COPY is unlimited metadata).
	MaxSendsPerDay = 5
)

var ErrUnsupportedChannel = errors.New("unsupported token share channel")

func ParseChannel(s string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case Copy, SMS, Email, WhatsApp:
		return strings.ToUpper(strings.TrimSpace(s)), nil
	default:
		return "", ErrUnsupportedChannel
	}
}

func IsSendChannel(ch string) bool {
	switch ch {
	case SMS, Email, WhatsApp:
		return true
	default:
		return false
	}
}

// DestAAD is the AES-GCM binding for a share destination ciphertext.
func DestAAD(id uuid.UUID) []byte {
	return []byte("token-share-dest:" + id.String())
}

// LagosDayBounds returns the half-open UTC interval [start, end) covering the
// Africa/Lagos calendar day that contains now. WAT is UTC+1 with no DST.
func LagosDayBounds(now time.Time) (start, end time.Time) {
	wat := now.UTC().Add(time.Hour)
	y, m, d := wat.Date()
	start = time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(-time.Hour)
	end = start.Add(24 * time.Hour)
	return
}
