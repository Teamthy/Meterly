package paystack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
)

const secret = "sk_test_fake_secret"

func setup(t *testing.T) (*paystack.Client, *paystacktest.Fake) {
	t.Helper()
	f := paystacktest.New(secret)
	t.Cleanup(f.Close)
	c, err := paystack.New(paystack.Config{SecretKey: secret, BaseURL: f.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

var ctx = context.Background()

func ngn(v int64) money.Amount { return money.New(v, money.NGN) }

func TestInitializeVerifyPay(t *testing.T) {
	c, f := setup(t)
	res, err := c.InitializePayment(ctx, providers.PaymentInit{Reference: "txn-1", Amount: ngn(510000), Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.ProviderRef, "txn-1.") || res.CheckoutURL == "" {
		t.Fatalf("got %+v", res)
	}
	v, err := c.VerifyPayment(ctx, res.ProviderRef)
	if err != nil || v.Status != providers.PaymentPending {
		t.Fatalf("before paying: %+v %v", v, err)
	}
	f.Pay(res.ProviderRef, 0)
	v, err = c.VerifyPayment(ctx, res.ProviderRef)
	if err != nil || v.Status != providers.PaymentSucceeded || v.Amount != ngn(510000) || v.PaidAt == nil {
		t.Fatalf("after paying: %+v %v", v, err)
	}
}

func TestRetriedInitializeUsesFreshReference(t *testing.T) {
	c, f := setup(t)
	a, err := c.InitializePayment(ctx, providers.PaymentInit{Reference: "txn-1", Amount: ngn(100), Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.InitializePayment(ctx, providers.PaymentInit{Reference: "txn-1", Amount: ngn(100), Email: "a@example.com"})
	if err != nil {
		t.Fatalf("a retry must not hit Paystack's duplicate-reference error: %v", err)
	}
	if a.ProviderRef == b.ProviderRef || len(f.References()) != 2 {
		t.Fatalf("refs %s %s", a.ProviderRef, b.ProviderRef)
	}
}

func TestStatusMapping(t *testing.T) {
	c, f := setup(t)
	res, _ := c.InitializePayment(ctx, providers.PaymentInit{Reference: "t", Amount: ngn(100), Email: "a@example.com"})
	for status, want := range map[string]providers.PaymentStatus{
		"success": providers.PaymentSucceeded, "failed": providers.PaymentFailed, "reversed": providers.PaymentFailed,
		"abandoned": providers.PaymentPending, "ongoing": providers.PaymentPending, "processing": providers.PaymentPending,
		"queued": providers.PaymentPending, "something_new": providers.PaymentPending,
	} {
		f.SetStatus(res.ProviderRef, status)
		v, err := c.VerifyPayment(ctx, res.ProviderRef)
		if err != nil || v.Status != want {
			t.Errorf("%s: got %s (%v), want %s", status, v.Status, err, want)
		}
	}
}

func TestErrorsAreClassified(t *testing.T) {
	c, f := setup(t)
	// 4xx: definitive.
	_, err := c.VerifyPayment(ctx, "missing")
	var apiErr *paystack.APIError
	if !errors.As(err, &apiErr) || providers.IsAmbiguous(err) || apiErr.HTTPStatus != 400 {
		t.Fatalf("missing reference: %v", err)
	}
	// 5xx after the change was applied: ambiguous.
	f.FailAfterApply("POST /transaction/initialize", 1)
	_, err = c.InitializePayment(ctx, providers.PaymentInit{Reference: "t", Amount: ngn(100), Email: "a@example.com"})
	if !providers.IsAmbiguous(err) {
		t.Fatalf("5xx must be ambiguous: %v", err)
	}
	// Wrong key: definitive.
	bad, _ := paystack.New(paystack.Config{SecretKey: "sk_test_wrong", BaseURL: f.URL})
	if _, err := bad.VerifyPayment(ctx, "x"); providers.IsAmbiguous(err) || err == nil {
		t.Fatalf("auth failure: %v", err)
	}
	// Missing email is rejected locally.
	if _, err := c.InitializePayment(ctx, providers.PaymentInit{Reference: "t", Amount: ngn(100)}); err == nil {
		t.Fatal("want email error")
	}
}

func TestTimeoutIsAmbiguous(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer slow.Close()
	c, _ := paystack.New(paystack.Config{SecretKey: secret, BaseURL: slow.URL})
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.VerifyPayment(tctx, "x"); !providers.IsAmbiguous(err) {
		t.Fatalf("got %v", err)
	}
}

func TestRefundIsIdempotent(t *testing.T) {
	c, f := setup(t)
	res, _ := c.InitializePayment(ctx, providers.PaymentInit{Reference: "t", Amount: ngn(510000), Email: "a@example.com"})
	f.Pay(res.ProviderRef, 0)
	req := providers.RefundRequest{ProviderRef: res.ProviderRef, Amount: ngn(510000), Reference: "refund:t"}

	// The first refund succeeds at Paystack, but the response is lost.
	f.FailAfterApply("POST /refund", 1)
	if _, err := c.RefundPayment(ctx, req); !providers.IsAmbiguous(err) {
		t.Fatalf("want ambiguous, got %v", err)
	}
	id1, err := c.RefundPayment(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	id2, err := c.RefundPayment(ctx, req)
	if err != nil || id1 != id2 || f.Refunds() != 1 {
		t.Fatalf("ids %s %s, refunds %d, err %v", id1, id2, f.Refunds(), err)
	}
}

func TestWebhookSignature(t *testing.T) {
	c, _ := setup(t)
	body := []byte(`{"event":"charge.success","data":{"reference":"txn-1.abc","amount":510000}}`)
	h := http.Header{}
	h.Set("X-Paystack-Signature", paystack.Sign(secret, body))
	evt, err := c.ParseWebhook(h, body)
	if err != nil || !evt.Relevant || evt.Reference != "txn-1.abc" {
		t.Fatalf("valid: %+v %v", evt, err)
	}

	tampered := []byte(strings.Replace(string(body), "510000", "999999", 1))
	if _, err := c.ParseWebhook(h, tampered); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("tampered body: %v", err)
	}
	h2 := http.Header{}
	h2.Set("X-Paystack-Signature", paystack.Sign("sk_test_other", body))
	if _, err := c.ParseWebhook(h2, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err := c.ParseWebhook(http.Header{}, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("missing header: %v", err)
	}

	other := []byte(`{"event":"refund.processed","data":{"reference":"x"}}`)
	h3 := http.Header{}
	h3.Set("X-Paystack-Signature", paystack.Sign(secret, other))
	if evt, err := c.ParseWebhook(h3, other); err != nil || evt.Relevant {
		t.Fatalf("other events are acknowledged but ignored: %+v %v", evt, err)
	}
}
