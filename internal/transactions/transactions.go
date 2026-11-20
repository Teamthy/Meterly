// Package transactions models an electricity purchase with three
// independent state dimensions — payment, vending and delivery — whose
// transitions are validated (spec §9.1). The overall status is derived,
// never stored independently.
package transactions

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
)

type PaymentState string
type VendingState string
type DeliveryState string
type DeliveryMethod string
type Overall string

const (
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
	PayCreated:       {PayPending, PayFailed},
	PayPending:       {PayConfirmed, PayFailed},
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

var ErrInvalidTransition = errors.New("transactions: invalid state transition")

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
	Payment        PaymentState   `json:"payment_status"`
	Vending        VendingState   `json:"vending_status"`
	Delivery       DeliveryState  `json:"delivery_status"`
	DeliveryMethod DeliveryMethod `json:"delivery_method"`

	PaymentProviderRef  string `json:"-"`
	CheckoutURL         string `json:"checkout_url,omitempty"`
	VendProviderRef     string `json:"-"`
	DeliveryProviderRef string `json:"-"`
	TokenCiphertext     []byte `json:"-"`
	TokenKeyVersion     string `json:"-"`
	UnitsKWh            string `json:"units_kwh,omitempty"`
	FailureCode         string `json:"failure_code,omitempty"`
	RefundProviderRef   string `json:"-"`

	History   []Event   `json:"history"`
	Version   int       `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Status derives the overall status from the three dimensions.
func (t *Transaction) Status() Overall {
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

func (t *Transaction) record(dim, from, to, note string, now time.Time) {
	t.History = append(t.History, Event{At: now, Dimension: dim, From: from, To: to, Note: note})
	t.UpdatedAt = now
}

func (t *Transaction) SetPayment(to PaymentState, note string) error {
	if !allowed(paymentTransitions, t.Payment, to) {
		return fmt.Errorf("%w: payment %s -> %s", ErrInvalidTransition, t.Payment, to)
	}
	t.record("payment", string(t.Payment), string(to), note, time.Now().UTC())
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
	t.record("vending", string(t.Vending), string(to), note, time.Now().UTC())
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
	t.record("delivery", string(t.Delivery), string(to), note, time.Now().UTC())
	t.Delivery = to
	return nil
}

var (
	ErrNotFound = errors.New("transactions: not found")
	ErrConflict = errors.New("transactions: concurrent update")
)

// Store is an in-memory repository with optimistic concurrency. The
// PostgreSQL implementation must use `WHERE version = $n` updates.
type Store struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]*Transaction
	byPay map[string]uuid.UUID
}

func NewStore() *Store {
	return &Store{byID: map[uuid.UUID]*Transaction{}, byPay: map[string]uuid.UUID{}}
}

func clone(t *Transaction) *Transaction {
	c := *t
	c.History = append([]Event(nil), t.History...)
	c.TokenCiphertext = append([]byte(nil), t.TokenCiphertext...)
	return &c
}

func (s *Store) Create(t *Transaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt, t.Version = now, now, 1
	s.byID[t.ID] = clone(t)
}

func (s *Store) Get(id uuid.UUID) (*Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

func (s *Store) ByPaymentRef(ref string) (*Transaction, error) {
	s.mu.Lock()
	id, ok := s.byPay[ref]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return s.Get(id)
}

// Update applies fn to a copy and saves it only if nobody else saved first.
func (s *Store) Update(id uuid.UUID, fn func(*Transaction) error) (*Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	next := clone(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	if next.Version != cur.Version {
		return nil, ErrConflict
	}
	next.Version++
	s.byID[id] = next
	if next.PaymentProviderRef != "" {
		s.byPay[next.PaymentProviderRef] = id
	}
	return clone(next), nil
}

func (s *Store) ListForCustomer(customerID string) []*Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Transaction
	for _, t := range s.byID {
		if t.CustomerID == customerID {
			out = append(out, clone(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// NeedsReconciliation returns transactions in an ambiguous state.
func (s *Store) NeedsReconciliation() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for id, t := range s.byID {
		if t.Vending == VendUnknown || t.Delivery == DelUnknown || t.Delivery == DelAccepted {
			out = append(out, id)
		}
	}
	return out
}
