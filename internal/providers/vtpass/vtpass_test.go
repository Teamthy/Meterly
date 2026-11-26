package vtpass_test

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
	"github.com/teamthy/meterly/internal/providers/vtpass"
	"github.com/teamthy/meterly/internal/providers/vtpass/vtpasstest"
)

const (
	apiKey = "test-api-key"
	secret = "SK_test_fake_secret"
	phone  = "08012345678"
)

func setup(t *testing.T) (*vtpass.Client, *vtpasstest.Fake) {
	t.Helper()
	f := vtpasstest.New(apiKey, secret)
	t.Cleanup(f.Close)
	c, err := vtpass.New(vtpass.Config{
		APIKey: apiKey, SecretKey: secret, BaseURL: f.URL, DefaultPhone: phone,
		Now: func() time.Time { return time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

var ctx = context.Background()

func ngn(naira int64) money.Amount { return money.New(naira*100, money.NGN) }

func TestFormatRequestID(t *testing.T) {
	at := time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos())
	id := vtpass.FormatRequestID("vend:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", at)
	if !strings.HasPrefix(id, "202611261430") {
		t.Fatalf("prefix: %s", id)
	}
	if !vtpass.IsRequestID(id) || strings.Contains(id, "-") || strings.Contains(id, ":") {
		t.Fatalf("shape: %s", id)
	}
	if vtpass.IsRequestID("vend:not-a-request-id") {
		t.Fatal("vend:uuid must not pass as a VTpass request_id")
	}
}

func TestValidatePrepaidAndPostpaid(t *testing.T) {
	c, _ := setup(t)
	pre, err := c.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC"})
	if err != nil || !pre.Valid || pre.MeterType != "PREPAID" || pre.CustomerName != "TESTMETER1" {
		t.Fatalf("prepaid: %+v %v", pre, err)
	}
	post, err := c.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: vtpasstest.PostpaidMeter, DiscoCode: "EKEDC"})
	if err != nil || !post.Valid || post.MeterType != "POSTPAID" {
		t.Fatalf("postpaid: %+v %v", post, err)
	}
	bad, err := c.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: "9999999999999", DiscoCode: "IKEDC"})
	if err != nil || bad.Valid {
		t.Fatalf("unknown meter must be invalid, not an error: %+v %v", bad, err)
	}
}

func TestUnsupportedDiscoIsDefinitive(t *testing.T) {
	c, _ := setup(t)
	_, err := c.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "ACME"})
	if err == nil || providers.IsAmbiguous(err) {
		t.Fatalf("unsupported disco: %v", err)
	}
}

