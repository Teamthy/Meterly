package certify

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/providers"
)

type WhatsAppConfig struct {
	PhoneNumberID string
	AccessToken   string
	AppSecret     string
	VerifyToken   string
	APIVersion    string
	BaseURL       string
	To            string
	Template      string
	HTTPClient    *http.Client
}

func (c WhatsAppConfig) skipReason() string {
	if c.PhoneNumberID == "" || c.AccessToken == "" {
		return "WHATSAPP_PHONE_NUMBER_ID/ACCESS_TOKEN not set"
	}
	return ""
}

func WhatsApp(ctx context.Context, cfg WhatsAppConfig) []Result {
	const prov = "whatsapp"
	if reason := cfg.skipReason(); reason != "" {
		return []Result{
			skipped(prov, "template_send", reason),
			skipped(prov, "verify_token_handshake", reason),
			skipped(prov, "xhub_signature", reason),
		}
	}
	client, err := whatsapp.New(whatsapp.Config{
		PhoneNumberID: cfg.PhoneNumberID, AccessToken: cfg.AccessToken,
		AppSecret: cfg.AppSecret, VerifyToken: cfg.VerifyToken,
		APIVersion: cfg.APIVersion, BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return []Result{
			failed(prov, "template_send", err.Error()),
			failed(prov, "verify_token_handshake", err.Error()),
			failed(prov, "xhub_signature", err.Error()),
		}
	}

	out := []Result{}
	if cfg.To == "" {
		out = append(out, skipped(prov, "template_send", "CERTIFY_WHATSAPP_TO not set"))
	} else {
		tpl := cfg.Template
		if tpl == "" {
			tpl = "meterly_offline_token_v1"
		}
		out = append(out, timed(prov, "template_send", func() ([]string, error) {
			res, err := client.SendDelivery(ctx, notifications.SendRequest{
				DeliveryID:           uuid.New(),
				WhatsAppTemplateName: tpl,
				TemplateParams:       []string{"certify"},
				Message: notifications.Message{
					Channel:     notifications.WhatsApp,
					Destination: cfg.To,
					Body:        "certify",
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

	if cfg.VerifyToken == "" {
		out = append(out, skipped(prov, "verify_token_handshake", "WHATSAPP_VERIFY_TOKEN not set"))
	} else {
		out = append(out, timed(prov, "verify_token_handshake", func() ([]string, error) {
			got, err := client.VerifyChallenge("subscribe", cfg.VerifyToken, "challenge-token")
			if err != nil || got != "challenge-token" {
				return nil, fmt.Errorf("handshake: got %q err=%v", got, err)
			}
			if _, err := client.VerifyChallenge("subscribe", "wrong", "challenge-token"); !errors.Is(err, providers.ErrBadSignature) {
				return nil, fmt.Errorf("wrong verify token: %v", err)
			}
			return nil, nil
		}))
	}

	if cfg.AppSecret == "" {
		out = append(out, skipped(prov, "xhub_signature", "WHATSAPP_APP_SECRET not set"))
		return out
	}
	out = append(out, timed(prov, "xhub_signature", func() ([]string, error) {
		body := []byte(`{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{"messages":[{"from":"2348012345678","type":"text","text":{"body":"hi"}}]}}]}]}`)
		h := http.Header{}
		h.Set("X-Hub-Signature-256", whatsapp.Sign("wrong", body))
		if _, err := client.ParseInbound(h, body); !errors.Is(err, providers.ErrBadSignature) {
			return nil, fmt.Errorf("forged signature: %v", err)
		}
		h.Set("X-Hub-Signature-256", whatsapp.Sign(cfg.AppSecret, body))
		if _, err := client.ParseInbound(h, body); err != nil {
			return nil, err
		}
		return nil, nil
	}))
	return out
}
