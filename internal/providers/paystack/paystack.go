// Package paystack adapts the Paystack API to providers.PaymentProvider.
//
//   - Initialize: POST /transaction/initialize (amount in kobo).
//   - Verify:     GET  /transaction/verify/:reference, the source of truth.
//   - Refund:     POST /refund, made idempotent by first listing existing
//     refunds for the transaction.
//   - Webhooks:   HMAC-SHA512 of the raw body keyed with the secret key, in
//     the x-paystack-signature header, compared in constant time.
//
// Error semantics: a network error, timeout or 5xx after the request was
// sent is providers.ErrAmbiguous (the operation may have happened). A 4xx
// is a definitive rejection. Unrecognised statuses map to "pending", never
// to success or failure.
package paystack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
)

const DefaultBaseURL = "https://api.paystack.co"

type Config struct {
	SecretKey   string
	BaseURL     string // DefaultBaseURL unless testing
	CallbackURL string // optional: where checkout redirects the customer
	HTTPClient  *http.Client
}

type Client struct {
	secret      string
	base        string
	callbackURL string
	http        *http.Client
}

func New(cfg Config) (*Client, error) {
	if cfg.SecretKey == "" {
		return nil, errors.New("paystack: secret key is required")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{secret: cfg.SecretKey, base: base, callbackURL: cfg.CallbackURL, http: hc}, nil
}

func (c *Client) Name() string { return "paystack" }

// APIError is a definitive rejection from Paystack (HTTP 4xx, or
// status=false on a 2xx).
type APIError struct {
	HTTPStatus int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("paystack: %s (HTTP %d)", e.Message, e.HTTPStatus)
}

type envelope struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// call performs a request and decodes data into out.
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		// Never include the URL-bearing error text verbatim in callers' logs
		// beyond this: it has no secrets, but keep the message short.
		return fmt.Errorf("%w: paystack %s %s: %v", providers.ErrAmbiguous, method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: paystack read: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode >= 500 {
		return fmt.Errorf("%w: paystack HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if res.StatusCode >= 400 {
			return &APIError{HTTPStatus: res.StatusCode, Message: "unparseable error response"}
		}
		return fmt.Errorf("%w: paystack: unparseable response", providers.ErrAmbiguous)
	}
	if res.StatusCode >= 400 || !env.Status {
		return &APIError{HTTPStatus: res.StatusCode, Message: env.Message}
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%w: paystack: unexpected data shape", providers.ErrAmbiguous)
		}
	}
	return nil
}

// reference returns a Paystack reference for one initialisation attempt.
// Paystack rejects a reused reference, and a retry after a crash cannot
// recover the first attempt's checkout URL, so each attempt gets a fresh
// suffix. Only the attempt we committed is ever shown to the customer, so
// an orphaned attempt has no checkout link anyone can pay.
func reference(ours string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return ours + "." + hex.EncodeToString(b)
}

func (c *Client) InitializePayment(ctx context.Context, req providers.PaymentInit) (providers.PaymentInitResult, error) {
	if req.Email == "" {
		return providers.PaymentInitResult{}, &APIError{HTTPStatus: 0, Message: "customer email is required"}
	}
	if req.Amount.Currency != money.NGN || req.Amount.Minor <= 0 {
		return providers.PaymentInitResult{}, &APIError{Message: "amount must be a positive NGN value"}
	}
	body := map[string]any{
		"email":     req.Email,
		"amount":    strconv.FormatInt(req.Amount.Minor, 10), // kobo
		"currency":  string(req.Amount.Currency),
		"reference": reference(req.Reference),
		"metadata":  map[string]string{"transaction_id": req.Reference},
	}
	if c.callbackURL != "" {
		body["callback_url"] = c.callbackURL
	}
	var data struct {
		AuthorizationURL string `json:"authorization_url"`
		Reference        string `json:"reference"`
	}
	if err := c.call(ctx, http.MethodPost, "/transaction/initialize", body, &data); err != nil {
		return providers.PaymentInitResult{}, err
	}
	if data.Reference == "" || data.AuthorizationURL == "" {
		return providers.PaymentInitResult{}, fmt.Errorf("%w: paystack initialize returned no reference or URL", providers.ErrAmbiguous)
	}
	return providers.PaymentInitResult{ProviderRef: data.Reference, CheckoutURL: data.AuthorizationURL}, nil
}

