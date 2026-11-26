// Package vtpass adapts the VTpass electricity API to providers.VendingProvider.
//
// Documented surface (https://www.vtpass.com/documentation/):
//
//   - Auth: POST requests send api-key and secret-key headers.
//   - Verify: POST /merchant-verify {billersCode, serviceID, type}.
//   - Pay:    POST /pay {request_id, serviceID, billersCode, variation_code, amount, phone}.
//   - Requery: POST /requery {request_id}.
//
// request_id must start with Africa/Lagos YYYYMMDDHHII (12 digits) plus a
// unique suffix. Meterly records Client.NewVendKey at vend-claim time and
// reuses it for pay and requery so a lost response cannot double-vend.
//
// Response mapping follows https://www.vtpass.com/documentation/response-codes/:
//
//   - HTTP 5xx, timeouts, dropped connections and unparseable bodies are
//     providers.ErrAmbiguous (the pay may have happened).
//   - code 000 means "processed": inspect content.transactions.status
//     (delivered / pending / initiated). Never treat 000 alone as success.
//   - code 099 (and any undocumented code) is PENDING; requery.
//   - code 015 on requery is NOT_FOUND (safe to retry pay with a new key).
//   - code 014 (request id already exists) is reconciled by requery, not a
//     second pay.
package vtpass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/providers"
)

const (
	DefaultLiveBaseURL    = "https://vtpass.com/api"
	DefaultSandboxBaseURL = "https://sandbox.vtpass.com/api"
	Name                  = "vtpass"
)

// ServiceID maps a Meterly DisCo code to the VTpass serviceID documented for
// that distribution company. Unknown codes are rejected locally.
var ServiceID = map[string]string{
	"IKEDC":  "ikeja-electric",
	"EKEDC":  "eko-electric",
	"AEDC":   "abuja-electric",
	"IBEDC":  "ibadan-electric",
	"EEDC":   "enugu-electric",
	"PHED":   "portharcourt-electric",
	"KEDCO":  "kano-electric",
	"KAEDCO": "kaduna-electric",
	"JED":    "jos-electric",
	"YEDC":   "yola-electric",
}

type Config struct {
	APIKey       string
	SecretKey    string
	PublicKey    string // unused on POST; accepted for completeness
	BaseURL      string
	DefaultPhone string
	HTTPClient   *http.Client
	Now          func() time.Time
}

type Client struct {
	apiKey string
	secret string
	public string
	base   string
	phone  string
	http   *http.Client
	now    func() time.Time
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("vtpass: api-key and secret-key are required")
	}
	phone := digitsOnly(cfg.DefaultPhone)
	if phone == "" {
		return nil, errors.New("vtpass: default phone is required (VTpass pay needs a customer phone)")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultSandboxBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		apiKey: strings.TrimSpace(cfg.APIKey),
		secret: strings.TrimSpace(cfg.SecretKey),
		public: strings.TrimSpace(cfg.PublicKey),
		base:   base,
		phone:  phone,
		http:   hc,
		now:    now,
	}, nil
}

func (c *Client) Name() string { return Name }

func (c *Client) NewVendKey(transactionID string, at time.Time) string {
	return FormatRequestID("vend:"+transactionID, at)
}

// APIError is a definitive VTpass rejection (known failure code, or HTTP 4xx).
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("vtpass: %s (%s, HTTP %d)", e.Message, e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("vtpass: %s (HTTP %d)", e.Message, e.HTTPStatus)
}

// Lagos is Africa/Lagos (WAT, UTC+1) without depending on tzdata.
func Lagos() *time.Location { return time.FixedZone("WAT", 3600) }

// FormatRequestID builds a VTpass request_id: 12-digit Africa/Lagos
// YYYYMMDDHHII plus an alphanumeric suffix derived from ourKey.
func FormatRequestID(ourKey string, at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	stamp := at.In(Lagos()).Format("200601021504")
	var b strings.Builder
	b.WriteString(stamp)
	for _, r := range ourKey {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) < 13 {
		s = stamp + "x"
	}
	return s
}