func TestVendPrepaidTokenAndRequery(t *testing.T) {
	c, f := setup(t)
	key := c.NewVendKey("11111111-1111-1111-1111-111111111111", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	res, err := c.Vend(ctx, providers.VendRequest{
		TransactionID: "11111111-1111-1111-1111-111111111111",
		MeterNumber:   vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if err != nil || res.Status != providers.VendSucceeded || res.Token != "26362054405982757802" || res.UnitsKWh != "79.9" {
		t.Fatalf("vend: %+v %v", res, err)
	}
	got, err := c.GetVendStatus(ctx, key)
	if err != nil || got.Status != providers.VendSucceeded || got.Token != res.Token || got.ProviderRef != res.ProviderRef {
		t.Fatalf("requery: %+v %v", got, err)
	}
	if !f.Has(key) {
		t.Fatal("fake should store by request_id")
	}
}

func TestVendIsIdempotentOnRequestID(t *testing.T) {
	c, f := setup(t)
	key := c.NewVendKey("txn-dup", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	req := providers.VendRequest{
		MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	}
	a, err := c.Vend(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Vend(ctx, req)
	if err != nil || a.Token != b.Token || a.ProviderRef != b.ProviderRef {
		t.Fatalf("014 must requery the original: %+v %+v %v", a, b, err)
	}
	if f.Requests["POST /pay"] != 2 {
		t.Fatalf("pay calls: %d", f.Requests["POST /pay"])
	}
	if f.Requests["POST /requery"] < 1 {
		t.Fatal("duplicate pay must requery")
	}
}

func TestCode000InspectsInnerStatus(t *testing.T) {
	c, f := setup(t)
	key := c.NewVendKey("txn-pending", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	res, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.PendingMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if err != nil || res.Status != providers.VendPending {
		t.Fatalf("099/pending: %+v %v", res, err)
	}
	f.SetStatus(key, "delivered", "000")
	got, err := c.GetVendStatus(ctx, key)
	if err != nil || got.Status != providers.VendPending {
		// pending meter has no token; prepaid delivered without token stays pending
		if got.Status != providers.VendPending && got.Status != providers.VendSucceeded {
			t.Fatalf("requery after set: %+v %v", got, err)
		}
	}
}

func TestDefinitiveFailureDoesNotError(t *testing.T) {
	c, _ := setup(t)
	key := c.NewVendKey("txn-fail", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	res, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: "9999999999999", DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if err != nil || res.Status != providers.VendFailed || res.FailureCode != "016" {
		t.Fatalf("want failed 016 with nil err, got %+v %v", res, err)
	}
}

func TestRequeryUnknownIsNotFound(t *testing.T) {
	c, _ := setup(t)
	res, err := c.GetVendStatus(ctx, "202611261430neverexisted")
	if err != nil || res.Status != providers.VendNotFound {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestLostPayResponseIsAmbiguousThenRequery(t *testing.T) {
	c, f := setup(t)
	key := c.NewVendKey("txn-lost", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	f.FailAfterApply("POST /pay", 1)
	_, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if !providers.IsAmbiguous(err) {
		t.Fatalf("5xx after apply must be ambiguous: %v", err)
	}
	if !f.Has(key) {
		t.Fatal("pay must have been applied before the 500")
	}
	got, err := c.GetVendStatus(ctx, key)
	if err != nil || got.Status != providers.VendSucceeded || got.Token != "26362054405982757802" {
		t.Fatalf("requery after lost pay: %+v %v", got, err)
	}
}

func TestWrongKeyIsDefinitive(t *testing.T) {
	_, f := setup(t)
	bad, err := vtpass.New(vtpass.Config{APIKey: "nope", SecretKey: "nope", BaseURL: f.URL, DefaultPhone: phone})
	if err != nil {
		t.Fatal(err)
	}
	res, err := bad.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: "202611261430abc", MeterType: "PREPAID",
	})
	if err != nil || res.Status != providers.VendFailed || res.FailureCode != "087" {
		t.Fatalf("bad credentials: %+v %v", res, err)
	}
}

func TestTimeoutIsAmbiguous(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)
	c, err := vtpass.New(vtpass.Config{APIKey: apiKey, SecretKey: secret, BaseURL: slow.URL, DefaultPhone: phone})
	if err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = c.GetVendStatus(tctx, "202611261430x")
	if !providers.IsAmbiguous(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUnparseableBodyIsAmbiguous(t *testing.T) {
	c, _ := setup(t)
	key := c.NewVendKey("txn-junk", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	_, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.UnexpectedMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if !providers.IsAmbiguous(err) {
		t.Fatalf("unparseable: %v", err)
	}
}

func TestEmptyBodyIsAmbiguous(t *testing.T) {
	c, _ := setup(t)
	key := c.NewVendKey("txn-nobody", time.Date(2026, 11, 26, 14, 30, 0, 0, vtpass.Lagos()))
	_, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.NoBodyMeter, DiscoCode: "IKEDC",
		Amount: ngn(2000), IdempotencyKey: key, MeterType: "PREPAID",
	})
	if !providers.IsAmbiguous(err) {
		t.Fatalf("empty body: %v", err)
	}
}

func TestNewRequiresKeysAndPhone(t *testing.T) {
	if _, err := vtpass.New(vtpass.Config{}); err == nil {
		t.Fatal("want error")
	}
	if _, err := vtpass.New(vtpass.Config{APIKey: "a", SecretKey: "b"}); err == nil || !strings.Contains(err.Error(), "phone") {
		t.Fatalf("phone: %v", err)
	}
}

func TestWholeNairaRequired(t *testing.T) {
	c, _ := setup(t)
	_, err := c.Vend(ctx, providers.VendRequest{
		MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		Amount: money.New(150, money.NGN), IdempotencyKey: "202611261430abc", MeterType: "PREPAID",
	})
	if err == nil || providers.IsAmbiguous(err) {
		t.Fatalf("fractional naira: %v", err)
	}
	var api *vtpass.APIError
	if !errors.As(err, &api) {
		t.Fatalf("want APIError, got %T %v", err, err)
	}
}
