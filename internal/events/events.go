// Package events is a minimal outbox. In PostgreSQL the outbox row must be
// written in the same database transaction as the state change, and a
// worker publishes it afterwards (spec §9.6). Consumers must be idempotent.
package events

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID          uuid.UUID      `json:"id"`
	Type        string         `json:"type"`
	AggregateID uuid.UUID      `json:"aggregate_id"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   time.Time      `json:"created_at"`
	PublishedAt *time.Time     `json:"published_at,omitempty"`
}

type Outbox struct {
	mu     sync.Mutex
	events []*Event
}

func NewOutbox() *Outbox { return &Outbox{} }

func (o *Outbox) Add(typ string, agg uuid.UUID, payload map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, &Event{ID: uuid.New(), Type: typ, AggregateID: agg, Payload: payload, CreatedAt: time.Now().UTC()})
}

// Drain publishes unpublished events through fn, marking each published
// only when fn succeeds.
func (o *Outbox) Drain(fn func(Event) error) {
	o.mu.Lock()
	var pending []*Event
	for _, e := range o.events {
		if e.PublishedAt == nil {
			pending = append(pending, e)
		}
	}
	o.mu.Unlock()
	for _, e := range pending {
		if fn(*e) == nil {
			now := time.Now().UTC()
			o.mu.Lock()
			e.PublishedAt = &now
			o.mu.Unlock()
		}
	}
}

func (o *Outbox) For(agg uuid.UUID) []Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Event
	for _, e := range o.events {
		if e.AggregateID == agg {
			out = append(out, *e)
		}
	}
	return out
}
