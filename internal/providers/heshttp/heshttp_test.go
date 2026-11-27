package heshttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/heshttp"
	"github.com/teamthy/meterly/internal/providers/heshttp/hestest"
)

func TestHESHTTPCreditAndWebhook(t *testing.T) {
	const secret = "test-hes-hmac-secret"
	fake := hestest.New(secret)
	defer fake.Close()
	c, err := heshttp.New(heshttp.Config{BaseURL: fake.URL, HMACSecret: secret, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	el, reach, err := c.CheckEligibility(ctx, "70011112222")
	if err != nil || !el || reach == nil || !*reach {
		t.Fatalf("eligible 7…: %v %v %v", el, reach, err)
	}
	el, _, err = c.CheckEligibility(ctx, "45011112222")
	if err != nil || el {
		t.Fatalf("keypad must not be eligible without Allow: %v %v", el, err)
	}

	req := providers.RemoteCreditRequest{
		TransactionID: "txn-1", MeterNumber: "70011112222", Token: "123456789012345678901234",
		Amount: money.New(500000, money.NGN), IdempotencyKey: "credit:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	res, err := c.InitiateCredit(ctx, req)
	if err != nil || res.Status != providers.CreditAccepted || res.ProviderRef == "" {
		t.Fatalf("initiate: %+v %v", res, err)
	}
	again, err := c.InitiateCredit(ctx, req)
	if err != nil || again.ProviderRef != res.ProviderRef || again.Status != providers.CreditAccepted {
		t.Fatalf("idempotent initiate: %+v %v", again, err)
	}

	fake.Settle()
	got, err := c.GetCreditStatus(ctx, req.IdempotencyKey)
	if err != nil || got.Status != providers.CreditConfirmed {
		t.Fatalf("after settle: %+v %v", got, err)
	}

	body, _ := json.Marshal(map[string]string{"idempotency_key": req.IdempotencyKey})
	hdr := http.Header{}
	hdr.Set(heshttp.SignatureHeader, heshttp.Sign(secret, body))
	wh, err := c.ParseWebhook(hdr, body)
	if err != nil || !wh.Relevant || wh.IdempotencyKey != req.IdempotencyKey {
		t.Fatalf("webhook: %+v %v", wh, err)
	}
	hdr.Set(heshttp.SignatureHeader, "deadbeef")
	if _, err := c.ParseWebhook(hdr, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("forged webhook: %v", err)
	}

	missing, err := c.GetCreditStatus(ctx, "credit:00000000-0000-0000-0000-000000000000")
	if err != nil || missing.Status != providers.CreditNotFound {
		t.Fatalf("missing: %+v %v", missing, err)
	}
}

func TestHESHTTPAmbiguousOnLostAck(t *testing.T) {
	const secret = "test-hes-hmac-secret"
	fake := hestest.New(secret)
	defer fake.Close()
	c, err := heshttp.New(heshttp.Config{BaseURL: fake.URL, HMACSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.InitiateCredit(context.Background(), providers.RemoteCreditRequest{
		TransactionID: "txn-77", MeterNumber: "77011112222", Token: "tok",
		Amount: money.New(100000, money.NGN), IdempotencyKey: "credit:bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee",
	})
	if !providers.IsAmbiguous(err) {
		t.Fatalf("77… must be ambiguous after apply: %v", err)
	}
	got, err := c.GetCreditStatus(context.Background(), "credit:bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil || got.Status != providers.CreditConfirmed {
		t.Fatalf("requery after lost ack: %+v %v", got, err)
	}
}
