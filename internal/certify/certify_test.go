package certify

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/notifications/africastalking/africastalkingtest"
	"github.com/teamthy/meterly/internal/notifications/resend/resendtest"
	"github.com/teamthy/meterly/internal/notifications/whatsapp/whatsapptest"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
	"github.com/teamthy/meterly/internal/providers/vtpass/vtpasstest"
)

func TestSanitize(t *testing.T) {
	in := "sk_test_abc123 paid 4084084084084081 to ada@example.com AUTH_secret"
	out := Sanitize(in)
	for _, banned := range []string{"sk_test_abc123", "4084084084084081", "ada@example.com", "AUTH_secret"} {
		if strings.Contains(out, banned) {
			t.Fatalf("still contains %q: %s", banned, out)
		}
	}
}

func TestRunSkipsWithoutKeys(t *testing.T) {
	rep := Run(context.Background(), Config{})
	if rep.Summary.Failed != 0 {
		t.Fatalf("no keys must not fail: %+v", rep)
	}
	if rep.Summary.Passed != 0 || rep.Summary.Skipped == 0 {
		t.Fatalf("want all skipped, got %+v", rep.Summary)
	}
	raw, _ := json.Marshal(rep)
	s := string(raw)
	if strings.Contains(s, "sk_") || strings.Contains(s, "4084084084084081") {
		t.Fatalf("report leaked a secret or PAN: %s", s)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "certify-report.json")
	rep := Run(context.Background(), Config{})
	if err := WriteFile(path, rep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if json.Unmarshal(b, &got) != nil || got.Summary.Skipped == 0 {
		t.Fatalf("round-trip: %s", b)
	}
}

func TestPaystackAgainstFake(t *testing.T) {
	f := paystacktest.New("sk_test_fake")
	t.Cleanup(f.Close)
	results := Paystack(context.Background(), PaystackConfig{
		SecretKey: "sk_test_fake", BaseURL: f.URL, Email: "certify@example.com",
	})
	requirePass(t, results, "webhook_captured_payload")
}

func TestVTPassAgainstFake(t *testing.T) {
	f := vtpasstest.New("api", "secret")
	t.Cleanup(f.Close)
	results := VTPass(context.Background(), VTPassConfig{
		APIKey: "api", SecretKey: "secret", Phone: "08012345678", BaseURL: f.URL,
	})
	requirePass(t, results, "low_balance")
}

func TestAfricasTalkingAgainstFake(t *testing.T) {
	f := africastalkingtest.New("sandbox", "at_key")
	t.Cleanup(f.Close)
	results := AfricasTalking(context.Background(), ATConfig{
		Username: "sandbox", APIKey: "at_key", WebhookSecret: "wh_secret",
		BaseURL: f.URL, To: "+2348012345678",
	})
	requirePass(t, results)
}

func TestResendAgainstFake(t *testing.T) {
	f := resendtest.New("re_test_fake")
	t.Cleanup(f.Close)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	results := Resend(context.Background(), ResendConfig{
		APIKey: "re_test_fake", From: "Meterly <noreply@meterly.ng>",
		WebhookSecret: secret, BaseURL: f.URL, To: "certify@example.com",
	})
	requirePass(t, results)
}

func TestWhatsAppAgainstFake(t *testing.T) {
	f := whatsapptest.New("109876543210", "wa_token")
	t.Cleanup(f.Close)
	results := WhatsApp(context.Background(), WhatsAppConfig{
		PhoneNumberID: "109876543210", AccessToken: "wa_token",
		AppSecret: "wa_secret", VerifyToken: "wa_verify",
		BaseURL: f.URL, To: "+2348012345678", Template: "meterly_offline_token_v1",
	})
	requirePass(t, results)
}

func TestOIDCAgainstInProcessJWKS(t *testing.T) {
	idp := startTestIDP(t)
	tok := idp.sign(map[string]any{
		"iss": idp.issuer, "aud": "meterly-api", "sub": "staff-1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"roles": []string{"admin"}, "amr": []string{"mfa"},
	})
	results := OIDC(context.Background(), OIDCConfig{
		Issuer: idp.issuer, Audience: "meterly-api", JWKSURL: idp.jwksURL,
		StaffToken: tok, MFAValues: []string{"mfa"}, HTTPClient: http.DefaultClient,
	})
	requirePass(t, results)
}

func TestLiveKeysRefused(t *testing.T) {
	results := Paystack(context.Background(), PaystackConfig{SecretKey: "sk_live_nope"})
	for _, r := range results {
		if r.Status != StatusSkipped {
			t.Fatalf("sk_live_ must skip (or refuse) without CERTIFY_ALLOW_LIVE: %+v", r)
		}
	}
}

func requirePass(t *testing.T, results []Result, allowSkip ...string) {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("no results")
	}
	allow := map[string]bool{}
	for _, s := range allowSkip {
		allow[s] = true
	}
	for _, r := range results {
		switch r.Status {
		case StatusFail:
			t.Errorf("%s/%s failed: %s", r.Provider, r.Scenario, r.Error)
		case StatusSkipped:
			if !allow[r.Scenario] {
				t.Errorf("%s/%s skipped: %s", r.Provider, r.Scenario, r.Detail)
			}
		}
		for _, id := range r.ProviderIDs {
			if strings.Contains(id, "@") || len(id) >= 20 && isAllDigits(id) {
				t.Errorf("provider id looks like PII: %s", id)
			}
		}
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type testIDP struct {
	issuer  string
	jwksURL string
	rsa     *rsa.PrivateKey
	kid     string
}

func startTestIDP(t *testing.T) *testIDP {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &testIDP{rsa: rk, kid: "rsa-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": p.issuer, "jwks_uri": p.jwksURL,
			})
		case "/jwks":
			n := base64.RawURLEncoding.EncodeToString(p.rsa.N.Bytes())
			e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.rsa.E)).Bytes())
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{
					{"kty": "RSA", "kid": p.kid, "use": "sig", "n": n, "e": e},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p.issuer = srv.URL
	p.jwksURL = srv.URL + "/jwks"
	return p
}

func (p *testIDP) sign(claims map[string]any) string {
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": p.kid, "typ": "JWT"})
	b, _ := json.Marshal(claims)
	input := enc(h) + "." + enc(b)
	sum := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.rsa, crypto.SHA256, sum[:])
	return input + "." + enc(sig)
}
