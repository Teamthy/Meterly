package paystack_test

import (
	"context"
	"encoding/json"
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

	dispute := []byte(`{"event":"charge.dispute.create","data":{"transaction":{"reference":"txn-1.abc"}}}`)
	h4 := http.Header{}
	h4.Set("X-Paystack-Signature", paystack.Sign(secret, dispute))
	evt, err = c.ParseWebhook(h4, dispute)
	if err != nil || !evt.Relevant || evt.Reference != "txn-1.abc" || evt.Type != "charge.dispute.create" {
		t.Fatalf("dispute: %+v %v", evt, err)
	}
}

func TestChargeSavedAndLostResponseReconciliation(t *testing.T) {
	c, f := setup(t)

	initRes, err := c.InitializePayment(ctx, providers.PaymentInit{
		Reference: "seed-1", Amount: ngn(510000), Email: "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Pay(initRes.ProviderRef, 0)

	verified, err := c.VerifyPayment(ctx, initRes.ProviderRef)
	if err != nil || verified.Authorization == nil || !verified.Authorization.Reusable {
		t.Fatalf("expected reusable card authorization, got %+v err=%v", verified, err)
	}
	auth := verified.Authorization
	if auth.CardBrand != "visa" || auth.Last4 != "4081" || auth.ExpMonth != 12 || auth.ExpYear != 2030 || auth.CustomerEmail != "ada@example.com" {
		t.Fatalf("unexpected authorization metadata: %+v", auth)
	}

	// 1. Happy-path ChargeSaved + VerifyPayment confirmation.
	chg, err := c.ChargeSaved(ctx, providers.ChargeRequest{
		AuthorizationRef: auth.AuthorizationCode,
		Email:            auth.CustomerEmail,
		Amount:           ngn(1010000),
		Reference:        "apchg-run-1",
	})
	if err != nil || chg.Status != providers.PaymentSucceeded || chg.Amount != ngn(1010000) {
		t.Fatalf("charge_authorization: %+v err=%v", chg, err)
	}
	v1, err := c.VerifyPayment(ctx, "apchg-run-1")
	if err != nil || v1.Status != providers.PaymentSucceeded || v1.Amount != ngn(1010000) {
		t.Fatalf("verify charged ref: %+v err=%v", v1, err)
	}

	// 2. Lost response on charge_authorization: charge applied at Paystack, HTTP 500 returned.
	// Reconciling by VerifyPayment("apchg-run-2") confirms success with only 1 charge call.
	f.FailAfterApply("POST /transaction/charge_authorization", 1)
	if _, err := c.ChargeSaved(ctx, providers.ChargeRequest{
		AuthorizationRef: auth.AuthorizationCode,
		Email:            auth.CustomerEmail,
		Amount:           ngn(510000),
		Reference:        "apchg-run-2",
	}); !providers.IsAmbiguous(err) {
		t.Fatalf("lost charge_authorization response must be ambiguous, got %v", err)
	}
	v2, err := c.VerifyPayment(ctx, "apchg-run-2")
	if err != nil || v2.Status != providers.PaymentSucceeded || v2.Amount != ngn(510000) {
		t.Fatalf("reconcile lost charge_authorization via VerifyPayment: %+v err=%v", v2, err)
	}
	if got := f.Requests["POST /transaction/charge_authorization"]; got != 2 {
		t.Fatalf("expected exactly 2 total charge_authorization calls, got %d", got)
	}

	// 3. send_otp / pending maps conservatively to PaymentPending; failed maps to PaymentFailed.
	f.SetAuthorizationChargeStatus(auth.AuthorizationCode, "send_otp")
	otpRes, err := c.ChargeSaved(ctx, providers.ChargeRequest{
		AuthorizationRef: auth.AuthorizationCode,
		Email:            auth.CustomerEmail,
		Amount:           ngn(510000),
		Reference:        "apchg-run-3",
	})
	if err != nil || otpRes.Status != providers.PaymentPending {
		t.Fatalf("send_otp must map to PaymentPending: %+v err=%v", otpRes, err)
	}

	f.SetAuthorizationChargeStatus(auth.AuthorizationCode, "failed")
	declined, err := c.ChargeSaved(ctx, providers.ChargeRequest{
		AuthorizationRef: auth.AuthorizationCode,
		Email:            auth.CustomerEmail,
		Amount:           ngn(510000),
		Reference:        "apchg-run-4",
	})
	if err != nil || declined.Status != providers.PaymentFailed {
		t.Fatalf("declined charge must map to PaymentFailed: %+v err=%v", declined, err)
	}

	// 4. Missing reference unwraps to providers.ErrPaymentNotFound.
	if _, err := c.VerifyPayment(ctx, "apchg-never-sent"); !errors.Is(err, providers.ErrPaymentNotFound) {
		t.Fatalf("expected ErrPaymentNotFound, got %v", err)
	}
}

func TestChargeDocumentedTestCard(t *testing.T) {
	c, f := setup(t)
	initRes, err := c.InitializePayment(ctx, providers.PaymentInit{
		Reference: "card-1", Amount: ngn(10000), Email: "a@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"email": "a@example.com", "amount": "10000", "currency": "NGN",
		"reference": initRes.ProviderRef,
		"card":      map[string]string{"number": "4084084084084081", "cvv": "408", "expiry_month": "12", "expiry_year": "30"},
	})
	req, _ := http.NewRequest(http.MethodPost, f.URL+"/charge", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("charge HTTP %d", res.StatusCode)
	}
	v, err := c.VerifyPayment(ctx, initRes.ProviderRef)
	if err != nil || v.Status != providers.PaymentSucceeded || v.Authorization == nil || !v.Authorization.Reusable {
		t.Fatalf("documented success card: %+v %v", v, err)
	}

	init2, err := c.InitializePayment(ctx, providers.PaymentInit{
		Reference: "card-2", Amount: ngn(10000), Email: "a@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{
		"email": "a@example.com", "amount": "10000", "currency": "NGN",
		"reference": init2.ProviderRef,
		"card":      map[string]string{"number": "0000000000000000", "cvv": "000", "expiry_month": "12", "expiry_year": "30"},
	})
	req, _ = http.NewRequest(http.MethodPost, f.URL+"/charge", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	v, err = c.VerifyPayment(ctx, init2.ProviderRef)
	if err != nil || v.Status == providers.PaymentSucceeded {
		t.Fatalf("undocumented PAN must not succeed: %+v %v", v, err)
	}
}

func TestListTransactionsRefundsSettlements(t *testing.T) {
	c, f := setup(t)
	paid := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	f.SeedTransaction("ref-list", "success", 510000, paid)
	f.SeedRefund("ref-list", "processed", 510000)
	f.SeedSettlement(42, "success", 510000, 10000, 500000, paid)
	from, to := paid, paid.Add(24*time.Hour)
	txns, err := c.ListTransactions(ctx, from, to)
	if err != nil || len(txns) != 1 || txns[0].Reference != "ref-list" || txns[0].Amount != 510000 {
		t.Fatalf("txns %+v %v", txns, err)
	}
	refunds, err := c.ListRefunds(ctx, from, to)
	if err != nil || len(refunds) != 1 || refunds[0].TransactionRef != "ref-list" {
		t.Fatalf("refunds %+v %v", refunds, err)
	}
	settlements, err := c.ListSettlements(ctx, from, to)
	if err != nil || len(settlements) != 1 || settlements[0].ID != 42 || settlements[0].TotalFees != 10000 {
		t.Fatalf("settlements %+v %v", settlements, err)
	}
	f.SeedDispute(2867, "ref-list", "awaiting-merchant-feedback", 510000)
	ds, err := c.ListDisputes(ctx, from, to)
	if err != nil || len(ds) != 1 || ds[0].ID != 2867 || ds[0].Reference != "ref-list" {
		t.Fatalf("disputes %+v %v", ds, err)
	}
	got, err := c.FetchDispute(ctx, 2867)
	if err != nil || got.ID != 2867 {
		t.Fatalf("fetch %+v %v", got, err)
	}
	if err := c.AddEvidence(ctx, 2867, "ops@example.com", "Meterly", "08000000000", "prepaid electricity token delivered"); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveDispute(ctx, 2867, "declined", "token was delivered", 0); err != nil {
		t.Fatal(err)
	}
}
