// Package notificationintents defines non-deliverable notification candidates.
// They contain no destinations, token values, message content, or consent.
package notificationintents

import (
	"github.com/google/uuid"
	"time"
)

type Intent struct {
	EventID       uuid.UUID
	TransactionID uuid.UUID
	EventType     string
	CreatedAt     time.Time
}

// EventTypes is the fixed allowlist of outbox events for which a later,
// consent-aware workflow may consider creating a notification. These records
// are not permission to send and must never be processed as a delivery queue.
func EventTypes() []string {
	return []string{"payment.confirmed", "token.available", "electricity.delivery.confirmed", "electricity.delivery.failed", "electricity.delivery.unknown", "electricity.refund.completed"}
}
