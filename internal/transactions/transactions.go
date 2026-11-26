// Package transactions models an electricity purchase with three
// independent state dimensions — payment, vending and delivery — whose
// transitions are validated (spec §9.1). The overall status is derived,
// never stored independently. Persistence lives in internal/store.
package transactions

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
)

type PaymentState string
type VendingState string
type DeliveryState string
type DeliveryMethod string
type FundingSource string
type Overall string

const (
	FundingPSP FundingSource = "PSP"
	FundingPot FundingSource = "POT"

	PayCreated       PaymentState = "CREATED"
	PayPending       PaymentState = "PENDING"
	PayConfirmed     PaymentState = "CONFIRMED"
	PayFailed        PaymentState = "FAILED"
	PayRefundPending PaymentState = "REFUND_PENDING"
	PayRefunded      PaymentState = "REFUNDED"

	VendNotStarted VendingState = "NOT_STARTED"
	VendPending    VendingState = "PENDING"
	VendConfirmed  VendingState = "CONFIRMED"
	VendFailed     VendingState = "FAILED"
	VendUnknown    VendingState = "UNKNOWN"

	DelNotStarted DeliveryState = "NOT_STARTED"
	DelPending    DeliveryState = "PENDING"
	DelAccepted   DeliveryState = "ACCEPTED"
	DelConfirmed  DeliveryState = "CONFIRMED"
	DelFailed     DeliveryState = "FAILED"
	DelUnknown    DeliveryState = "UNKNOWN"

	MethodToken  DeliveryMethod = "TOKEN"
	MethodRemote DeliveryMethod = "REMOTE"

	OverallPaymentPending  Overall = "PAYMENT_PENDING"
	OverallFailed          Overall = "FAILED"
	OverallVendingPending  Overall = "VENDING_PENDING"
	OverallDeliveryPending Overall = "DELIVERY_PENDING"
	OverallReconciliation  Overall = "RECONCILIATION_PENDING"
	OverallManualReview    Overall = "MANUAL_REVIEW"
	OverallRefundPending   Overall = "REFUND_PENDING"
	OverallRefunded        Overall = "REFUNDED"
	OverallCompleted       Overall = "COMPLETED"
)

var paymentTransitions = map[PaymentState][]PaymentState{
	PayCreated: {PayPending, PayFailed},
	// PENDING/FAILED -> REFUND_PENDING exist only for operator-approved
	// refunds of money that arrived but was never accepted (amount
	// mismatch, payment after expiry). Automation never takes them.
	PayPending:       {PayConfirmed, PayFailed, PayRefundPending},
	PayFailed:        {PayRefundPending},
	PayConfirmed:     {PayRefundPending},
	PayRefundPending: {PayRefunded},
}

var vendingTransitions = map[VendingState][]VendingState{
	VendNotStarted: {VendPending},
	VendPending:    {VendConfirmed, VendFailed, VendUnknown},
	VendUnknown:    {VendConfirmed, VendFailed, VendPending}, // PENDING only when provider confirms no record
}

var deliveryTransitions = map[DeliveryState][]DeliveryState{
	DelNotStarted: {DelPending},
	DelPending:    {DelAccepted, DelConfirmed, DelFailed, DelUnknown},
	DelAccepted:   {DelConfirmed, DelFailed, DelUnknown},
	DelUnknown:    {DelAccepted, DelConfirmed, DelFailed, DelPending},
	DelFailed:     {DelPending}, // operator-approved fallback to token only
}

var (
	ErrInvalidTransition = errors.New("transactions: invalid state transition")
	ErrNotFound          = errors.New("transactions: not found")
	ErrConflict          = errors.New("transactions: concurrent update")
)

func allowed[S comparable](table map[S][]S, from, to S) bool {
	for _, s := range table[from] {
		if s == to {
			return true
		}
	}
	return false
}

