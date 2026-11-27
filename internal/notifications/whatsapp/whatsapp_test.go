package whatsapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/notifications/whatsapp/whatsapptest"
	"github.com/teamthy/meterly/internal/providers"
)

func TestWhatsAppAdapterContract(t *testing.T) {
	fake := whatsapptest.New("109876543210", "wa_token_test")
	defer fake.Close()

	c, err := whatsapp.New(whatsapp.Config{
		PhoneNumberID: "109876543210",
		AccessToken:   "wa_token_test",
		AppSecret:     "wa_app_secret",
		VerifyToken:   "wa_verify_tok",
		BaseURL:       fake.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Free-form text outside the 24-hour customer service window is rejected locally.
	freeForm := notifications.SendRequest{
		DeliveryID: uuid.New(),
		Message: notifications.Message{
			Destination: "+2348012345678",
			Body:        "Free form text without template",
		},
		WithinCustomerServiceWindow: false,
	}
	if _, err := c.SendDelivery(context.Background(), freeForm); err == nil {
		t.Fatal("expected permanent error for free-form WhatsApp message outside 24h window")
	} else if code, ok := notifications.IsPermanent(err); !ok || code != "TEMPLATE_REQUIRED_OUTSIDE_WINDOW" {
		t.Fatalf("got %q %v", code, err)
	}

	// Even if client claims WithinCustomerServiceWindow=true when provider has no open window, Meta returns 131047 -> PermanentError.
	freeForm.WithinCustomerServiceWindow = true
	if _, err := c.SendDelivery(context.Background(), freeForm); err == nil {
		t.Fatal("expected Meta 131047 rejection when provider window is closed")
	} else if code, ok := notifications.IsPermanent(err); !ok || code != "WA_131047" {
		t.Fatalf("want WA_131047, got %q %v", code, err)
	}

	// Pre-approved template succeeds outside 24h window.
	tplReq := notifications.SendRequest{
		DeliveryID: uuid.New(),
		TemplateID: notifications.TemplatePaymentConfirmed,
		Locale:     notifications.LocaleEN,
		Message: notifications.Message{
			Destination: "+2348012345678",
			Body:        "Your payment was confirmed.",
		},
	}
	res, err := c.SendDelivery(context.Background(), tplReq)
	if err != nil || res.ProviderRef == "" || len(fake.Sent()) != 1 || fake.Sent()[0].Type != "template" {
		t.Fatalf("template send: %+v %v sent=%+v", res, err, fake.Sent())
	}

	// Inside an open 24h window, free-form text also succeeds.
	fake.OpenCustomerServiceWindow("+2348012345678", 24*time.Hour)
	if _, err := c.SendDelivery(context.Background(), freeForm); err != nil || len(fake.Sent()) != 2 || fake.Sent()[1].Type != "text" {
		t.Fatalf("in-window text send: %v sent=%+v", err, fake.Sent())
	}

	// Lost response -> ErrAmbiguous.
	fake.FailAfterApply(1)
	if _, err := c.SendDelivery(context.Background(), tplReq); !providers.IsAmbiguous(err) {
		t.Fatalf("want ErrAmbiguous, got %v", err)
	}

	// Challenge handshake & inbound STOP + window recording.
	if ch, err := c.VerifyChallenge("subscribe", "wa_verify_tok", "12345"); err != nil || ch != "12345" {
		t.Fatalf("challenge: %q %v", ch, err)
	}
	if _, err := c.VerifyChallenge("subscribe", "wrong", "12345"); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("wrong verify_token must fail: %v", err)
	}

	inboundBody, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []map[string]any{{
			"changes": []map[string]any{{
				"field": "messages",
				"value": map[string]any{
					"messages": []map[string]any{{
						"from": "2348012345678",
						"type": "text",
						"text": map[string]any{"body": "STOP"},
					}},
				},
			}},
		}},
	})
	hdr := http.Header{}
	hdr.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if _, err := c.ParseInbound(hdr, inboundBody); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("forged X-Hub-Signature-256: %v", err)
	}
	hdr.Set("X-Hub-Signature-256", whatsapp.Sign("wa_app_secret", inboundBody))
	in, err := c.ParseInbound(hdr, inboundBody)
	if err != nil || !in.Relevant || len(in.OptOuts) != 1 || in.OptOuts[0].Destination != "+2348012345678" || len(in.WhatsAppInbounds) != 1 {
		t.Fatalf("valid inbound: %+v %v", in, err)
	}
}
