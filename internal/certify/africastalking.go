package certify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/providers"
)

type ATConfig struct {
	Username      string
	APIKey        string
	SenderID      string
	WebhookSecret string
	BaseURL       string
	To            string
	HTTPClient    *http.Client
	AllowLive     bool
}

func (c ATConfig) skipReason() string {
	if c.Username == "" || c.APIKey == "" {
		return "AFRICASTALKING_USERNAME/API_KEY not set"
	}
	if !c.AllowLive && !strings.EqualFold(c.Username, "sandbox") {
		return "refusing non-sandbox Africa's Talking username (set CERTIFY_ALLOW_LIVE=true to override)"
	}
	return ""
}

func AfricasTalking(ctx context.Context, cfg ATConfig) []Result {
	const prov = "africastalking"
	if reason := cfg.skipReason(); reason != "" {
		return []Result{
			skipped(prov, "send_sms", reason),
			skipped(prov, "callback_signature", reason),
			skipped(prov, "stop_inbound", reason),
		}
	}
	client, err := africastalking.New(africastalking.Config{
		Username: cfg.Username, APIKey: cfg.APIKey, SenderID: cfg.SenderID,
		WebhookSecret: cfg.WebhookSecret, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return []Result{
			failed(prov, "send_sms", err.Error()),
			failed(prov, "callback_signature", err.Error()),
			failed(prov, "stop_inbound", err.Error()),
		}
	}

	out := []Result{}
	to := cfg.To
	if to == "" {
		out = append(out, skipped(prov, "send_sms", "CERTIFY_AT_TO not set"))
	} else {
		out = append(out, timed(prov, "send_sms", func() ([]string, error) {
			res, err := client.SendDelivery(ctx, notifications.SendRequest{
				DeliveryID: uuid.New(),
				Message: notifications.Message{
					Channel:     notifications.SMS,
					Destination: to,
					Body:        "Meterly certify ping",
				},
			})
			if err != nil {
				return nil, err
			}
			ids := []string{}
			if res.ProviderRef != "" {
				ids = append(ids, res.ProviderRef)
			}
			return ids, nil
		}))
	}

	secret := cfg.WebhookSecret
	if secret == "" {
		out = append(out, skipped(prov, "callback_signature", "AFRICASTALKING_WEBHOOK_SECRET not set"))
		out = append(out, skipped(prov, "stop_inbound", "AFRICASTALKING_WEBHOOK_SECRET not set"))
		return out
	}

	form := url.Values{"from": {"+2348012345678"}, "to": {"28901"}, "text": {"STOP"}}.Encode()
	body := []byte(form)

	out = append(out, timed(prov, "callback_signature", func() ([]string, error) {
		h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
		h.Set("X-AfricasTalking-Signature", africastalking.Sign("wrong", body))
		if _, err := client.ParseInbound(h, body); !errors.Is(err, providers.ErrBadSignature) {
			return nil, fmt.Errorf("forged signature: %v", err)
		}
		h.Set("X-AfricasTalking-Signature", africastalking.Sign(secret, body))
		if _, err := client.ParseInbound(h, body); err != nil {
			return nil, err
		}
		return nil, nil
	}))

	out = append(out, timed(prov, "stop_inbound", func() ([]string, error) {
		h := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
		h.Set("X-AfricasTalking-Signature", africastalking.Sign(secret, body))
		in, err := client.ParseInbound(h, body)
		if err != nil {
			return nil, err
		}
		if !in.Relevant || len(in.OptOuts) != 1 {
			return nil, fmt.Errorf("STOP inbound: relevant=%v optouts=%d", in.Relevant, len(in.OptOuts))
		}
		return nil, nil
	}))
	return out
}