type Event struct {
	At        time.Time `json:"at"`
	Dimension string    `json:"dimension"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Note      string    `json:"note,omitempty"`
}

type Transaction struct {
	ID             uuid.UUID      `json:"transaction_id"`
	CustomerID     string         `json:"customer_id"`
	MeterID        uuid.UUID      `json:"meter_id"`
	QuoteID        uuid.UUID      `json:"quote_id"`
	IsGift         bool           `json:"is_gift"`
	RecipientName  string         `json:"recipient_name,omitempty"`
	Amount         money.Amount   `json:"amount"`
	Fee            money.Amount   `json:"fee"`
	Total          money.Amount   `json:"total"`
	FundingSource  FundingSource  `json:"funding_source,omitempty"`
	Payment        PaymentState   `json:"payment_status"`
	Vending        VendingState   `json:"vending_status"`
	Delivery       DeliveryState  `json:"delivery_status"`
	DeliveryMethod DeliveryMethod `json:"delivery_method"`

	PaymentProviderName  string       `json:"-"`
	PaymentProviderRef   string       `json:"-"`
	CheckoutURL          string       `json:"checkout_url,omitempty"`
	VendProviderName     string       `json:"-"`
	VendProviderRef      string       `json:"-"`
	// VendIdempotencyKey is the provider-facing vend key recorded at claim
	// time (VTpass request_id, mock "vend:{uuid}"). Stable across retries.
	VendIdempotencyKey string `json:"-"`
	DeliveryProviderName string       `json:"-"`
	DeliveryProviderRef  string       `json:"-"`
	TokenCiphertext      []byte       `json:"-"`
	TokenKeyVersion      string       `json:"-"`
	UnitsKWh             string       `json:"units_kwh,omitempty"`
	FailureCode          string       `json:"failure_code,omitempty"`
	RefundReason         string       `json:"-"`
	RefundAmount         money.Amount `json:"-"` // what is being refunded; may differ from Total
	RefundProviderRef    string       `json:"-"`

	// ReviewReason, when set, puts the transaction in MANUAL_REVIEW and stops
	// all automated processing until an operator resolves it.
	ReviewReason string `json:"review_reason,omitempty"`
	// Scheduling for the resume/reconciliation worker.
	CheckAttempts int       `json:"-"`
	NextCheckAt   time.Time `json:"-"`

	History []Event `json:"history"`
	// PersistedEvents counts History entries already stored; repositories
	// append only History[PersistedEvents:].
	PersistedEvents int       `json:"-"`
	Version         int       `json:"-"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// New returns a PSP-funded transaction in its initial state.
func New(customerID string, q *Quote, isGift bool, recipient string, now time.Time) *Transaction {
	return &Transaction{
		ID: uuid.New(), CustomerID: customerID, MeterID: q.MeterID, QuoteID: q.ID,
		IsGift: isGift, RecipientName: recipient,
		Amount: q.Amount, Fee: q.Fee, Total: q.Total,
		FundingSource: FundingPSP,
		Payment:       PayCreated, Vending: VendNotStarted, Delivery: DelNotStarted, DeliveryMethod: q.DeliveryMethod,
		NextCheckAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

// NewPotFunded returns a POT-funded transaction whose payment is already
// CONFIRMED inside the surrounding pot-allocation DB transaction. It carries
// no PSP reference; any vend-failure refund returns money to POT_FUNDS_HELD.
func NewPotFunded(customerID string, q *Quote, now time.Time) *Transaction {
	return &Transaction{
		ID: uuid.New(), CustomerID: customerID, MeterID: q.MeterID, QuoteID: q.ID,
		Amount: q.Amount, Fee: q.Fee, Total: q.Total,
		FundingSource: FundingPot,
		Payment:       PayConfirmed, Vending: VendNotStarted, Delivery: DelNotStarted, DeliveryMethod: q.DeliveryMethod,
		History: []Event{{
			At: now.UTC(), Dimension: "payment", From: "", To: string(PayConfirmed), Note: "funded from pot allocation",
		}},
		NextCheckAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

// EffectiveFundingSource returns FundingPSP when FundingSource was left unset.
func (t *Transaction) EffectiveFundingSource() FundingSource {
	if t.FundingSource == "" {
		return FundingPSP
	}
	return t.FundingSource
}

// Status derives the overall status from the three dimensions.
func (t *Transaction) Status() Overall {
	if t.ReviewReason != "" {
		return OverallManualReview
	}
	switch t.Payment {
	case PayCreated, PayPending:
		return OverallPaymentPending
	case PayFailed:
		return OverallFailed
	case PayRefundPending:
		return OverallRefundPending
	case PayRefunded:
		return OverallRefunded
	}
	switch t.Vending {
	case VendNotStarted, VendPending:
		return OverallVendingPending
	case VendUnknown:
		return OverallReconciliation
	case VendFailed:
		return OverallManualReview // payment confirmed but not yet moved to refund
	}
	switch t.Delivery {
	case DelConfirmed:
		return OverallCompleted
	case DelFailed:
		return OverallManualReview
	case DelUnknown:
		return OverallReconciliation
	}
	return OverallDeliveryPending
}

// Settled reports whether automation has nothing left to do: the
// transaction finished, failed before money moved, was refunded, or awaits
// an operator.
func (t *Transaction) Settled() bool {
	if t.ReviewReason != "" {
		return true
	}
	switch t.Payment {
	case PayFailed, PayRefunded:
		return true
	}
	return t.Delivery == DelConfirmed || t.Delivery == DelFailed
}

func (t *Transaction) record(dim, from, to, note string) {
	now := time.Now().UTC()
	t.History = append(t.History, Event{At: now, Dimension: dim, From: from, To: to, Note: note})
	t.UpdatedAt = now
}

func (t *Transaction) SetPayment(to PaymentState, note string) error {
	if !allowed(paymentTransitions, t.Payment, to) {
		return fmt.Errorf("%w: payment %s -> %s", ErrInvalidTransition, t.Payment, to)
	}
	t.record("payment", string(t.Payment), string(to), note)
	t.Payment = to
	return nil
}

func (t *Transaction) SetVending(to VendingState, note string) error {
	if !allowed(vendingTransitions, t.Vending, to) {
		return fmt.Errorf("%w: vending %s -> %s", ErrInvalidTransition, t.Vending, to)
	}
	if to == VendPending && t.Payment != PayConfirmed {
		return fmt.Errorf("%w: cannot vend before payment is confirmed", ErrInvalidTransition)
	}
	t.record("vending", string(t.Vending), string(to), note)
	t.Vending = to
	return nil
}

func (t *Transaction) SetDelivery(to DeliveryState, note string) error {
	if !allowed(deliveryTransitions, t.Delivery, to) {
		return fmt.Errorf("%w: delivery %s -> %s", ErrInvalidTransition, t.Delivery, to)
	}
	if to == DelPending && t.Vending != VendConfirmed {
		return fmt.Errorf("%w: cannot deliver before vending is confirmed", ErrInvalidTransition)
	}
	t.record("delivery", string(t.Delivery), string(to), note)
	t.Delivery = to
	return nil
}

// FlagForReview moves the transaction to MANUAL_REVIEW. Idempotent.
func (t *Transaction) FlagForReview(reason string) {
	if t.ReviewReason != "" {
		return
	}
	t.ReviewReason = reason
	t.record("review", "", "MANUAL_REVIEW", reason)
}

// Quote is a priced, time-limited, single-use offer for one meter.
type Quote struct {
	ID              uuid.UUID        `json:"quote_id"`
	CustomerID      string           `json:"-"`
	MeterID         uuid.UUID        `json:"meter_id"`
	Amount          money.Amount     `json:"amount"`
	Fee             money.Amount     `json:"fee"`
	Total           money.Amount     `json:"total"`
	DeliveryMethod  DeliveryMethod   `json:"delivery_method"`
	DeliveryOptions []DeliveryMethod `json:"delivery_options"`
	ExpiresAt       time.Time        `json:"expires_at"`
	Used            bool             `json:"-"`
	CreatedAt       time.Time        `json:"-"`
}
