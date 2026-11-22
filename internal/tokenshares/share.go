// Package tokenshares models metadata only; it never stores raw token data.
package tokenshares

import (
	"github.com/google/uuid"
	"time"
)

type Share struct {
	ID            uuid.UUID `json:"share_id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	UserID        string    `json:"-"`
	Channel       string    `json:"channel"`
	SharedAt      time.Time `json:"recorded_at"`
}

// Copy records a purchaser's report of a local copy action. It does not mean
// the token was actually copied, sent, received or entered into a meter.
const Copy = "COPY"
