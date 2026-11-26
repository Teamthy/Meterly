// Package providers defines the internal contracts that external payment,
// vending and remote-credit integrations are adapted to (spec §8).
// Business logic depends only on these interfaces, never on a vendor SDK.
package providers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/teamthy/meterly/internal/money"
)

// ErrAmbiguous means the provider may or may not have performed the
// operation (timeout, dropped connection, 5xx after send). Callers MUST
// treat the outcome as UNKNOWN and reconcile via a status query; they must
// never blindly resend (spec §9.3).
var ErrAmbiguous = errors.New("provider: outcome unknown")

// IsAmbiguous reports whether err leaves the remote outcome unknown.
func IsAmbiguous(err error) bool {
	return errors.Is(err, ErrAmbiguous) || errors.Is(err, context.DeadlineExceeded)
}

// ---- Payment ---------------------------------------------------------------

type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "PENDING"
	PaymentSucceeded PaymentStatus = "SUCCEEDED"
	PaymentFailed    PaymentStatus = "FAILED"
)

type PaymentInit struct {
	Reference string // our reference, also used as idempotency key
	Amount    money.Amount
	Email     string
}

type PaymentInitResult struct {
	ProviderRef string
	CheckoutURL string
}

type PaymentVerification struct {
	ProviderRef   string
	Status        PaymentStatus
	Amount        money.Amount
	PaidAt        *time.Time
	Authorization *CardAuthorization // present when provider reports a card authorization
}

// CardAuthorization captures reusable card metadata returned by the payment
// provider on VerifyPayment. AuthorizationCode is a sensitive credential and
// must never be logged or returned in API responses.
type CardAuthorization struct {
	AuthorizationCode string
	Reusable          bool
	CardBrand         string
	Last4             string
	ExpMonth          int
	ExpYear           int
	Channel           string
	Signature         string
	CustomerEmail     string
}

type ChargeRequest struct {
	AuthorizationRef string // provider authorization_code (sensitive)
	Email            string
	Amount           money.Amount
	Reference        string // deterministic reference for status reconciliation
}

type RefundRequest struct {
	ProviderRef string
	Amount      money.Amount
	Reference   string
}

// ErrPaymentNotFound means the payment provider definitively confirmed it has
// no transaction record for the queried reference.
var ErrPaymentNotFound = errors.New("provider: payment reference not found")

type PaymentProvider interface {
	Name() string
	InitializePayment(ctx context.Context, req PaymentInit) (PaymentInitResult, error)
	// ChargeSaved initiates an off-session charge against a saved reusable
	// card authorization. Callers MUST still treat the outcome as unverified
	// until VerifyPayment confirms it (invariant 1).
	ChargeSaved(ctx context.Context, req ChargeRequest) (PaymentVerification, error)
	// VerifyPayment is the server-side source of truth. Webhook bodies are
	// only a hint to call this.
	VerifyPayment(ctx context.Context, providerRef string) (PaymentVerification, error)
	// RefundPayment must be idempotent per RefundRequest.Reference: a retry
	// after a lost response returns the existing refund, never a second one.
	RefundPayment(ctx context.Context, req RefundRequest) (string, error)
	// ParseWebhook authenticates a webhook (ErrBadSignature if it fails) and
	// extracts the payment reference. The body is only a hint: callers must
	// still VerifyPayment before acting.
	ParseWebhook(h http.Header, body []byte) (WebhookEvent, error)
}

// WebhookEvent is the provider-neutral part of a payment webhook.
type WebhookEvent struct {
	Type      string
	Reference string // payment reference; empty when Relevant is false
	Relevant  bool   // false for event types we acknowledge but ignore
}

// ErrBadSignature means a webhook failed authentication.
var ErrBadSignature = errors.New("provider: webhook signature invalid")

// ---- Vending ---------------------------------------------------------------

type MeterValidationRequest struct {
	MeterNumber string
	DiscoCode   string
}

