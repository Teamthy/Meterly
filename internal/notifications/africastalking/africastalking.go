// Package africastalking adapts Africa's Talking SMS API to
// notifications.Provider and notifications.InboundVerifier.
//
// Official documentation:
//   - Outbound SMS: https://developers.africastalking.com/docs/sms/sending/bulk
//     (POST /version1/messaging, application/x-www-form-urlencoded, apiKey header)
//   - Inbound SMS callbacks: https://developers.africastalking.com/docs/sms/incoming_messages
//
// Error semantics: network errors, timeouts, HTTP 5xx, unparseable 2xx bodies
// and gateway 5xx recipient status codes return providers.ErrAmbiguous so the
// caller marks the delivery UNKNOWN and never auto-resends a token SMS.
package africastalking

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/providers"
)

const (
	SandboxBaseURL    = "https://api.sandbox.africastalking.com"
	ProductionBaseURL = "https://api.africastalking.com"
)

type Config struct {
	Username      string
	APIKey        string
	SenderID      string // optional registered shortcode or alphanumeric sender ID
	WebhookSecret string // required to verify inbound callbacks (X-AfricasTalking-Signature)
	BaseURL       string
	HTTPClient    *http.Client
}

type Client struct {
	username      string
	apiKey        string
	senderID      string
	webhookSecret string
	base          string
	http          *http.Client
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Username) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("africastalking: username and api key are required")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		if cfg.Username == "sandbox" {
			base = SandboxBaseURL
		} else {
			base = ProductionBaseURL
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		username:      cfg.Username,
		apiKey:        cfg.APIKey,
		senderID:      cfg.SenderID,
		webhookSecret: cfg.WebhookSecret,
		base:          base,
		http:          hc,
	}, nil
}

func (c *Client) Name() string                   { return "africastalking" }
func (c *Client) Channel() notifications.Channel { return notifications.SMS }

func (c *Client) Send(ctx context.Context, m notifications.Message) error {
	_, err := c.SendDelivery(ctx, notifications.SendRequest{
		DeliveryID: m.EventID,
		Message:    m,
	})
	return err
}

type sendResponse struct {
	SMSMessageData struct {
		Message    string `json:"Message"`
		Recipients []struct {
			StatusCode int    `json:"statusCode"`
			Number     string `json:"number"`
			Status     string `json:"status"`
			Cost       string `json:"cost"`
			MessageID  string `json:"messageId"`
		} `json:"Recipients"`
	} `json:"SMSMessageData"`
}

func (c *Client) SendDelivery(ctx context.Context, req notifications.SendRequest) (notifications.SendResult, error) {
	dest, err := notifications.NormalizePhone(req.Message.Destination)
	if err != nil {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "INVALID_PHONE", Message: "destination must be E.164"}
	}
	if strings.TrimSpace(req.Message.Body) == "" {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "EMPTY_BODY", Message: "message body is empty"}
	}

	form := url.Values{}
	form.Set("username", c.username)
	form.Set("to", dest)
	form.Set("message", req.Message.Body)
	form.Set("enqueue", "1")
	if c.senderID != "" {
		form.Set("from", c.senderID)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/version1/messaging", strings.NewReader(form.Encode()))
	if err != nil {
		return notifications.SendResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("apiKey", c.apiKey)
	observability.InjectHTTP(ctx, hreq.Header)

	res, err := c.http.Do(hreq)
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: africastalking POST /version1/messaging: %v", providers.ErrAmbiguous, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: africastalking read body: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode >= 500 {
		return notifications.SendResult{}, fmt.Errorf("%w: africastalking HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return notifications.SendResult{}, &notifications.RetryableError{Code: "RATE_LIMITED", Message: "HTTP 429"}
	}
	if res.StatusCode >= 400 {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "HTTP_" + strconv.Itoa(res.StatusCode), Message: "rejected by Africa's Talking"}
	}

	var parsed sendResponse
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.SMSMessageData.Recipients) == 0 {
		return notifications.SendResult{}, fmt.Errorf("%w: africastalking unparseable response", providers.ErrAmbiguous)
	}
	rcpt := parsed.SMSMessageData.Recipients[0]
	switch rcpt.StatusCode {
	case 100, 101, 102: // Processed, Sent ("Success"), Queued
		ref := rcpt.MessageID
		if ref == "" && req.DeliveryID != uuid.Nil {
			ref = "at:" + req.DeliveryID.String()
		}
		return notifications.SendResult{ProviderRef: ref}, nil
	case 405: // InsufficientBalance
		return notifications.SendResult{}, &notifications.RetryableError{Code: "INSUFFICIENT_BALANCE", Message: rcpt.Status}
	case 401, 402, 403, 404, 406, 407: // RiskHold, InvalidSenderId, InvalidPhoneNumber, UnsupportedNumberType, UserInBlacklist, CouldNotRoute
		return notifications.SendResult{}, &notifications.PermanentError{Code: "AT_" + strconv.Itoa(rcpt.StatusCode), Message: rcpt.Status}
	default:
		if rcpt.StatusCode >= 500 {
			return notifications.SendResult{}, fmt.Errorf("%w: africastalking recipient status %d", providers.ErrAmbiguous, rcpt.StatusCode)
		}
		if strings.EqualFold(rcpt.Status, "Success") || strings.EqualFold(rcpt.Status, "Sent") {
			return notifications.SendResult{ProviderRef: rcpt.MessageID}, nil
		}
		return notifications.SendResult{}, fmt.Errorf("%w: africastalking unknown recipient status %d", providers.ErrAmbiguous, rcpt.StatusCode)
	}
}

// Sign computes the HMAC-SHA256 signature expected in X-AfricasTalking-Signature.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (c *Client) ParseInbound(h http.Header, body []byte) (notifications.InboundResult, error) {
	if c.webhookSecret == "" {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	sigHdr := strings.TrimSpace(h.Get("X-AfricasTalking-Signature"))
	sigHex := strings.TrimPrefix(sigHdr, "sha256=")
	got, err := hex.DecodeString(sigHex)
	if err != nil || len(got) == 0 {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	m := hmac.New(sha256.New, []byte(c.webhookSecret))
	m.Write(body)
	want := m.Sum(nil)
	if !hmac.Equal(got, want) {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}

	var from, text string
	ct := strings.ToLower(h.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		var payload struct {
			From string `json:"from"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return notifications.InboundResult{}, errors.New("africastalking: malformed JSON callback")
		}
		from, text = payload.From, payload.Text
	} else {
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return notifications.InboundResult{}, errors.New("africastalking: malformed form callback")
		}
		from, text = vals.Get("from"), vals.Get("text")
	}
	if from == "" {
		return notifications.InboundResult{}, errors.New("africastalking: missing sender phone")
	}
	if !notifications.IsStopKeyword(text) {
		return notifications.InboundResult{Relevant: false}, nil
	}
	norm, err := notifications.NormalizePhone(from)
	if err != nil {
		return notifications.InboundResult{}, errors.New("africastalking: invalid sender phone")
	}
	return notifications.InboundResult{
		Relevant: true,
		OptOuts:  []notifications.InboundOptOut{{Destination: norm, Channel: notifications.SMS}},
	}, nil
}

var (
	_ notifications.Provider        = (*Client)(nil)
	_ notifications.SMSSender       = (*Client)(nil)
	_ notifications.InboundVerifier = (*Client)(nil)
)
