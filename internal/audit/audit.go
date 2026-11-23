// Package audit defines audit events: who did what to which resource, and
// why. Events for state-changing actions are written in the same database
// transaction as the change. Metadata must already be redacted: never put
// tokens, meter numbers or contact details in it.
package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type ActorType string

const (
	ActorUser     ActorType = "USER"
	ActorOperator ActorType = "OPERATOR"
	ActorSystem   ActorType = "SYSTEM"
	ActorProvider ActorType = "PROVIDER"
)

type Event struct {
	ID            int64          `json:"id"`
	ActorUserID   string         `json:"actor_user_id,omitempty"` // internal user UUID
	ActorType     ActorType      `json:"actor_type"`
	Action        string         `json:"action"`
	ResourceType  string         `json:"resource_type"`
	ResourceID    *uuid.UUID     `json:"resource_id,omitempty"`
	CorrelationID *uuid.UUID     `json:"correlation_id,omitempty"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
}

// Actor identifies who is performing an action, for audit and history.
type Actor struct {
	UserID        string
	Type          ActorType
	Reason        string
	CorrelationID *uuid.UUID
}

// Event builds an audit event for this actor.
func (a Actor) Event(action, resourceType string, resourceID uuid.UUID, meta map[string]any) Event {
	if meta == nil {
		meta = map[string]any{}
	}
	if a.Reason != "" {
		meta["reason"] = a.Reason
	}
	id := resourceID
	return Event{ActorUserID: a.UserID, ActorType: a.Type, Action: action, ResourceType: resourceType,
		ResourceID: &id, CorrelationID: a.CorrelationID, Metadata: meta, CreatedAt: time.Now().UTC()}
}

type ctxKey struct{}

// WithCorrelation stores the request correlation ID for audit events.
func WithCorrelation(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// CorrelationFrom returns the correlation ID stored by WithCorrelation.
func CorrelationFrom(ctx context.Context) *uuid.UUID {
	if id, ok := ctx.Value(ctxKey{}).(uuid.UUID); ok {
		return &id
	}
	return nil
}
