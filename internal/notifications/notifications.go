// Package notifications defines privacy-conscious message templates, delivery
// state transitions and transport contracts for SMS, email and WhatsApp.
package notifications

import (
	"context"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Channel string

const (
	SMS      Channel = "SMS"
	Email    Channel = "EMAIL"
	WhatsApp Channel = "WHATSAPP"
)

type Message struct {
	EventID       uuid.UUID
	TransactionID uuid.UUID
	Channel       Channel
	Destination   string // sensitive: never log or persist in plaintext
	Subject       string
	Body          string // sensitive: never log
}

// Sender implementations must document their idempotency and ambiguous-send
// semantics. A timeout after sending cannot automatically be treated as failure.
type Sender interface {
	Send(context.Context, Message) error
}
type SMSSender interface{ Sender }
type EmailSender interface{ Sender }
type WhatsAppSender interface{ Sender }

type Content struct{ Subject, Body string }

// TemplateFor creates a minimal notification from a trusted outbox event and
// the actual delivery method for that event. It ignores arbitrary event payloads
// and never copies account data, a meter number, or token into the message.
// A token-available message tells the purchaser to retrieve it securely; gift
// token delivery requires separate recipient consent and secure delivery work.
// ok=false means this event must not send a notification.
func TemplateFor(e events.Event, method tx.DeliveryMethod) (content Content, ok bool, err error) {
	spec, ok, err := TemplateSpecFor(e, method)
	if err != nil || !ok {
		return Content{}, ok, err
	}
	c, _, err := RenderPurchaserTemplate(spec.ID, LocaleEN)
	if err != nil {
		return Content{}, false, err
	}
	return c, true, nil
}
