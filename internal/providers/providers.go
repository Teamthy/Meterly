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
	ProviderRef string
	Status      PaymentStatus
	Amount      money.Amount
	PaidAt      *time.Time
}

type RefundRequest struct {
	ProviderRef string
	Amount      money.Amount
	Reference   string
}

type PaymentProvider interface {
	Name() string
	InitializePayment(ctx context.Context, req PaymentInit) (PaymentInitResult, error)
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
}

type VendRequest struct {
	TransactionID  string
	MeterNumber    string
	DiscoCode      string
	Amount         money.Amount
	IdempotencyKey string
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
