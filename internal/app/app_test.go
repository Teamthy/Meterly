package app_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestBuildWiresOIDC(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	enc := base64.RawURLEncoding.EncodeToString
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer jwks.Close()
	sign := func(c map[string]any) string {
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
		b, _ := json.Marshal(c)
		in := enc(h) + "." + enc(b)
		d := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
		return in + "." + enc(sig)
	}
	cfg, err := config.Load(func(k string) string {
		return map[string]string{"MODE": "dev", "AUTH_MODE": "oidc", "OIDC_ISSUER": "https://id.test/", "OIDC_AUDIENCE": "meterly",
			"OIDC_JWKS_URL": jwks.URL}[k]
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
	get := func(tok string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/admin/providers", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	base := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": "https://id.test/", "aud": "meterly", "sub": "staff-1", "exp": time.Now().Add(time.Hour).Unix()}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	if c := get("operator:dev-style-token"); c != 401 {
		t.Fatalf("dev tokens must not work under OIDC: %d", c)
	}
	if c := get(sign(base(map[string]any{"roles": []string{"operator"}, "amr": []string{"pwd"}}))); c != 403 {
		t.Fatalf("staff without MFA: %d", c)
	}
	if c := get(sign(base(map[string]any{"roles": []string{"operator"}, "amr": []string{"pwd", "mfa"}}))); c != 200 {
		t.Fatalf("staff with MFA: %d", c)
	}
}