// IsRequestID reports whether s already satisfies VTpass's dated prefix rule.
func IsRequestID(s string) bool {
	if len(s) < 12 {
		return false
	}
	for i := 0; i < 12; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (c *Client) requestID(ourKey string) string {
	if IsRequestID(ourKey) {
		return ourKey
	}
	return FormatRequestID(ourKey, c.now())
}

func ServiceIDFor(disco string) (string, error) {
	id, ok := ServiceID[strings.ToUpper(strings.TrimSpace(disco))]
	if !ok {
		return "", &APIError{HTTPStatus: 0, Code: "UNSUPPORTED_DISCO", Message: "unsupported disco " + disco}
	}
	return id, nil
}

func variationCode(meterType string) string {
	switch strings.ToLower(strings.TrimSpace(meterType)) {
	case "postpaid", "post-paid", "post_paid":
		return "postpaid"
	default:
		return "prepaid"
	}
}

func (c *Client) ValidateMeter(ctx context.Context, req providers.MeterValidationRequest) (providers.MeterValidationResult, error) {
	sid, err := ServiceIDFor(req.DiscoCode)
	if err != nil {
		return providers.MeterValidationResult{Valid: false}, err
	}
	number := strings.TrimSpace(req.MeterNumber)
	if number == "" {
		return providers.MeterValidationResult{Valid: false}, &APIError{Message: "meter number is required"}
	}
	// Type is mandatory on merchant-verify. Try prepaid first, then postpaid.
	for _, typ := range []string{"prepaid", "postpaid"} {
		res, err := c.verify(ctx, number, sid, typ)
		if err != nil {
			if providers.IsAmbiguous(err) {
				return providers.MeterValidationResult{}, err
			}
			continue
		}
		if res.Valid {
			return res, nil
		}
	}
	return providers.MeterValidationResult{Valid: false}, nil
}

func (c *Client) verify(ctx context.Context, meter, serviceID, typ string) (providers.MeterValidationResult, error) {
	raw, err := c.post(ctx, "/merchant-verify", map[string]any{
		"billersCode": meter,
		"serviceID":   serviceID,
		"type":        typ,
	})
	if err != nil {
		return providers.MeterValidationResult{}, err
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return providers.MeterValidationResult{}, err
	}
	switch env.Code {
	case "000":
		vc := verifyContent(env.Content)
		if vc.WrongBillersCode {
			return providers.MeterValidationResult{Valid: false}, nil
		}
		name := firstNonEmpty(vc.CustomerName, vc.CustomerNameAlt)
		if name == "" && vc.MeterNumber == "" && vc.MeterType == "" {
			return providers.MeterValidationResult{Valid: false}, nil
		}
		mt := strings.ToUpper(strings.TrimSpace(firstNonEmpty(vc.MeterType, typ)))
		if mt == "" {
			mt = strings.ToUpper(typ)
		}
		return providers.MeterValidationResult{
			Valid:        true,
			MeterType:    mt,
			CustomerName: name,
			Address:      firstNonEmpty(vc.Address, vc.AddressAlt),
		}, nil
	case "011", "012", "030":
		return providers.MeterValidationResult{Valid: false}, nil
	default:
		// Undocumented verify codes: do not invent success.
		return providers.MeterValidationResult{Valid: false}, nil
	}
}

func (c *Client) Vend(ctx context.Context, req providers.VendRequest) (providers.VendResult, error) {
	if err := validateVend(req); err != nil {
		return providers.VendResult{}, err
	}
	sid, err := ServiceIDFor(req.DiscoCode)
	if err != nil {
		return providers.VendResult{}, err
	}
	rid := c.requestID(req.IdempotencyKey)
	phone := digitsOnly(req.Phone)
	if phone == "" {
		phone = c.phone
	}
	variation := variationCode(req.MeterType)
	if strings.TrimSpace(req.MeterType) == "" {
		v, vErr := c.verify(ctx, req.MeterNumber, sid, "prepaid")
		if vErr != nil {
			if providers.IsAmbiguous(vErr) {
				return providers.VendResult{}, vErr
			}
		} else if v.Valid {
			variation = variationCode(v.MeterType)
		} else {
			v, vErr = c.verify(ctx, req.MeterNumber, sid, "postpaid")
			if vErr != nil {
				if providers.IsAmbiguous(vErr) {
					return providers.VendResult{}, vErr
				}
			} else if v.Valid {
				variation = variationCode(v.MeterType)
			}
		}
	}
	naira, err := nairaAmount(req.Amount)
	if err != nil {
		return providers.VendResult{}, err
	}
	raw, err := c.post(ctx, "/pay", map[string]any{
		"request_id":     rid,
		"serviceID":      sid,
		"billersCode":    req.MeterNumber,
		"variation_code": variation,
		"amount":         naira,
		"phone":          phone,
	})
	if err != nil {
		return providers.VendResult{}, err
	}
	res, err := mapPayEnvelope(raw, rid, variation, false)
	if err != nil {
		return providers.VendResult{}, err
	}
	if res.replay {
		return c.GetVendStatus(ctx, rid)
	}
	return res.VendResult, nil
}

func (c *Client) GetVendStatus(ctx context.Context, idempotencyKey string) (providers.VendResult, error) {
	rid := strings.TrimSpace(idempotencyKey)
	if rid == "" {
		return providers.VendResult{Status: providers.VendNotFound}, nil
	}
	raw, err := c.post(ctx, "/requery", map[string]any{"request_id": rid})
	if err != nil {
		return providers.VendResult{}, err
	}
	res, err := mapPayEnvelope(raw, rid, "", true)
	if err != nil {
		return providers.VendResult{}, err
	}
	if res.notFound {
		return providers.VendResult{Status: providers.VendNotFound}, nil
	}
	return res.VendResult, nil
}

func validateVend(req providers.VendRequest) error {
	if strings.TrimSpace(req.MeterNumber) == "" {
		return &APIError{Message: "meter number is required"}
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return &APIError{Message: "idempotency key is required"}
	}
	if req.Amount.Currency != money.NGN || req.Amount.Minor <= 0 {
		return &APIError{Message: "amount must be a positive NGN value"}
	}
	return nil
}

func nairaAmount(a money.Amount) (int64, error) {
	if a.Minor%100 != 0 {
		return 0, &APIError{Message: "amount must be whole naira"}
	}
	return a.Minor / 100, nil
}

type mapped struct {
	providers.VendResult
	replay   bool
	notFound bool
}

func mapPayEnvelope(raw []byte, requestID, variation string, requery bool) (mapped, error) {
	env, err := decodeEnvelope(raw)
	if err != nil {
		return mapped{}, err
	}
	tx := txnFromContent(env.Content)
	status := strings.ToLower(strings.TrimSpace(tx.Status))
	ref := firstNonEmpty(tx.TransactionID, env.RequestID, requestID)

	switch env.Code {
	case "000":
		switch status {
		case "delivered":
			token := extractToken(env.PurchasedCode, env.Token, env.TokenAlt, tx.Extras)
			units := parseUnits(env.Units, env.UnitsAlt)
			if variation == "prepaid" && token == "" {
				// Token may land on requery; do not confirm a blank STS token.
				return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
			}
			return mapped{VendResult: providers.VendResult{
				ProviderRef: ref,
				Status:      providers.VendSucceeded,
				Token:       token,
				UnitsKWh:    units,
			}}, nil
		case "pending", "initiated", "":
			return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
		default:
			// Undocumented inner status: pending, never success/failure.
			return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
		}
	case "099", "001", "089":
		return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
	case "014":
		if requery {
			return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
		}
		return mapped{replay: true}, nil
	case "015":
		return mapped{notFound: true}, nil
	case "016", "091", "010", "011", "012", "013", "017", "018", "019",
		"021", "022", "023", "024", "025", "026", "027", "028",
		"030", "031", "032", "034", "035", "040", "083", "085", "087":
		return mapped{VendResult: providers.VendResult{
			ProviderRef: ref,
			Status:      providers.VendFailed,
			FailureCode: env.Code,
		}}, nil
	default:
		// "Take any response that differs from the guidelines as pending."
		return mapped{VendResult: providers.VendResult{ProviderRef: ref, Status: providers.VendPending}}, nil
	}
}

func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", c.apiKey)
	req.Header.Set("secret-key", c.secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	observability.InjectHTTP(ctx, req.Header)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: vtpass POST %s: %v", providers.ErrAmbiguous, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: vtpass read: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: vtpass HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if res.StatusCode >= 400 {
			return nil, &APIError{HTTPStatus: res.StatusCode, Message: "empty error response"}
		}
		return nil, fmt.Errorf("%w: vtpass empty body", providers.ErrAmbiguous)
	}
	if res.StatusCode >= 400 {
		if _, decErr := decodeEnvelope(raw); decErr != nil {
			return nil, &APIError{HTTPStatus: res.StatusCode, Message: "unparseable error response"}
		}
		// JSON error body: map by `code` the same way as HTTP 200.
	}
	return raw, nil
}

