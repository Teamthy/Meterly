package auth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/auth"
)

const (
	iss = "https://id.example.test/"
	aud = "meterly-api"
)

type idp struct {
	srv     *httptest.Server
	rsa     *rsa.PrivateKey
	ec      *ecdsa.PrivateKey
	rsaKid  string
	fetches atomic.Int32
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newIDP(t *testing.T) *idp {
	t.Helper()
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p := &idp{rsa: rk, ec: ek, rsaKid: "rsa-1"}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.fetches.Add(1)
		keys := []map[string]string{
			{"kty": "RSA", "kid": p.rsaKid, "use": "sig", "n": enc(p.rsa.N.Bytes()), "e": enc(big.NewInt(int64(p.rsa.E)).Bytes())},
			{"kty": "EC", "kid": "ec-1", "crv": "P-256", "x": enc(p.ec.X.FillBytes(make([]byte, 32))), "y": enc(p.ec.Y.FillBytes(make([]byte, 32)))},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) verifier(t *testing.T, mfa ...string) *auth.OIDC {
	t.Helper()
	v, err := auth.NewOIDC(auth.OIDCConfig{Issuer: iss, Audience: aud, JWKSURL: p.srv.URL, MFAValues: mfa,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func claims(extra map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{"iss": iss, "aud": aud, "sub": "user-123", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix()}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (p *idp) sign(alg, kid string, c map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	b, _ := json.Marshal(c)
	input := enc(h) + "." + enc(b)
	d := sha256.Sum256([]byte(input))
	var sig []byte
	switch alg {
	case "RS256":
		sig, _ = rsa.SignPKCS1v15(rand.Reader, p.rsa, crypto.SHA256, d[:])
	case "ES256":
		r, s, _ := ecdsa.Sign(rand.Reader, p.ec, d[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "HS256":
		// Key confusion attack: HMAC keyed with the public RSA modulus.
		m := hmac.New(sha256.New, p.rsa.N.Bytes())
		m.Write([]byte(input))
		sig = m.Sum(nil)
	}
	return input + "." + enc(sig)
}

var ctx = context.Background()

func TestValidTokens(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	for _, tok := range []string{p.sign("RS256", "rsa-1", claims(nil)), p.sign("ES256", "ec-1", claims(nil))} {
		pr, err := v.Verify(ctx, tok)
		if err != nil || pr.Subject != "user-123" || pr.Has(auth.RoleOperator) {
			t.Fatalf("%+v %v", pr, err)
		}
	}
	// Audience as an array, and roles via a nested claim path.
	v2, _ := auth.NewOIDC(auth.OIDCConfig{Issuer: iss, Audience: aud, JWKSURL: p.srv.URL, RolesClaim: "realm_access.roles"})
	pr, err := v2.Verify(ctx, p.sign("RS256", "rsa-1", claims(map[string]any{
		"aud": []string{"other", aud}, "realm_access": map[string]any{"roles": []string{"admin", "unknown-role"}},
	})))
	if err != nil || !pr.Has(auth.RoleAdmin) || !pr.Has(auth.RoleOperator) || len(pr.Roles) != 1 {
		t.Fatalf("%+v %v", pr, err)
	}
}

func TestRejections(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	good := p.sign("RS256", "rsa-1", claims(nil))
	parts := strings.Split(good, ".")
	none := enc([]byte(`{"alg":"none","kid":"rsa-1"}`)) + "." + parts[1] + "."
	tamperedPayload, _ := json.Marshal(claims(map[string]any{"sub": "attacker"}))
	cases := map[string]string{
		"expired":          p.sign("RS256", "rsa-1", claims(map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()})),
		"missing exp":      p.sign("RS256", "rsa-1", claims(map[string]any{"exp": nil})),
		"not yet valid":    p.sign("RS256", "rsa-1", claims(map[string]any{"nbf": time.Now().Add(5 * time.Minute).Unix()})),
		"future iat":       p.sign("RS256", "rsa-1", claims(map[string]any{"iat": time.Now().Add(5 * time.Minute).Unix()})),
		"wrong issuer":     p.sign("RS256", "rsa-1", claims(map[string]any{"iss": "https://evil.test/"})),
		"wrong audience":   p.sign("RS256", "rsa-1", claims(map[string]any{"aud": "someone-else"})),
		"missing sub":      p.sign("RS256", "rsa-1", claims(map[string]any{"sub": nil})),
		"alg none":         none,
		"HS256 confusion":  p.sign("HS256", "rsa-1", claims(nil)),
		"tampered payload": parts[0] + "." + enc(tamperedPayload) + "." + parts[2],
		"alg/key mismatch": p.sign("ES256", "rsa-1", claims(nil)),
		"unknown kid":      p.sign("RS256", "nope", claims(nil)),
		"garbage":          "not.a.jwt",
		"two parts":        "abc.def",
	}
	for name, tok := range cases {
		if _, err := v.Verify(ctx, tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: want rejection, got %v", name, err)
		}
	}
	// Within the 60s skew an expired token is still accepted.
	if _, err := v.Verify(ctx, p.sign("RS256", "rsa-1", claims(map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()}))); err != nil {
		t.Fatalf("within skew: %v", err)
	}
}

func TestStaffRolesRequireMFA(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t, "mfa", "otp")
	noMFA, err := v.Verify(ctx, p.sign("RS256", "rsa-1", claims(map[string]any{"roles": []string{"operator"}, "amr": []string{"pwd"}})))
	if err != nil || noMFA.Has(auth.RoleOperator) {
		t.Fatalf("operator without MFA must be a plain customer: %+v %v", noMFA, err)
	}
	withMFA, err := v.Verify(ctx, p.sign("RS256", "rsa-1", claims(map[string]any{"roles": "operator", "amr": []string{"pwd", "otp"}})))
	if err != nil || !withMFA.Has(auth.RoleOperator) || withMFA.Has(auth.RoleAdmin) {
		t.Fatalf("%+v %v", withMFA, err)
	}
}

func TestKeyRotationAndCaching(t *testing.T) {
	p := newIDP(t)
	v := p.verifier(t)
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(ctx, p.sign("RS256", "rsa-1", claims(nil))); err != nil {
			t.Fatal(err)
		}
	}
	if n := p.fetches.Load(); n != 1 {
		t.Fatalf("JWKS should be cached, fetched %d times", n)
	}
	// The IdP rotates to a new key id; the first token with it triggers a refresh.
	p.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	p.rsaKid = "rsa-2"
	if _, err := v.Verify(ctx, p.sign("RS256", "rsa-2", claims(nil))); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	// Unknown kids don't hammer the IdP: refreshes are rate-limited.
	before := p.fetches.Load()
	for i := 0; i < 5; i++ {
		_, _ = v.Verify(ctx, p.sign("RS256", "bogus", claims(nil)))
	}
	if p.fetches.Load() != before {
		t.Fatalf("unknown kid refetched JWKS %d times", p.fetches.Load()-before)
	}
}

func TestDiscovery(t *testing.T) {
	p := newIDP(t)
	var issuer string
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": p.srv.URL})
	}))
	defer disc.Close()
	issuer = disc.URL
	v, _ := auth.NewOIDC(auth.OIDCConfig{Issuer: issuer, Audience: aud})
	if _, err := v.Verify(ctx, p.sign("RS256", "rsa-1", claims(map[string]any{"iss": issuer}))); err != nil {
		t.Fatalf("discovery: %v", err)
	}
}

func TestDevAuthRoles(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer admin:ola")
	p, err := auth.DevAuth{}.Authenticate(r)
	if err != nil || !p.Has(auth.RoleAdmin) || !p.Has(auth.RoleOperator) {
		t.Fatalf("%+v %v", p, err)
	}
	r.Header.Set("Authorization", "Bearer ada")
	if p, _ := (auth.DevAuth{}).Authenticate(r); p.Has(auth.RoleOperator) {
		t.Fatal("plain subject must not be staff")
	}
	r.Header.Del("Authorization")
	if _, err := (auth.DevAuth{}).Authenticate(r); err == nil {
		t.Fatal("missing token")
	}
}
