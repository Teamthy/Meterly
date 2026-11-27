package notifications

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// PermanentError marks a definitive non-retryable rejection (e.g. invalid
// destination or blacklisted recipient).
type PermanentError struct {
	Code    string
	Message string
}

func (e *PermanentError) Error() string {
	if e.Message == "" {
		return "notifications: permanent error: " + e.Code
	}
	return fmt.Sprintf("notifications: %s (%s)", e.Message, e.Code)
}

func IsPermanent(err error) (string, bool) {
	var pe *PermanentError
	if errors.As(err, &pe) {
		if pe.Code != "" {
			return pe.Code, true
		}
		return "PERMANENT_FAILURE", true
	}
	return "", false
}

// RetryableError marks a transient rejection where the provider confirmed the
// message was not sent (e.g. HTTP 429 rate limit).
type RetryableError struct {
	Code    string
	Message string
}

func (e *RetryableError) Error() string {
	if e.Message == "" {
		return "notifications: retryable error: " + e.Code
	}
	return fmt.Sprintf("notifications: %s (%s)", e.Message, e.Code)
}

func IsRetryable(err error) (string, bool) {
	var re *RetryableError
	if errors.As(err, &re) {
		if re.Code != "" {
			return re.Code, true
		}
		return "RETRYABLE_FAILURE", true
	}
	return "", false
}

type SendRequest struct {
	DeliveryID                  uuid.UUID
	IdempotencyKey              string
	Message                     Message
	TemplateID                  string
	TemplateVersion             string
	Locale                      string
	WhatsAppTemplateName        string
	TemplateParams              []string
	WithinCustomerServiceWindow bool
	PreferTextInWindow          bool
}

type SendResult struct {
	ProviderRef string
}

type ProviderStatus string

const (
	ProviderStatusSent     ProviderStatus = "SENT"
	ProviderStatusPending  ProviderStatus = "PENDING"
	ProviderStatusFailed   ProviderStatus = "FAILED"
	ProviderStatusNotFound ProviderStatus = "NOT_FOUND"
)

// Provider sends notifications over a single channel. Network timeouts, 5xx
// responses and unparseable 2xx responses MUST be returned wrapped with
// providers.ErrAmbiguous so the delivery worker marks the job UNKNOWN rather
// than resending blindly.
type Provider interface {
	Name() string
	Channel() Channel
	SendDelivery(ctx context.Context, req SendRequest) (SendResult, error)
}

// StatusChecker is implemented by providers that expose a server-side message
// status endpoint for reconciling UNKNOWN deliveries.
type StatusChecker interface {
	CheckDeliveryStatus(ctx context.Context, idempotencyKey, providerRef string) (status ProviderStatus, errCode string, err error)
}

type InboundOptOut struct {
	Destination string // normalised E.164 phone or lowercase email
	Channel     Channel
}

type InboundResult struct {
	Relevant         bool
	OptOuts          []InboundOptOut
	WhatsAppInbounds []string // normalised E.164 phones that sent an inbound WhatsApp message
}

// InboundVerifier authenticates and parses inbound provider webhooks (STOP
// keywords, spam complaints). Signature failures must return
// providers.ErrBadSignature.
type InboundVerifier interface {
	Name() string
	ParseInbound(h http.Header, body []byte) (InboundResult, error)
}

// ChallengeVerifier is optionally implemented by providers (WhatsApp Cloud API)
// that verify webhook endpoints via a GET challenge handshake.
type ChallengeVerifier interface {
	VerifyChallenge(mode, verifyToken, challenge string) (string, error)
}

type wrappedSender struct {
	name string
	ch   Channel
	s    Sender
}

// WrapSender adapts a simple Sender (such as MockSender) to Provider.
func WrapSender(name string, ch Channel, s Sender) Provider {
	return &wrappedSender{name: name, ch: ch, s: s}
}

func (w *wrappedSender) Name() string     { return w.name }
func (w *wrappedSender) Channel() Channel { return w.ch }

func (w *wrappedSender) SendDelivery(ctx context.Context, req SendRequest) (SendResult, error) {
	msg := req.Message
	msg.Channel = w.ch
	if err := w.s.Send(ctx, msg); err != nil {
		return SendResult{}, err
	}
	return SendResult{ProviderRef: w.name + ":" + req.DeliveryID.String()}, nil
}

// IsStopKeyword reports whether a trimmed inbound text message requests opt-out.
func IsStopKeyword(text string) bool {
	cleaned := strings.ToUpper(strings.TrimSpace(text))
	cleaned = strings.Trim(cleaned, ".!?,;:\"'")
	switch cleaned {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "OPTOUT", "OPT OUT":
		return true
	default:
		return false
	}
}
