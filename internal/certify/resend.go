package certify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/resend"
	"github.com/teamthy/meterly/internal/providers"
)

type ResendConfig struct {
	APIKey        string
	From          string
	WebhookSecret string
	BaseURL       string
	To            string
	HTTPClient    *http.Client
	Now           func() time.Time
}

func (c ResendConfig) skipReason() string {
	if c.APIKey == "" || c.From == "" {
		return "RESEND_API_KEY/RESEND_FROM not set"
	}
	return ""
}

func Resend(ctx context.Context, cfg ResendConfig) []Result {
	const prov = "resend"
	if reason := cfg.skipReason(); reason != "" {
		return []Result{
			skipped(prov, "send_email", reason),
			skipped(prov, "webhook_signature", reason),
		}
	}
	client, err := resend.New(resend.Config{
		APIKey: cfg.APIKey, From: cfg.From, WebhookSecret: cfg.WebhookSecret,
		BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Now: cfg.Now,
	})
	if err != nil {
		return []Result{
			failed(prov, "send_email", err.Error()),
			failed(prov, "webhook_signature", err.Error()),
		}
	}

	out := []Result{}
	to := cfg.To
	if to == "" {
		to = cfg.From // documented: send to a mailbox you control
	}
	out = append(out, timed(prov, "send_email", func() ([]string, error) {
		res, err := client.SendDelivery(ctx, notifications.SendRequest{
			DeliveryID:     uuid.New(),
			IdempotencyKey: "certify-" + newRef("mail"),
			Message: notifications.Message{
				Channel:     notifications.Email,
				Destination: to,
				Subject:     "Meterly certify ping",
				Body:        "Meterly certify ping. No tokens.",
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

	if cfg.WebhookSecret == "" {
		out = append(out, skipped(prov, "webhook_signature", "RESEND_WEBHOOK_SECRET not set"))
		return out
	}

	out = append(out, timed(prov, "webhook_signature", func() ([]string, error) {
		body, _ := json.Marshal(map[string]any{
			"type": "email.complained",
			"data": map[string]any{"to": []string{"ada@example.com"}},
		})
		now := time.Now()
		if cfg.Now != nil {
			now = cfg.Now()
		}
		ts := strconv.FormatInt(now.Unix(), 10)
		msgID := "certify-msg-1"
		sig, err := resend.SignWebhook(cfg.WebhookSecret, msgID, ts, body)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set("svix-id", msgID)
		h.Set("svix-timestamp", ts)
		h.Set("svix-signature", sig)
		in, err := client.ParseInbound(h, body)
		if err != nil || !in.Relevant {
			return nil, fmt.Errorf("valid signature: relevant=%v err=%v", in.Relevant, err)
		}
		h.Set("svix-signature", "v1,AAAA")
		if _, err := client.ParseInbound(h, body); !errors.Is(err, providers.ErrBadSignature) {
			return nil, fmt.Errorf("forged signature: %v", err)
		}
		return []string{msgID}, nil
	}))
	return out
}