type MeterValidationResult struct {
	Valid        bool
	MeterType    string
	CustomerName string
	Address      string
	// Capabilities as reported by the provider for this meter.
	RemoteCreditSupported  bool
	RemoteReadingSupported bool
	// CanVend is true when the provider omitted the flag. False means do not take payment.
	CanVend bool
	// MinPurchaseMinor is the Disco floor in kobo; 0 means none.
	MinPurchaseMinor int64
	// ArrearsMinor is outstanding debt in kobo that may be taken from the vend amount.
	ArrearsMinor int64
}

type VendRequest struct {
	TransactionID  string
	MeterNumber    string
	DiscoCode      string
	Amount         money.Amount
	IdempotencyKey string
	// MeterType is PREPAID or POSTPAID when known from a prior validation.
	MeterType string
	// Phone is the customer or recipient MSISDN some aggregators require on pay.
	Phone string
}

type VendStatus string

const (
	VendPending   VendStatus = "PENDING"
	VendSucceeded VendStatus = "SUCCEEDED"
	VendFailed    VendStatus = "FAILED"
	VendNotFound  VendStatus = "NOT_FOUND" // provider has no record: safe to retry
)

type VendResult struct {
	ProviderRef string
	Status      VendStatus
	Token       string
	UnitsKWh    string
	FailureCode string
}

type VendingProvider interface {
	Name() string
	// NewVendKey returns the provider-facing idempotency key for one
	// transaction. It must stay stable for the life of that vend attempt
	// once the caller records it (some aggregators require a dated prefix).
	NewVendKey(transactionID string, at time.Time) string
	ValidateMeter(ctx context.Context, req MeterValidationRequest) (MeterValidationResult, error)
	Vend(ctx context.Context, req VendRequest) (VendResult, error)
	// GetVendStatus looks up by our idempotency key so it works even when the
	// original response (and provider ref) was lost.
	GetVendStatus(ctx context.Context, idempotencyKey string) (VendResult, error)
}

// ---- Remote credit (AMI / HES) --------------------------------------------

type CreditStatus string

const (
	CreditAccepted  CreditStatus = "ACCEPTED"  // command queued; NOT proof of credit
	CreditConfirmed CreditStatus = "CONFIRMED" // meter acknowledged
	CreditFailed    CreditStatus = "FAILED"
	CreditNotFound  CreditStatus = "NOT_FOUND"
)

type RemoteCreditRequest struct {
	TransactionID  string
	MeterNumber    string
	Token          string // some HES deliver the STS token to the meter
	Amount         money.Amount
	IdempotencyKey string
}

type RemoteCreditResult struct {
	ProviderRef string
	Status      CreditStatus
	FailureCode string
	At          time.Time
}

type RemoteCreditProvider interface {
	Name() string
	CheckEligibility(ctx context.Context, meterNumber string) (eligible bool, reachable *bool, err error)
	InitiateCredit(ctx context.Context, req RemoteCreditRequest) (RemoteCreditResult, error)
	GetCreditStatus(ctx context.Context, idempotencyKey string) (RemoteCreditResult, error)
}

// RemoteCreditWebhook is a hint that a HES may have moved a credit command.
// Callers MUST still GetCreditStatus; never trust a status in the body.
type RemoteCreditWebhook struct {
	IdempotencyKey string
	Relevant       bool
}

// RemoteCreditWebhookParser is optional. Adapters that can receive partner
// callbacks implement it; in-process mocks used only in unit tests may not.
type RemoteCreditWebhookParser interface {
	ParseWebhook(h http.Header, body []byte) (RemoteCreditWebhook, error)
}

// ---- Remote meter reading (AMI / HES) -------------------------------------

// ErrReadingUnavailable means no confirmed meter balance reading is available.
var ErrReadingUnavailable = errors.New("provider: meter reading unavailable")

type MeterReadingProvider interface {
	Name() string
	LatestReading(ctx context.Context, meterNumber string) (kWh float64, at time.Time, err error)
}