type envelope struct {
	Code                string          `json:"code"`
	ResponseDescription string          `json:"response_description"`
	Message             string          `json:"message"`
	RequestID           string          `json:"requestId"`
	Content             json.RawMessage `json:"content"`
	PurchasedCode       string          `json:"purchased_code"`
	Token               string          `json:"token"`
	TokenAlt            string          `json:"Token"`
	Units               json.RawMessage `json:"units"`
	UnitsAlt            json.RawMessage `json:"Units"`
}

func decodeEnvelope(raw []byte) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return envelope{}, fmt.Errorf("%w: vtpass: unparseable response", providers.ErrAmbiguous)
	}
	env.Code = strings.TrimSpace(env.Code)
	return env, nil
}

type verifyFields struct {
	CustomerName     string `json:"Customer_Name"`
	CustomerNameAlt  string `json:"customerName"`
	Address          string `json:"Address"`
	AddressAlt       string `json:"customerAddress"`
	MeterNumber      string `json:"Meter_Number"`
	MeterType        string `json:"Meter_Type"`
	WrongBillersCode bool   `json:"WrongBillersCode"`
}

func verifyContent(raw json.RawMessage) verifyFields {
	var v verifyFields
	if len(raw) == 0 {
		return v
	}
	_ = json.Unmarshal(raw, &v)
	return v
}

