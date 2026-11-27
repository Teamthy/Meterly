// Package resend adapts the Resend email API to notifications.Provider,
// notifications.StatusChecker and notifications.InboundVerifier.
//
// Official documentation:
//   - Send email:     https://resend.com/docs/api-reference/emails/send-email
//     (POST /emails with Authorization: Bearer and Idempotency-Key)
//   - Retrieve email: https://resend.com/docs/api-reference/emails/retrieve-email
//     (GET /emails/:id)
//   - Webhooks:       https://resend.com/docs/dashboard/webhooks/verify-webhooks-requests
//     and https://docs.svix.com/receiving/verifying-payloads/how-manual
package resend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/providers"
)

const (
	DefaultBaseURL          = "https://api.resend.com"
	webhookToleranceSeconds = 300
)

type Config struct {
	APIKey        string
	From          string
	WebhookSecret string // whsec_<base64> signing secret
	BaseURL       string
	HTTPClient    *http.Client
	Now           func() time.Time
}

type Client struct {
	apiKey        string
	from          string
	webhookSecret string
	base          string
	http          *http.Client
	now           func() time.Time
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("resend: api key and sender address are required")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Client{
		apiKey:        cfg.APIKey,
		from:          cfg.From,
		webhookSecret: cfg.WebhookSecret,
		base:          base,
		http:          hc,
		now:           nowFn,
	}, nil
}

func (c *Client) Name() string                   { return "resend" }
func (c *Client) Channel() notifications.Channel { return notifications.Email }

func (c *Client) Send(ctx context.Context, m notifications.Message) error {
	var idem string
	if m.EventID.String() != "00000000-0000-0000-0000-000000000000" {
		idem = "notif:" + m.EventID.String()
	}
	_, err := c.SendDelivery(ctx, notifications.SendRequest{
		DeliveryID:     m.EventID,
		IdempotencyKey: idem,
		Message:        m,
	})
	return err
}

