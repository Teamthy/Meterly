// Package whatsapp adapts the Meta WhatsApp Cloud API to
// notifications.Provider, notifications.InboundVerifier and
// notifications.ChallengeVerifier.
//
// Official documentation:
//   - Send messages: https://developers.facebook.com/docs/whatsapp/cloud-api/reference/messages
//     (POST /{api-version}/{phone-number-id}/messages with Bearer access token)
//   - Webhooks & X-Hub-Signature-256:
//     https://developers.facebook.com/docs/graph-api/webhooks/getting-started
//   - Customer service window & template requirement (error 131047):
//     https://developers.facebook.com/docs/whatsapp/cloud-api/guides/send-messages
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
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

	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/providers"
)

const (
	DefaultBaseURL    = "https://graph.facebook.com"
	DefaultAPIVersion = "v21.0"
)

type Config struct {
	PhoneNumberID string
	AccessToken   string
	AppSecret     string // HMAC-SHA256 secret for X-Hub-Signature-256
	VerifyToken   string // GET webhook challenge token
	APIVersion    string
	BaseURL       string
	HTTPClient    *http.Client
}

type Client struct {
	phoneNumberID string
	accessToken   string
	appSecret     string
	verifyToken   string
	apiVersion    string
	base          string
	http          *http.Client
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.PhoneNumberID) == "" || strings.TrimSpace(cfg.AccessToken) == "" {
		return nil, errors.New("whatsapp: phone number id and access token are required")
	}
	ver := strings.TrimSpace(cfg.APIVersion)
	if ver == "" {
		ver = DefaultAPIVersion
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		phoneNumberID: cfg.PhoneNumberID,
		accessToken:   cfg.AccessToken,
		appSecret:     cfg.AppSecret,
		verifyToken:   cfg.VerifyToken,
		apiVersion:    ver,
		base:          base,
		http:          hc,
	}, nil
}

func (c *Client) Name() string                   { return "whatsapp" }
func (c *Client) Channel() notifications.Channel { return notifications.WhatsApp }

// Send satisfies notifications.WhatsAppSender for simple in-window callers;
// outside a 24-hour customer service window it refuses free-form text unless a
// template is supplied via SendDelivery.
func (c *Client) Send(ctx context.Context, m notifications.Message) error {
	_, err := c.SendDelivery(ctx, notifications.SendRequest{
		DeliveryID: m.EventID,
		Message:    m,
	})
	return err
}

func (c *Client) SendDelivery(ctx context.Context, req notifications.SendRequest) (notifications.SendResult, error) {
	dest, err := notifications.NormalizePhone(req.Message.Destination)
	if err != nil {
		return notifications.SendResult{}, &notifications.PermanentError{Code: "INVALID_PHONE", Message: "destination must be E.164"}
	}

	tplName := strings.TrimSpace(req.WhatsAppTemplateName)
	if tplName == "" && req.TemplateID != "" {
		tplName = notifications.WhatsAppTemplateName(req.TemplateID)
	}
	if !req.WithinCustomerServiceWindow && tplName == "" {
		return notifications.SendResult{}, &notifications.PermanentError{
			Code:    "TEMPLATE_REQUIRED_OUTSIDE_WINDOW",
			Message: "pre-approved WhatsApp template required outside 24-hour customer service window",
		}
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                strings.TrimPrefix(dest, "+"),
	}
	if tplName != "" {
		lang := languageCodeFor(req.Locale)
		tplObj := map[string]any{
			"name":     tplName,
			"language": map[string]any{"code": lang},
		}
		if len(req.TemplateParams) > 0 {
			params := make([]map[string]any, 0, len(req.TemplateParams))
			for _, p := range req.TemplateParams {
				params = append(params, map[string]any{"type": "text", "text": p})
			}
			tplObj["components"] = []map[string]any{{
				"type":       "body",
				"parameters": params,
			}}
		}
		payload["type"] = "template"
		payload["template"] = tplObj
	} else {
		if strings.TrimSpace(req.Message.Body) == "" {
			return notifications.SendResult{}, &notifications.PermanentError{Code: "EMPTY_BODY", Message: "message body is empty"}
		}
		payload["type"] = "text"
		payload["text"] = map[string]any{
			"preview_url": false,
			"body":        req.Message.Body,
		}
	}

	rawReq, err := json.Marshal(payload)
	if err != nil {
		return notifications.SendResult{}, err
	}
	endpoint := fmt.Sprintf("%s/%s/%s/messages", c.base, url.PathEscape(c.apiVersion), url.PathEscape(c.phoneNumberID))
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return notifications.SendResult{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.accessToken)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")

	res, err := c.http.Do(hreq)
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: whatsapp POST messages: %v", providers.ErrAmbiguous, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return notifications.SendResult{}, fmt.Errorf("%w: whatsapp read body: %v", providers.ErrAmbiguous, err)
	}
	if res.StatusCode >= 500 {
		return notifications.SendResult{}, fmt.Errorf("%w: whatsapp HTTP %d", providers.ErrAmbiguous, res.StatusCode)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return notifications.SendResult{}, &notifications.RetryableError{Code: "RATE_LIMITED", Message: "HTTP 429"}
	}
	if res.StatusCode >= 400 {
		var errResp struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &errResp)
		code := "HTTP_" + strconv.Itoa(res.StatusCode)
		if errResp.Error.Code != 0 {
			code = "WA_" + strconv.Itoa(errResp.Error.Code)
		}
		if errResp.Error.Code == 130429 || errResp.Error.Code == 131056 {
			return notifications.SendResult{}, &notifications.RetryableError{Code: code, Message: errResp.Error.Message}
		}
		return notifications.SendResult{}, &notifications.PermanentError{Code: code, Message: errResp.Error.Message}
	}

	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Messages) == 0 || out.Messages[0].ID == "" {
		return notifications.SendResult{}, fmt.Errorf("%w: whatsapp unparseable response", providers.ErrAmbiguous)
	}
	return notifications.SendResult{ProviderRef: out.Messages[0].ID}, nil
}

