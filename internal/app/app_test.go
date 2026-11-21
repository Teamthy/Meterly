package app_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teamthy/meterly/internal/app"
	"github.com/teamthy/meterly/internal/platform/config"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
)

func TestBuildWiresPaystack(t *testing.T) {
	fake := paystacktest.New("sk_test_wiring")
	defer fake.Close()
	cfg, err := config.Load(func(k string) string {
		return map[string]string{"MODE": "dev", "PAYMENT_PROVIDER": "paystack", "PAYSTACK_SECRET_KEY": "sk_test_wiring", "PAYSTACK_BASE_URL": fake.URL}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	body := []byte(`{"event":"transfer.success","data":{}}`)
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/payments/webhooks/paystack", bytes.NewReader(body))
		req.Header.Set("X-Paystack-Signature", sig)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := post(paystack.Sign("sk_test_wiring", body)); c != 200 {
		t.Fatalf("signed webhook: %d", c)
	}
	if c := post("00"); c != 401 {
		t.Fatalf("unsigned webhook: %d", c)
	}
	// The mock route is gone when Paystack is in use.
	res, _ := http.Post(srv.URL+"/v1/payments/webhooks/mockpay", "application/json", bytes.NewReader(body))
	if res.StatusCode != 404 {
		t.Fatalf("mock webhook route: %d", res.StatusCode)
	}
	res.Body.Close()
}