type txnFields struct {
	Status        string `json:"status"`
	TransactionID string `json:"transactionId"`
	Extras        string `json:"extras"`
}

func txnFromContent(raw json.RawMessage) txnFields {
	if len(raw) == 0 {
		return txnFields{}
	}
	var wrap struct {
		Transactions txnFields `json:"transactions"`
	}
	if err := json.Unmarshal(raw, &wrap); err == nil && wrap.Transactions.Status+wrap.Transactions.TransactionID != "" {
		return wrap.Transactions
	}
	var direct txnFields
	_ = json.Unmarshal(raw, &direct)
	return direct
}

var tokenPrefix = regexp.MustCompile(`(?i)^token\s*:\s*`)

func extractToken(vals ...string) string {
	for _, s := range vals {
		s = strings.TrimSpace(s)
		if s == "" || strings.EqualFold(s, "N/A") {
			continue
		}
		s = strings.TrimSpace(tokenPrefix.ReplaceAllString(s, ""))
		s = strings.ReplaceAll(s, " ", "")
		if s == "" || strings.EqualFold(s, "N/A") {
			continue
		}
		return s
	}
	return ""
}

func parseUnits(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			s = strings.TrimSpace(s)
			s = strings.TrimSuffix(s, " kWh")
			s = strings.TrimSuffix(s, "kWh")
			s = strings.TrimSpace(s)
			if s != "" && !strings.EqualFold(s, "N/A") {
				return s
			}
			continue
		}
		var n float64
		if json.Unmarshal(raw, &n) == nil {
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
	}
	return ""
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
