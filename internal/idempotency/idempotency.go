// Package idempotency defines Idempotency-Key records. A retried request
// returns the original result; a reused key with a different body is
// rejected. Reservation, the work, and completion happen in one database
// transaction (see store.IdempotencyRepo), so a failed attempt leaves no
// reservation behind.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var (
	ErrConflict   = errors.New("idempotency: key reused with a different request")
	ErrInProgress = errors.New("idempotency: request with this key is still in progress")
)

type Record struct {
	Scope       string // customer or API client
	Operation   string
	Key         string
	RequestHash string
	ResultRef   string // empty while in progress
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

func HashRequest(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}
