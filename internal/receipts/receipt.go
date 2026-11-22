// Package receipts builds a minimal, token-free transaction record. A receipt
// reports observed states; it is not proof that remote credit reached a meter.
package receipts

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Receipt struct {
	TransactionID     uuid.UUID         `json:"transaction_id"`
	MaskedMeterNumber string            `json:"masked_meter_number"`
	Disco             string            `json:"disco"`
	AmountMinor       int64             `json:"amount_minor"`
	FeeMinor          int64             `json:"fee_minor"`
	TotalMinor        int64             `json:"total_minor"`
	Currency          string            `json:"currency"`
	PaymentStatus     tx.PaymentState   `json:"payment_status"`
	VendingStatus     tx.VendingState   `json:"vending_status"`
	DeliveryStatus    tx.DeliveryState  `json:"delivery_status"`
	DeliveryMethod    tx.DeliveryMethod `json:"delivery_method"`
	OverallStatus     tx.Overall        `json:"overall_status"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// Build does not authorise access; callers must verify purchaser ownership.
// It deliberately excludes the token, recipient contact, provider references,
// checkout URL, account details and internal review notes.
func Build(t *tx.Transaction, m *meters.Meter) (Receipt, error) {
	if t == nil || m == nil || t.ID == uuid.Nil || t.MeterID != m.ID || m.Masked == "" {
		return Receipt{}, errors.New("receipt data unavailable")
	}
	return Receipt{TransactionID: t.ID, MaskedMeterNumber: m.Masked, Disco: m.DiscoCode, AmountMinor: t.Amount.Minor, FeeMinor: t.Fee.Minor, TotalMinor: t.Total.Minor, Currency: string(t.Total.Currency), PaymentStatus: t.Payment, VendingStatus: t.Vending, DeliveryStatus: t.Delivery, DeliveryMethod: t.DeliveryMethod, OverallStatus: t.Status(), CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC()}, nil
}
