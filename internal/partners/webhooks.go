package partners

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// PurchaseEvents is the closed set of outbound partner webhook types.
var PurchaseEvents = []string{
	EventPaymentConfirmed,
	EventVended,
	EventDelivered,
	EventFailed,
	EventRefunded,
	EventUnknown,
}

var outboxToPartner = map[string]string{
	"payment.confirmed":              EventPaymentConfirmed,
	"electricity.vending.completed":  EventVended,
	"electricity.delivery.confirmed": EventDelivered,
	"electricity.vending.failed":     EventFailed,
	"payment.failed":                 EventFailed,
	"electricity.refund.completed":   EventRefunded,
	"electricity.vending.unknown":    EventUnknown,
	"electricity.delivery.unknown":   EventUnknown,
}

type WebhookEndpoint struct {
	ID                   uuid.UUID
	PartnerID            uuid.UUID
	URL                  string
	Description          string
	Events               []string
	SecretCiphertext     []byte
	SecretKeyVersion     string
	PrevSecretCiphertext []byte
	PrevSecretKeyVersion string
	OverlapUntil         *time.Time
	IncludeToken         bool
	Enabled              bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type WebhookDelivery struct {
	ID             uuid.UUID
	EndpointID     uuid.UUID
	PartnerID      uuid.UUID
	EventID        uuid.UUID
	EventType      string
	TransactionID  uuid.UUID
	Payload        json.RawMessage
	Status         string
	AttemptCount   int
	NextAttemptAt  time.Time
	FirstAttemptAt *time.Time
	LastAttemptAt  *time.Time
	ResponseCode   int
	ErrorClass     string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type PurchaseSnapshot struct {
	ID             uuid.UUID
	PartnerID      uuid.UUID
	PartnerRef     string
	AmountMinor    int64
	FeeMinor       int64
	TotalMinor     int64
	Currency       string
	Payment        string
	Vending        string
	Delivery       string
	Overall        string
	DeliveryMethod string
	UnitsKWh       string
	FailureCode    string
	CreatedAt      time.Time
}

// Runner runs partner-repo work in a store transaction. Implemented outside
// this package so partners does not import store (store already imports partners).
type Runner interface {
	InTx(ctx context.Context, fn func(Repo) error) error
}

func KnownWebhookEvent(s string) bool {
	if s == EventPing {
		return true
	}
	for _, e := range PurchaseEvents {
		if e == s {
			return true
		}
	}
	return false
}

func NormalizeWebhookEvents(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, ErrInvalidInput
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !KnownWebhookEvent(s) || s == EventPing {
			return nil, ErrInvalidInput
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, ErrInvalidInput
	}
	return out, nil
}

func (ep WebhookEndpoint) Subscribes(eventType string) bool {
	if eventType == EventPing {
		return true
	}
	for _, e := range ep.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

func MapOutbox(eventType string) string {
	return outboxToPartner[eventType]
}

func SnapshotOf(t *tx.Transaction) PurchaseSnapshot {
	if t == nil {
		return PurchaseSnapshot{}
	}
	cur := string(t.Total.Currency)
	if cur == "" {
		cur = "NGN"
	}
	return PurchaseSnapshot{
		ID:             t.ID,
		PartnerID:      t.PartnerID,
		PartnerRef:     t.PartnerRef,
		AmountMinor:    t.Amount.Minor,
		FeeMinor:       t.Fee.Minor,
		TotalMinor:     t.Total.Minor,
		Currency:       cur,
		Payment:        string(t.Payment),
		Vending:        string(t.Vending),
		Delivery:       string(t.Delivery),
		Overall:        string(t.Status()),
		DeliveryMethod: string(t.DeliveryMethod),
		UnitsKWh:       t.UnitsKWh,
		FailureCode:    t.FailureCode,
		CreatedAt:      t.CreatedAt.UTC(),
	}
}

func BuildPayload(ev events.Event, partnerType string, snap PurchaseSnapshot) ([]byte, error) {
	created := ev.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	data := map[string]any{
		"purchase_id":     snap.ID.String(),
		"partner_id":      snap.PartnerID.String(),
		"partner_ref":     snap.PartnerRef,
		"amount_minor":    snap.AmountMinor,
		"fee_minor":       snap.FeeMinor,
		"total_minor":     snap.TotalMinor,
		"currency":        snap.Currency,
		"payment_status":  snap.Payment,
		"vending_status":  snap.Vending,
		"delivery_status": snap.Delivery,
		"overall_status":  snap.Overall,
		"delivery_method": snap.DeliveryMethod,
	}
	if snap.UnitsKWh != "" {
		data["units_kwh"] = snap.UnitsKWh
	}
	if snap.FailureCode != "" {
		data["failure_code"] = snap.FailureCode
	}
	body := map[string]any{
		"id":         ev.ID.String(),
		"type":       partnerType,
		"created_at": created.Format(time.RFC3339),
		"data":       data,
	}
	return json.Marshal(body)
}

func BuildPingPayload(eventID uuid.UUID, endpointID uuid.UUID, now time.Time) ([]byte, error) {
	body := map[string]any{
		"id":         eventID.String(),
		"type":       EventPing,
		"created_at": now.UTC().Format(time.RFC3339),
		"data": map[string]any{
			"endpoint_id": endpointID.String(),
		},
	}
	return json.Marshal(body)
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 5 * time.Second
	for i := 1; i < attempt; i++ {
		d *= 4
		if d >= time.Hour {
			return time.Hour
		}
	}
	return d
}

func DeadLetterAfterFailure(firstAttempt time.Time, now time.Time, attempt int) (dead bool, next time.Time) {
	if firstAttempt.IsZero() {
		firstAttempt = now
	}
	deadline := firstAttempt.Add(WebhookRetryHorizon)
	n := now.Add(RetryDelay(attempt))
	if !now.Before(deadline) || !n.Before(deadline) {
		return true, time.Time{}
	}
	return false, n
}
