package resend_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/resend"
	"github.com/teamthy/meterly/internal/notifications/resend/resendtest"
	"github.com/teamthy/meterly/internal/providers"
)

func TestResendAdapterContract(t *testing.T) {
	fake := resendtest.New("re_test_123456")
	defer fake.Close()

	whSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	fixedNow := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	c, err := resend.New(resend.Config{
		APIKey:        "re_test_123456",
		From:          "Meterly <noreply@meterly.ng>",
		WebhookSecret: whSecret,
		BaseURL:       fake.URL,
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	req := notifications.SendRequest{
		DeliveryID:     uuid.New(),
		IdempotencyKey: "notif:email-1",
		Message: notifications.Message{
			EventID:       uuid.New(),
			TransactionID: uuid.New(),
			Channel:       notifications.Email,
			Destination:   "ada@example.com",
			Subject:       "Payment confirmed",
			Body:          "Your payment was confirmed.",
		},
	}

	// Lost response on first send, then idempotent retry yields only 1 email at Resend.
	fake.FailAfterApply(1)
	if _, err := c.SendDelivery(context.Background(), req); !providers.IsAmbiguous(err) {
		t.Fatalf("want ErrAmbiguous, got %v", err)
	}
	res, err := c.SendDelivery(context.Background(), req)
	if err != nil || res.ProviderRef == "" || len(fake.Sent()) != 1 {
		t.Fatalf("idempotent retry: %+v %v sent=%d", res, err, len(fake.Sent()))
	}

	st, _, err := c.CheckDeliveryStatus(context.Background(), req.IdempotencyKey, res.ProviderRef)
	if err != nil || st != notifications.ProviderStatusSent {
		t.Fatalf("status check: %s %v", st, err)
	}
	fake.SetLastEvent(res.ProviderRef, "bounced")
	st, code, err := c.CheckDeliveryStatus(context.Background(), req.IdempotencyKey, res.ProviderRef)
	if err != nil || st != notifications.ProviderStatusFailed || code != "BOUNCED" {
		t.Fatalf("bounced status: %s %q %v", st, code, err)
	}

	// Svix webhook verification: bad signature, stale timestamp, and valid STOP/complaint.
	body, _ := json.Marshal(map[string]any{
		"type": "email.received",
		"data": map[string]any{"from": "Ada <ada@example.com>", "subject": "STOP", "text": ""},
	})
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	sig, err := resend.SignWebhook(whSecret, "msg_1", ts, body)
	if err != nil {
		t.Fatal(err)
	}

	hdr := http.Header{}
	hdr.Set("svix-id", "msg_1")
	hdr.Set("svix-timestamp", ts)
	hdr.Set("svix-signature", "v1,badbase64==")
	if _, err := c.ParseInbound(hdr, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("forged svix sig: %v", err)
	}

	staleTS := strconv.FormatInt(fixedNow.Add(-10*time.Minute).Unix(), 10)
	staleSig, _ := resend.SignWebhook(whSecret, "msg_1", staleTS, body)
	hdr.Set("svix-timestamp", staleTS)
	hdr.Set("svix-signature", staleSig)
	if _, err := c.ParseInbound(hdr, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("stale svix timestamp must fail: %v", err)
	}

	hdr.Set("svix-timestamp", ts)
	hdr.Set("svix-signature", sig)
	in, err := c.ParseInbound(hdr, body)
	if err != nil || !in.Relevant || len(in.OptOuts) != 1 || in.OptOuts[0].Destination != "ada@example.com" {
		t.Fatalf("valid inbound STOP: %+v %v", in, err)
	}
}