func languageCodeFor(locale string) string {
	switch notifications.NormalizeLocale(locale) {
	case notifications.LocaleYO:
		return "yo"
	case notifications.LocaleHA:
		return "ha"
	default:
		return "en_US"
	}
}

func (c *Client) VerifyChallenge(mode, verifyToken, challenge string) (string, error) {
	if c.verifyToken == "" || mode != "subscribe" || challenge == "" {
		return "", providers.ErrBadSignature
	}
	if subtle.ConstantTimeCompare([]byte(verifyToken), []byte(c.verifyToken)) != 1 {
		return "", providers.ErrBadSignature
	}
	return challenge, nil
}

// Sign computes X-Hub-Signature-256: sha256=<hex>.
func Sign(appSecret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (c *Client) ParseInbound(h http.Header, body []byte) (notifications.InboundResult, error) {
	if c.appSecret == "" {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	sigHdr := strings.TrimSpace(h.Get("X-Hub-Signature-256"))
	if !strings.HasPrefix(sigHdr, "sha256=") {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHdr, "sha256="))
	if err != nil || len(got) == 0 {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}
	m := hmac.New(sha256.New, []byte(c.appSecret))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return notifications.InboundResult{}, providers.ErrBadSignature
	}

	var payload struct {
		Object string `json:"object"`
		Entry  []struct {
			Changes []struct {
				Field string `json:"field"`
				Value struct {
					Messages []struct {
						From string `json:"from"`
						Type string `json:"type"`
						Text struct {
							Body string `json:"body"`
						} `json:"text"`
						Button struct {
							Text string `json:"text"`
						} `json:"button"`
						Interactive struct {
							ButtonReply struct {
								Title string `json:"title"`
							} `json:"button_reply"`
						} `json:"interactive"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return notifications.InboundResult{}, errors.New("whatsapp: malformed webhook JSON")
	}

	var res notifications.InboundResult
	seenInbound := map[string]bool{}
	seenOptOut := map[string]bool{}
	for _, ent := range payload.Entry {
		for _, chg := range ent.Changes {
			for _, msg := range chg.Value.Messages {
				rawFrom := strings.TrimSpace(msg.From)
				if rawFrom != "" && !strings.HasPrefix(rawFrom, "+") {
					rawFrom = "+" + rawFrom
				}
				norm, err := notifications.NormalizePhone(rawFrom)
				if err != nil {
					continue
				}
				if !seenInbound[norm] {
					seenInbound[norm] = true
					res.WhatsAppInbounds = append(res.WhatsAppInbounds, norm)
					res.Relevant = true
				}
				candidate := msg.Text.Body
				if candidate == "" {
					candidate = msg.Button.Text
				}
				if candidate == "" {
					candidate = msg.Interactive.ButtonReply.Title
				}
				if notifications.IsStopKeyword(candidate) && !seenOptOut[norm] {
					seenOptOut[norm] = true
					res.OptOuts = append(res.OptOuts, notifications.InboundOptOut{
						Destination: norm,
						Channel:     notifications.WhatsApp,
					})
				}
			}
		}
	}
	return res, nil
}

var (
	_ notifications.Provider          = (*Client)(nil)
	_ notifications.WhatsAppSender    = (*Client)(nil)
	_ notifications.InboundVerifier   = (*Client)(nil)
	_ notifications.ChallengeVerifier = (*Client)(nil)
)