func (c *Client) SendDelivery(ctx context.Context, req notifications.SendRequest) (notifications.SendResult, error) {
	dest, err := notifications.NormalizeEmail(req.Message.Destination)
	if err != nil {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "INVALID_EMAIL", Message: "destination must be a valid email"}
	}
	subj := strings.TrimSpace(req.Message.Subject)
	if subj == "" {
		subj = "Meterly notification"
	}
	if strings.TrimSpace(req.Message.Body) == "" {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "EMPTY_BODY", Message: "email body is empty"}
	}

	payload := map[string]any{
		"from":    c.from,
		"to":      []string{dest},
		"subject": subj,
		"text":    req.Message.Body,
	}
	rawReq, err := json.Marshal(payload)
	if err != nil {
		return notifications.SendResult{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/emails", bytes.NewReader(rawReq))
	if err != nil {
		return notifications.SendResult{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if req.IdempotencyKey != "" {
		hreq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	observability.InjectHTTP(ctx, hreq.Header)

	res, err := c.http.Do(hreq)
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: resend POST /emails: %v", providers.ErrAmbiguous, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: resend read body: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode >= 500 {
		return notifications.SendResult{}, fmt.Errorf("%w: resend HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return notifications.SendResult{}, &notifications.RetryableError{Code: "RATE_LIMITED", Message: "HTTP 429"}
	}
	if res.StatusCode >= 400 {
		var errBody struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &errBody)
		code := errBody.Name
		if code == "" {
			code = "HTTP_" + strconv.Itoa(res.StatusCode)
		}
		return notifications.SendResult{}, &notifications.PermanentError{Code: code, Message: errBody.Message}
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return notifications.SendResult{}, fmt.Errorf("%w: resend unparseable response", providers.ErrAmbiguous)
	}
	return notifications.SendResult{ProviderRef: out.ID}, nil
}

func (c *Client) CheckDeliveryStatus(ctx context.Context, _ string, providerRef string) (notifications.ProviderStatus, string, error) {
	if strings.TrimSpace(providerRef) == "" {
		return notifications.ProviderStatusPending, "", nil
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/emails/"+url.PathEscape(providerRef), nil)
	if err != nil {
		return "", "", err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	hreq.Header.Set("Accept", "application/json")
	observability.InjectHTTP(ctx, hreq.Header)

	res, err := c.http.Do(hreq)
	if err != nil {
		return "", "", fmt.Errorf("%w: resend GET /emails: %v", providers.ErrAmbiguous, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("%w: resend read status: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode == http.StatusNotFound {
		return notifications.ProviderStatusNotFound, "", nil
	}
	if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
		return "", "", fmt.Errorf("%w: resend status HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	if res.StatusCode >= 400 {
		return "", "", fmt.Errorf("resend status HTTP %d", res.StatusCode)
	}
	var data struct {
		ID        string `json:"id"`
		LastEvent string `json:"last_event"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.ID == "" {
		return "", "", fmt.Errorf("%w: resend unparseable status", providers.ErrAmbiguous)
	}
	switch strings.ToLower(data.LastEvent) {
	case "sent", "delivered", "opened", "clicked":
		return notifications.ProviderStatusSent, "", nil
	case "bounced", "failed", "complained", "suppressed":
		return notifications.ProviderStatusFailed, strings.ToUpper(data.LastEvent), nil
	default:
		return notifications.ProviderStatusPending, "", nil
	}
}

// SignWebhook computes the Svix v1 signature header value for tests and local tooling.
func SignWebhook(secret, msgID, timestamp string, body []byte) (string, error) {
	rawKey, err := decodeWebhookSecret(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeWebhookSecret(secret string) ([]byte, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(secret), "whsec_")
	if trimmed == "" {
		return nil, errors.New("empty secret")
	}
	return base64.StdEncoding.DecodeString(trimmed)
}

func (c *Client) ParseInbound(h http.Header, body []byte) (notifications.InboundResult, error) {
	rawKey, err := decodeWebhookSecret(c.webhookSecret)
	if err != nil {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	msgID := strings.TrimSpace(h.Get("svix-id"))
	tsStr := strings.TrimSpace(h.Get("svix-timestamp"))
	sigHdr := strings.TrimSpace(h.Get("svix-signature"))
	if msgID == "" || tsStr == "" || sigHdr == "" {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	diff := c.now().Unix() - ts
	if diff > webhookToleranceSeconds || diff < -webhookToleranceSeconds {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}

	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte(msgID + "." + tsStr + "."))
	mac.Write(body)
	want := mac.Sum(nil)

	matched := false
	for _, part := range strings.Fields(sigHdr) {
		verAndSig := strings.SplitN(part, ",", 2)
		if len(verAndSig) != 2 || verAndSig[0] != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(verAndSig[1])
		if err == nil && hmac.Equal(got, want) {
			matched = true
			break
		}
	}
	if !matched {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}

	var evt struct {
		Type string `json:"type"`
		Data struct {
			From    string   `json:"from"`
			To      []string `json:"to"`
			Subject string   `json:"subject"`
			Text    string   `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.Type == "" {
		return notifications.InboundResult{}, errors.New("resend: malformed webhook JSON")
	}

	switch evt.Type {
	case "email.complained":
		var opts []notifications.InboundOptOut
		for _, addr := range evt.Data.To {
			if norm, err := notifications.NormalizeEmail(extractEmailAddress(addr)); err == nil {
				opts = append(opts, notifications.InboundOptOut{Destination: norm, Channel: notifications.Email})
			}
		}
		return notifications.InboundResult{Relevant: len(opts) > 0, OptOuts: opts}, nil
	case "email.received":
		if !notifications.IsStopKeyword(evt.Data.Subject) && !notifications.IsStopKeyword(evt.Data.Text) {
			return notifications.InboundResult{Relevant: false}, nil
		}
		norm, err := notifications.NormalizeEmail(extractEmailAddress(evt.Data.From))
		if err != nil {
			return notifications.InboundResult{}, errors.New("resend: invalid sender email")
		}
		return notifications.InboundResult{
			Relevant: true,
			OptOuts:  []notifications.InboundOptOut{{Destination: norm, Channel: notifications.Email}},
		}, nil
	default:
		return notifications.InboundResult{Relevant: false}, nil
	}
}

func extractEmailAddress(s string) string {
	s = strings.TrimSpace(s)
	if start := strings.LastIndex(s, "<"); start >= 0 && strings.HasSuffix(s, ">") {
		return strings.TrimSpace(s[start+1 : len(s)-1])
	}
	return s
}

var (
	_ notifications.Provider        = (*Client)(nil)
	_ notifications.EmailSender     = (*Client)(nil)
	_ notifications.StatusChecker   = (*Client)(nil)
	_ notifications.InboundVerifier = (*Client)(nil)
)
