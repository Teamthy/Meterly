// Package events defines outbox events. An event is written in the same
// database transaction as the state change that caused it, and the worker
// publishes it afterwards (spec §9.6). Consumers must be idempotent and
// dedupe on Event.ID.
package events

import (
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID            uuid.UUID      `json:"id"`
	Type          string         `json:"type"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   uuid.UUID      `json:"aggregate_id"`
	Payload       map[string]any `json:"payload"`
	CreatedAt     time.Time      `json:"created_at"`
	PublishedAt   *time.Time     `json:"published_at,omitempty"`
}

// New builds an event for an electricity transaction.
func New(typ string, txnID uuid.UUID, payload map[string]any) Event {
	return NewForAggregate("electricity_transaction", typ, txnID, payload)
}

// NewForAggregate builds an outbox event for any domain aggregate.
func NewForAggregate(aggregateType, typ string, aggregateID uuid.UUID, payload map[string]any) Event {
	if payload == nil {
		payload = map[string]any{}
	}
	return Event{
		ID:            uuid.New(),
		Type:          typ,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}
}