type txnData struct {
	ID        int64      `json:"id"`
	Status    string     `json:"status"`
	Reference string     `json:"reference"`
	Amount    int64      `json:"amount"`
	Currency  string     `json:"currency"`
	PaidAt    *time.Time `json:"paid_at"`
}

// mapStatus is deliberately conservative: only "success" counts as paid,
// and only definitive non-payment states count as failed.
func mapStatus(s string) providers.PaymentStatus {
	switch s {
	case "success":
		return providers.PaymentSucceeded
	case "failed", "reversed":
		return providers.PaymentFailed
	default: // abandoned, ongoing, pending, processing, queued, unknown values
		return providers.PaymentPending
	}
}

func (c *Client) verify(ctx context.Context, ref string) (txnData, error) {
	var d txnData
	err := c.call(ctx, http.MethodGet, "/transaction/verify/"+url.PathEscape(ref), nil, &d)
	return d, err
}

func (c *Client) VerifyPayment(ctx context.Context, ref string) (providers.PaymentVerification, error) {
	d, err := c.verify(ctx, ref)
	if err != nil {
		return providers.PaymentVerification{}, err
	}
	if d.Reference != ref {
		return providers.PaymentVerification{}, fmt.Errorf("%w: paystack verify returned reference %q for %q", providers.ErrAmbiguous, d.Reference, ref)
	}
	return providers.PaymentVerification{
		ProviderRef: d.Reference,
		Status:      mapStatus(d.Status),
		Amount:      money.New(d.Amount, money.Currency(d.Currency)),
		PaidAt:      d.PaidAt,
	}, nil
}

type refundData struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Amount int64  `json:"amount"`
}

func (c *Client) RefundPayment(ctx context.Context, req providers.RefundRequest) (string, error) {
	d, err := c.verify(ctx, req.ProviderRef)
	if err != nil {
		return "", err
	}
	// Idempotency: Paystack has no idempotency key for refunds, so look for
	// an existing refund on this transaction before creating one.
	var existing []refundData
	if err := c.call(ctx, http.MethodGet, "/refund?transaction="+strconv.FormatInt(d.ID, 10), nil, &existing); err != nil {
		return "", err
	}
	for _, r := range existing {
		if r.Status != "failed" {
			return strconv.FormatInt(r.ID, 10), nil
		}
	}
	var created refundData
	err = c.call(ctx, http.MethodPost, "/refund", map[string]any{
		"transaction":   req.ProviderRef,
		"amount":        strconv.FormatInt(req.Amount.Minor, 10),
		"currency":      string(req.Amount.Currency),
		"merchant_note": req.Reference,
	}, &created)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

// Sign computes the webhook signature for body (exported for tests and
// local tooling).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (c *Client) ParseWebhook(h http.Header, body []byte) (providers.WebhookEvent, error) {
	got, err := hex.DecodeString(h.Get("X-Paystack-Signature"))
	if err != nil || len(got) == 0 {
		return providers.WebhookEvent{}, providers.ErrBadSignature
	}
	want, _ := hex.DecodeString(Sign(c.secret, body))
	if !hmac.Equal(got, want) {
		return providers.WebhookEvent{}, providers.ErrBadSignature
	}
	var evt struct {
		Event string `json:"event"`
		Data  struct {
			Reference string `json:"reference"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.Event == "" {
		return providers.WebhookEvent{}, errors.New("paystack: malformed webhook body")
	}
	switch evt.Event {
	case "charge.success":
		if evt.Data.Reference == "" {
			return providers.WebhookEvent{}, errors.New("paystack: charge.success without reference")
		}
		return providers.WebhookEvent{Type: evt.Event, Reference: evt.Data.Reference, Relevant: true}, nil
	default:
		// refund.*, transfer.*, subscription.* and so on: acknowledged, ignored.
		return providers.WebhookEvent{Type: evt.Event}, nil
	}
}
