package africastalking_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/notifications/africastalking/africastalkingtest"
	"github.com/teamthy/meterly/internal/providers"
)

func TestAfricasTalkingAdapterContract(t *testing.T) {
	fake := africastalkingtest.New("sandbox", "at_test_key")
	defer fake.Close()

	c, err := africastalking.New(africastalking.Config{
		Username:      "sandbox",
		APIKey:        "at_test_key",
		SenderID:      "Meterly",
		WebhookSecret: "wh_secret_test",
		BaseURL:       fake.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := notifications.SendRequest{
		DeliveryID:     uuid.New(),
		IdempotencyKey: "notif:1",
		Message: notifications.Message{
			EventID:       uuid.New(),
			TransactionID: uuid.New(),
			Channel:       notifications.SMS,
			Destination:   "+2348012345678",
			Body:          "Your payment was confirmed.",
		},
	}

	res, err := c.SendDelivery(context.Background(), req)
	if err != nil || !strings.HasPrefix(res.ProviderRef, "ATPid_") || len(fake.Sent()) != 1 {
		t.Fatalf("send: %+v %v", res, err)
	}

	// Lost response: message recorded at provider, adapter returns ErrAmbiguous.
	fake.FailAfterApply(1)
	if _, err := c.SendDelivery(context.Background(), req); !providers.IsAmbiguous(err) {
		t.Fatalf("want ErrAmbiguous on lost response, got %v", err)
	}
	if len(fake.Sent()) != 2 {
		t.Fatalf("expected fake to have applied send before 500, got %d", len(fake.Sent()))
	}

	// HTTP 429 -> RetryableError.
	fake.SetNextHTTPStatus(http.StatusTooManyRequests)
	if _, err := c.SendDelivery(context.Background(), req); err == nil {
		t.Fatal("expected retryable error on 429")
	} else if _, ok := notifications.IsRetryable(err); !ok {
		t.Fatalf("want RetryableError, got %v", err)
	}

	// Blacklisted number (406) -> PermanentError.
	fake.SetRecipientStatus("+2348099999999", 406, "UserInBlacklist")
	badReq := req
	badReq.Message.Destination = "+2348099999999"
	if _, err := c.SendDelivery(context.Background(), badReq); err == nil {
		t.Fatal("expected permanent error on 406")
	} else if code, ok := notifications.IsPermanent(err); !ok || code != "AT_406" {
		t.Fatalf("want AT_406 permanent error, got %q %v", code, err)
	}

	// Timeout -> ErrAmbiguous.
	fake.SetDelay(80 * time.Millisecond)
	tctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if _, err := c.SendDelivery(tctx, req); !providers.IsAmbiguous(err) {
		t.Fatalf("want ErrAmbiguous on timeout, got %v", err)
	}
	fake.SetDelay(0)

	// Inbound STOP webhook verification.
	form := url.Values{"from": {"+2348012345678"}, "to": {"28901"}, "text": {" stop "}}.Encode()
	hdr := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	if _, err := c.ParseInbound(hdr, []byte(form)); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("missing signature must fail: %v", err)
	}
	hdr.Set("X-AfricasTalking-Signature", africastalking.Sign("wrong", []byte(form)))
	if _, err := c.ParseInbound(hdr, []byte(form)); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("forged signature must fail: %v", err)
	}
	hdr.Set("X-AfricasTalking-Signature", africastalking.Sign("wh_secret_test", []byte(form)))
	in, err := c.ParseInbound(hdr, []byte(form))
	if err != nil || !in.Relevant || len(in.OptOuts) != 1 || in.OptOuts[0].Destination != "+2348012345678" {
		t.Fatalf("inbound STOP: %+v %v", in, err)
	}
}
