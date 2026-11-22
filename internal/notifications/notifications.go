// Package notifications defines privacy-conscious message templates and
// transport boundaries. It is not connected to the outbox or to live senders.
package notifications

import (
	"context"
	"errors"

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
	if e.ID == uuid.Nil || e.AggregateID == uuid.Nil || e.AggregateType != "electricity_transaction" {
		return Content{}, false, errors.New("invalid notification event")
	}
	switch e.Type {
	case "payment.confirmed":
		return Content{"Payment confirmed", "Your payment was confirmed. Electricity vending and delivery are tracked separately in Meterly."}, true, nil
	case "token.available":
		if method != tx.MethodToken {
			return Content{}, false, errors.New("token event requires token delivery")
		}
		return Content{"Electricity token available", "Your electricity token is available in Meterly. The token works only on the meter it was bought for; sharing it does not move credit between meters."}, true, nil
	case "electricity.delivery.confirmed":
		if method == tx.MethodToken {
			return Content{}, false, nil
		} // token availability has its own event
		if method != tx.MethodRemote {
			return Content{}, false, errors.New("unknown delivery method")
		}
		return Content{"Remote delivery confirmed", "Your meter acknowledged the remote electricity credit. View the transaction in Meterly."}, true, nil
	case "electricity.delivery.failed":
		if method != tx.MethodRemote {
			return Content{}, false, errors.New("remote delivery event requires remote method")
		}
		return Content{"Remote delivery failed", "Remote delivery failed. Check Meterly for the transaction status and support options."}, true, nil
	case "electricity.delivery.unknown":
		if method != tx.MethodRemote {
			return Content{}, false, errors.New("remote delivery event requires remote method")
		}
		return Content{"Remote delivery status unknown", "We could not confirm remote delivery at the time of this update. Check Meterly for the latest status."}, true, nil
	case "electricity.refund.completed":
		return Content{"Refund completed", "Your refund has been confirmed by the payment provider. Check Meterly for the transaction details."}, true, nil
	default:
		return Content{}, false, nil
	}
}
