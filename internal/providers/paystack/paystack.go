// Package paystack adapts the Paystack API to providers.PaymentProvider.
//
//   - Initialize: POST /transaction/initialize (amount in kobo).
//   - Verify:     GET  /transaction/verify/:reference, the source of truth.
//   - List:       GET /transaction, GET /refund, GET /settlement, GET /dispute
//     (https://paystack.com/docs/api/transaction/#list,
//     https://paystack.com/docs/api/refund/#list,
//     https://paystack.com/docs/api/settlement/#list,
//     https://paystack.com/docs/api/dispute/).
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
	"github.com/teamthy/meterly/internal/observability"
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

func (e *APIError) Unwrap() error {
	msg := strings.ToLower(e.Message)
	if e.HTTPStatus == http.StatusNotFound || strings.Contains(msg, "transaction reference not found") {
		return providers.ErrPaymentNotFound
	}
	return nil
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
	observability.InjectHTTP(ctx, req.Header)
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

type authData struct {
	AuthorizationCode string `json:"authorization_code"`
	CardType          string `json:"card_type"`
	Brand             string `json:"brand"`
	Last4             string `json:"last4"`
	ExpMonth          string `json:"exp_month"`
	ExpYear           string `json:"exp_year"`
	Channel           string `json:"channel"`
	Reusable          bool   `json:"reusable"`
	Signature         string `json:"signature"`
}

type txnData struct {
	ID        int64      `json:"id"`
	Status    string     `json:"status"`
	Reference string     `json:"reference"`
	Amount    int64      `json:"amount"`
	Currency  string     `json:"currency"`
	Channel   string     `json:"channel"`
	PaidAt    *time.Time `json:"paid_at"`
	Customer  struct {
		Email string `json:"email"`
	} `json:"customer"`
	Authorization *authData `json:"authorization"`
}

// mapStatus is deliberately conservative: only "success" counts as paid,
// and only definitive non-payment states count as failed.
func mapStatus(s string) providers.PaymentStatus {
	switch s {
	case "success":
		return providers.PaymentSucceeded
	case "failed", "reversed":
		return providers.PaymentFailed
	default: // abandoned, ongoing, pending, processing, queued, send_otp, unknown values
		return providers.PaymentPending
	}
}

func parseCardAuth(d txnData) *providers.CardAuthorization {
	a := d.Authorization
	if a == nil || strings.TrimSpace(a.AuthorizationCode) == "" {
		return nil
	}
	expMonth, _ := strconv.Atoi(strings.TrimSpace(a.ExpMonth))
	expYear, _ := strconv.Atoi(strings.TrimSpace(a.ExpYear))
	if expYear > 0 && expYear < 100 {
		expYear += 2000
	}
	brand := strings.ToLower(strings.TrimSpace(a.Brand))
	if brand == "" {
		parts := strings.Fields(strings.ToLower(strings.TrimSpace(a.CardType)))
		if len(parts) > 0 {
			brand = parts[0]
		}
	}
	ch := strings.ToLower(strings.TrimSpace(a.Channel))
	if ch == "" {
		ch = strings.ToLower(strings.TrimSpace(d.Channel))
	}
	return &providers.CardAuthorization{
		AuthorizationCode: strings.TrimSpace(a.AuthorizationCode),
		Reusable:          a.Reusable,
		CardBrand:         brand,
		Last4:             strings.TrimSpace(a.Last4),
		ExpMonth:          expMonth,
		ExpYear:           expYear,
		Channel:           ch,
		Signature:         strings.TrimSpace(a.Signature),
		CustomerEmail:     strings.ToLower(strings.TrimSpace(d.Customer.Email)),
	}
}

func toVerification(d txnData) providers.PaymentVerification {
	return providers.PaymentVerification{
		ProviderRef:   d.Reference,
		Status:        mapStatus(d.Status),
		Amount:        money.New(d.Amount, money.Currency(d.Currency)),
		PaidAt:        d.PaidAt,
		Authorization: parseCardAuth(d),
	}
}

func (c *Client) ChargeSaved(ctx context.Context, req providers.ChargeRequest) (providers.PaymentVerification, error) {
	if strings.TrimSpace(req.AuthorizationRef) == "" {
		return providers.PaymentVerification{}, &APIError{Message: "authorization_code is required"}
	}
	if strings.TrimSpace(req.Email) == "" {
		return providers.PaymentVerification{}, &APIError{Message: "customer email is required"}
	}
	if strings.TrimSpace(req.Reference) == "" {
		return providers.PaymentVerification{}, &APIError{Message: "reference is required"}
	}
	if req.Amount.Currency != money.NGN || req.Amount.Minor <= 0 {
		return providers.PaymentVerification{}, &APIError{Message: "amount must be a positive NGN value"}
	}
	body := map[string]any{
		"authorization_code": req.AuthorizationRef,
		"email":              req.Email,
		"amount":             strconv.FormatInt(req.Amount.Minor, 10),
		"currency":           string(req.Amount.Currency),
		"reference":          req.Reference,
	}
	var d txnData
	if err := c.call(ctx, http.MethodPost, "/transaction/charge_authorization", body, &d); err != nil {
		return providers.PaymentVerification{}, err
	}
	if d.Reference == "" {
		return providers.PaymentVerification{}, fmt.Errorf("%w: paystack charge_authorization returned no reference", providers.ErrAmbiguous)
	}
	if d.Reference != req.Reference {
		return providers.PaymentVerification{}, fmt.Errorf("%w: paystack charge_authorization returned reference %q for %q", providers.ErrAmbiguous, d.Reference, req.Reference)
	}
	return toVerification(d), nil
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
	return toVerification(d), nil
}

type refundData struct {
	ID           int64  `json:"id"`
	Status       string `json:"status"`
	Amount       int64  `json:"amount"`
	MerchantNote string `json:"merchant_note"`
}

func (c *Client) RefundPayment(ctx context.Context, req providers.RefundRequest) (string, error) {
	d, err := c.verify(ctx, req.ProviderRef)
	if err != nil {
		return "", err
	}
	// Idempotency: Paystack has no idempotency key for refunds, so look for
	// an existing non-failed refund matching our merchant_note (or amount
	// when merchant_note was not echoed) before creating one.
	var existing []refundData
	if err := c.call(ctx, http.MethodGet, "/refund?transaction="+strconv.FormatInt(d.ID, 10), nil, &existing); err != nil {
		return "", err
	}
	for _, r := range existing {
		if r.Status == "failed" {
			continue
		}
		if r.MerchantNote != "" {
			if r.MerchantNote == req.Reference {
				return strconv.FormatInt(r.ID, 10), nil
			}
			continue
		}
		if r.Amount == req.Amount.Minor {
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

// ListedTxn is one row from GET /transaction.
type ListedTxn struct {
	Reference string
	Status    string
	Amount    int64
	Currency  string
	PaidAt    *time.Time
}

// ListedRefundRow is one row from GET /refund.
type ListedRefundRow struct {
	TransactionRef string
	Amount         int64
	Status         string
}

// Settlement is one row from GET /settlement.
type Settlement struct {
	ID              int64
	Status          string
	Currency        string
	TotalAmount     int64
	EffectiveAmount int64
	TotalFees       int64
	SettlementDate  time.Time
}

// Dispute is one row from GET /dispute.
// Docs: https://paystack.com/docs/api/dispute/ and https://paystack.com/docs/payments/manage-disputes/
type Dispute struct {
	ID           int64
	RefundAmount int64
	Currency     string
	Status       string
	Resolution   string
	Category     string
	Reference    string
	DueAt        *time.Time
}

func (c *Client) ListDisputes(ctx context.Context, from, to time.Time) ([]Dispute, error) {
	var all []Dispute
	err := c.listPages(ctx, "/dispute", from, to, func(raw json.RawMessage) error {
		var rows []struct {
			ID           int64  `json:"id"`
			RefundAmount int64  `json:"refund_amount"`
			Currency     string `json:"currency"`
			Status       string `json:"status"`
			Resolution   string `json:"resolution"`
			Category     string `json:"category"`
			Transaction  struct {
				Reference string `json:"reference"`
			} `json:"transaction"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("%w: paystack: dispute list", providers.ErrAmbiguous)
		}
		for _, d := range rows {
			all = append(all, Dispute{
				ID: d.ID, RefundAmount: d.RefundAmount, Currency: d.Currency,
				Status: d.Status, Resolution: d.Resolution, Category: d.Category,
				Reference: d.Transaction.Reference,
			})
		}
		return nil
	})
	return all, err
}

func (c *Client) FetchDispute(ctx context.Context, id int64) (Dispute, error) {
	var row struct {
		ID           int64  `json:"id"`
		RefundAmount int64  `json:"refund_amount"`
		Currency     string `json:"currency"`
		Status       string `json:"status"`
		Resolution   string `json:"resolution"`
		Category     string `json:"category"`
		Transaction  struct {
			Reference string `json:"reference"`
		} `json:"transaction"`
	}
	if err := c.call(ctx, http.MethodGet, "/dispute/"+strconv.FormatInt(id, 10), nil, &row); err != nil {
		return Dispute{}, err
	}
	return Dispute{
		ID: row.ID, RefundAmount: row.RefundAmount, Currency: row.Currency,
		Status: row.Status, Resolution: row.Resolution, Category: row.Category,
		Reference: row.Transaction.Reference,
	}, nil
}

func (c *Client) AddEvidence(ctx context.Context, id int64, customerEmail, customerName, customerPhone, serviceDetails string) error {
	return c.call(ctx, http.MethodPost, "/dispute/"+strconv.FormatInt(id, 10)+"/evidence", map[string]any{
		"customer_email":  customerEmail,
		"customer_name":   customerName,
		"customer_phone":  customerPhone,
		"service_details": serviceDetails,
	}, nil)
}

func (c *Client) ResolveDispute(ctx context.Context, id int64, resolution, message string, refundAmount int64) error {
	return c.call(ctx, http.MethodPut, "/dispute/"+strconv.FormatInt(id, 10)+"/resolve", map[string]any{
		"resolution":    resolution,
		"message":       message,
		"refund_amount": refundAmount,
	}, nil)
}

// africaLagos is WAT (UTC+1, no DST) so list windows match the recon business date.
var africaLagos = time.FixedZone("WAT", 3600)

func (c *Client) listPages(ctx context.Context, path string, from, to time.Time, decode func(json.RawMessage) error) error {
	for page := 1; page <= 40; page++ {
		q := url.Values{}
		end := to.Add(-time.Nanosecond)
		q.Set("from", from.In(africaLagos).Format("2006-01-02"))
		q.Set("to", end.In(africaLagos).Format("2006-01-02"))
		q.Set("perPage", "50")
		q.Set("page", strconv.Itoa(page))
		var raw json.RawMessage
		if err := c.call(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &raw); err != nil {
			return err
		}
		n, err := decodePageCount(raw)
		if err != nil {
			return err
		}
		if err := decode(raw); err != nil {
			return err
		}
		if n < 50 {
			return nil
		}
	}
	return nil
}

func decodePageCount(raw json.RawMessage) (int, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, fmt.Errorf("%w: paystack: unexpected list shape", providers.ErrAmbiguous)
	}
	return len(arr), nil
}

func (c *Client) ListTransactions(ctx context.Context, from, to time.Time) ([]ListedTxn, error) {
	var all []ListedTxn
	err := c.listPages(ctx, "/transaction", from, to, func(raw json.RawMessage) error {
		var rows []txnData
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("%w: paystack: transaction list", providers.ErrAmbiguous)
		}
		for _, d := range rows {
			all = append(all, ListedTxn{Reference: d.Reference, Status: d.Status, Amount: d.Amount, Currency: d.Currency, PaidAt: d.PaidAt})
		}
		return nil
	})
	return all, err
}

func (c *Client) ListRefunds(ctx context.Context, from, to time.Time) ([]ListedRefundRow, error) {
	var all []ListedRefundRow
	err := c.listPages(ctx, "/refund", from, to, func(raw json.RawMessage) error {
		var rows []struct {
			Status      string `json:"status"`
			Amount      int64  `json:"amount"`
			Transaction struct {
				Reference string `json:"reference"`
			} `json:"transaction"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("%w: paystack: refund list", providers.ErrAmbiguous)
		}
		for _, d := range rows {
			all = append(all, ListedRefundRow{TransactionRef: d.Transaction.Reference, Amount: d.Amount, Status: d.Status})
		}
		return nil
	})
	return all, err
}

func (c *Client) ListSettlements(ctx context.Context, from, to time.Time) ([]Settlement, error) {
	var all []Settlement
	err := c.listPages(ctx, "/settlement", from, to, func(raw json.RawMessage) error {
		var rows []struct {
			ID              int64     `json:"id"`
			Status          string    `json:"status"`
			Currency        string    `json:"currency"`
			TotalAmount     int64     `json:"total_amount"`
			EffectiveAmount int64     `json:"effective_amount"`
			TotalFees       int64     `json:"total_fees"`
			SettlementDate  time.Time `json:"settlement_date"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("%w: paystack: settlement list", providers.ErrAmbiguous)
		}
		for _, d := range rows {
			all = append(all, Settlement{
				ID: d.ID, Status: d.Status, Currency: d.Currency,
				TotalAmount: d.TotalAmount, EffectiveAmount: d.EffectiveAmount, TotalFees: d.TotalFees,
				SettlementDate: d.SettlementDate,
			})
		}
		return nil
	})
	return all, err
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
			ID           int64  `json:"id"`
			RefundAmount int64  `json:"refund_amount"`
			Currency     string `json:"currency"`
			Status       string `json:"status"`
			Resolution   string `json:"resolution"`
			Category     string `json:"category"`
			DueAt        string `json:"dueAt"`
			Reference    string `json:"reference"`
			Transaction  struct {
				Reference string `json:"reference"`
				Amount    int64  `json:"amount"`
				Currency  string `json:"currency"`
			} `json:"transaction"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.Event == "" {
		return providers.WebhookEvent{}, errors.New("paystack: malformed webhook body")
	}
	ref := strings.TrimSpace(evt.Data.Reference)
	if ref == "" {
		ref = strings.TrimSpace(evt.Data.Transaction.Reference)
	}
	switch {
	case evt.Event == "charge.success":
		if ref == "" {
			return providers.WebhookEvent{}, errors.New("paystack: charge.success without reference")
		}
		return providers.WebhookEvent{Type: evt.Event, Reference: ref, Relevant: true}, nil
	case strings.HasPrefix(evt.Event, "charge.dispute"):
		// Pause the buyer. Never treat this as a payment success.
		amt := evt.Data.RefundAmount
		if amt <= 0 {
			amt = evt.Data.Transaction.Amount
		}
		cur := evt.Data.Currency
		if cur == "" {
			cur = evt.Data.Transaction.Currency
		}
		out := providers.WebhookEvent{
			Type: evt.Event, Reference: ref, Relevant: true,
			AmountMinor: amt, Currency: cur,
			Status: evt.Data.Status, Resolution: evt.Data.Resolution, Category: evt.Data.Category,
		}
		if evt.Data.ID != 0 {
			out.DisputeID = strconv.FormatInt(evt.Data.ID, 10)
		}
		if evt.Data.DueAt != "" {
			if ts, err := time.Parse(time.RFC3339, evt.Data.DueAt); err == nil {
				t := ts.UTC()
				out.DueAt = &t
			}
		}
		return out, nil
	default:
		// refund.*, transfer.*, subscription.* and so on: acknowledged, ignored.
		return providers.WebhookEvent{Type: evt.Event}, nil
	}
}
