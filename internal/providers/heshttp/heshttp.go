// Package heshttp is Meterly's signed-HTTP remote-credit adapter.
//
// This is NOT a Hexing, Conlog, Mojec or DisCo AMI client. Those vendor
// APIs are not documented here and must not be invented. heshttp speaks
// Meterly's outbound HES gateway contract so that:
//
//   - a partner can implement a thin gateway in front of their HES, or
//   - we can later write a vendor-specific translator that still satisfies
//     providers.RemoteCreditProvider.
//
// Contract (all JSON, HMAC-SHA256 hex in X-Meterly-Signature over the raw body):
//
//	POST {base}/eligibility  {"meter_number":"..."}
//	  → {"eligible":true,"reachable":true}
//
//	POST {base}/credit
//	  Idempotency-Key: credit:<txn-uuid>
//	  {"transaction_id","meter_number","token","amount_minor","currency","idempotency_key"}
//	  → {"provider_ref","status":"ACCEPTED|CONFIRMED|FAILED","failure_code","at"}
//
//	GET {base}/credit?idempotency_key=credit:<txn-uuid>
//	  → same body, or {"status":"NOT_FOUND"}
//
// Inbound confirmation (partner → Meterly):
//
//	POST /v1/webhooks/remote-credit/heshttp
//	  {"idempotency_key":"credit:<txn-uuid>"}
//
// Timeouts, 5xx and empty bodies on InitiateCredit are providers.ErrAmbiguous.
// GetCreditStatus network errors are returned as-is (safe to retry; no send).
// Status ACCEPTED is not delivered. Only CONFIRMED means the meter acknowledged.
package heshttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/providers"
)

const (
	Name             = "heshttp"
	SignatureHeader  = "X-Meterly-Signature"
	defaultTimeout   = 20 * time.Second
	maxBody          = 1 << 20
	minHMACSecretLen = 16
)

type Config struct {
	BaseURL    string
	HMACSecret string
	HTTPClient *http.Client
}

type Client struct {
	base   string
	secret []byte
	http   *http.Client
}

func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	secret := strings.TrimSpace(cfg.HMACSecret)
	if base == "" {
		return nil, errors.New("heshttp: BaseURL is required")
	}
	if len(secret) < minHMACSecretLen {
		return nil, errors.New("heshttp: HMACSecret must be at least 16 characters")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{base: base, secret: []byte(secret), http: hc}, nil
}

func (c *Client) Name() string { return Name }

func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (c *Client) sign(body []byte) string {
	m := hmac.New(sha256.New, c.secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (c *Client) checkSig(header string, body []byte) bool {
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil || len(got) == 0 {
		return false
	}
	want, err := hex.DecodeString(c.sign(body))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func (c *Client) CheckEligibility(ctx context.Context, meterNumber string) (bool, *bool, error) {
	raw, status, err := c.do(ctx, http.MethodPost, "/eligibility", map[string]any{"meter_number": meterNumber}, true)
	if err != nil {
		return false, nil, err
	}
	if status >= 400 {
		return false, nil, nil
	}
	var out struct {
		Eligible  bool  `json:"eligible"`
		Reachable *bool `json:"reachable"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return false, nil, fmt.Errorf("%w: heshttp eligibility unparseable", providers.ErrAmbiguous)
	}
	return out.Eligible, out.Reachable, nil
}

func (c *Client) InitiateCredit(ctx context.Context, req providers.RemoteCreditRequest) (providers.RemoteCreditResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || strings.TrimSpace(req.MeterNumber) == "" {
		return providers.RemoteCreditResult{Status: providers.CreditFailed, FailureCode: "INVALID_REQUEST"}, nil
	}
	body := map[string]any{
		"transaction_id":  req.TransactionID,
		"meter_number":    req.MeterNumber,
		"token":           req.Token,
		"amount_minor":    req.Amount.Minor,
		"currency":        req.Amount.Currency,
		"idempotency_key": req.IdempotencyKey,
	}
	raw, status, err := c.doWithIdempotency(ctx, http.MethodPost, "/credit", body, req.IdempotencyKey, true)
	if err != nil {
		return providers.RemoteCreditResult{}, err
	}
	if status == http.StatusConflict {
		return c.GetCreditStatus(ctx, req.IdempotencyKey)
	}
	if status >= 400 && status < 500 {
		return providers.RemoteCreditResult{Status: providers.CreditFailed, FailureCode: "HES_REJECTED"}, nil
	}
	res, err := decodeCredit(raw)
	if err != nil {
		return providers.RemoteCreditResult{}, fmt.Errorf("%w: %v", providers.ErrAmbiguous, err)
	}
	return res, nil
}

func (c *Client) GetCreditStatus(ctx context.Context, idempotencyKey string) (providers.RemoteCreditResult, error) {
	q := url.Values{"idempotency_key": {idempotencyKey}}
	raw, status, err := c.do(ctx, http.MethodGet, "/credit?"+q.Encode(), nil, false)
	if err != nil {
		return providers.RemoteCreditResult{}, err
	}
	if status == http.StatusNotFound {
		return providers.RemoteCreditResult{Status: providers.CreditNotFound}, nil
	}
	if status >= 400 {
		return providers.RemoteCreditResult{}, fmt.Errorf("heshttp status HTTP %d", status)
	}
	res, err := decodeCredit(raw)
	if err != nil {
		return providers.RemoteCreditResult{}, err
	}
	return res, nil
}

func (c *Client) ParseWebhook(h http.Header, body []byte) (providers.RemoteCreditWebhook, error) {
	if !c.checkSig(h.Get(SignatureHeader), body) {
		return providers.RemoteCreditWebhook{}, providers.ErrBadSignature
	}
	var evt struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if json.Unmarshal(body, &evt) != nil || strings.TrimSpace(evt.IdempotencyKey) == "" {
		return providers.RemoteCreditWebhook{}, fmt.Errorf("heshttp: malformed webhook")
	}
	return providers.RemoteCreditWebhook{IdempotencyKey: evt.IdempotencyKey, Relevant: true}, nil
}

func decodeCredit(raw []byte) (providers.RemoteCreditResult, error) {
	var out struct {
		ProviderRef string `json:"provider_ref"`
		Status      string `json:"status"`
		FailureCode string `json:"failure_code"`
		At          string `json:"at"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return providers.RemoteCreditResult{}, errors.New("unparseable credit body")
	}
	st := providers.CreditStatus(strings.ToUpper(strings.TrimSpace(out.Status)))
	switch st {
	case providers.CreditAccepted, providers.CreditConfirmed, providers.CreditFailed, providers.CreditNotFound:
	default:
		return providers.RemoteCreditResult{}, fmt.Errorf("unexpected credit status %q", out.Status)
	}
	res := providers.RemoteCreditResult{ProviderRef: out.ProviderRef, Status: st, FailureCode: out.FailureCode}
	if out.At != "" {
		if t, err := time.Parse(time.RFC3339, out.At); err == nil {
			res.At = t.UTC()
		}
	}
	return res, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any, ambiguousOnNet bool) ([]byte, int, error) {
	return c.doWithIdempotency(ctx, method, path, payload, "", ambiguousOnNet)
}

func (c *Client) doWithIdempotency(ctx context.Context, method, path string, payload any, idem string, ambiguousOnNet bool) ([]byte, int, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SignatureHeader, c.sign(body))
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	observability.InjectHTTP(ctx, req.Header)
	res, err := c.http.Do(req)
	if err != nil {
		if ambiguousOnNet {
			return nil, 0, fmt.Errorf("%w: heshttp %s %s: %v", providers.ErrAmbiguous, method, path, err)
		}
		return nil, 0, fmt.Errorf("heshttp %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		if ambiguousOnNet {
			return nil, 0, fmt.Errorf("%w: heshttp read: %v", providers.ErrAmbiguous, err)
		}
		return nil, 0, err
	}
	if res.StatusCode >= 500 {
		if ambiguousOnNet {
			return nil, res.StatusCode, fmt.Errorf("%w: heshttp HTTP %d", providers.ErrAmbiguous, res.StatusCode)
		}
		return raw, res.StatusCode, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 && res.StatusCode < 400 {
		if ambiguousOnNet {
			return nil, res.StatusCode, fmt.Errorf("%w: heshttp empty body", providers.ErrAmbiguous)
		}
	}
	return raw, res.StatusCode, nil
}
