package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// OIDCConfig configures bearer-JWT verification.
type OIDCConfig struct {
	Issuer   string // exact "iss" value
	Audience string // must appear in "aud"
	// JWKSURL; if empty it is discovered from
	// Issuer + "/.well-known/openid-configuration".
	JWKSURL string
	// RolesClaim is the claim holding roles: a string array, a
	// space-separated string, or a dotted path to one ("realm_access.roles").
	RolesClaim string
	// MFAValues: staff roles count only if "amr" contains one of these.
	// Empty disables the check (dev/test only; config enforces it otherwise).
	MFAValues  []string
	ClockSkew  time.Duration // default 60s
	CacheTTL   time.Duration // JWKS cache lifetime, default 10m
	HTTPClient *http.Client
	Log        *slog.Logger
	Now        func() time.Time
}

type OIDC struct {
	cfg OIDCConfig

	mu          sync.Mutex
	keys        map[string]crypto.PublicKey // by kid
	fetchedAt   time.Time
	lastRefresh time.Time
	jwksURL     string
}

func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("auth: OIDC issuer and audience are required")
	}
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = 60 * time.Second
	}
	if cfg.ClockSkew > 60*time.Second {
		return nil, errors.New("auth: clock skew must be at most 60s")
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 10 * time.Minute
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &OIDC{cfg: cfg, jwksURL: cfg.JWKSURL}, nil
}

func (o *OIDC) Authenticate(r *http.Request) (Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return o.Verify(r.Context(), tok)
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUnauthenticated}, args...)...)
}

// Verify checks signature, issuer, audience and validity times, then maps
// the claims to a Principal.
func (o *OIDC) Verify(ctx context.Context, token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, fail("malformed token")
	}
	hb, err := b64(parts[0])
	if err != nil {
		return Principal{}, fail("malformed header")
	}
	var h header
	if json.Unmarshal(hb, &h) != nil {
		return Principal{}, fail("malformed header")
	}
	// Only asymmetric algorithms: "none" and HS* (key confusion) are rejected.
	if h.Alg != "RS256" && h.Alg != "ES256" {
		return Principal{}, fail("unsupported alg %q", h.Alg)
	}
	sig, err := b64(parts[2])
	if err != nil {
		return Principal{}, fail("malformed signature")
	}
	key, err := o.key(ctx, h.Kid)
	if err != nil {
		return Principal{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if h.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return Principal{}, fail("bad signature")
		}
	case *ecdsa.PublicKey:
		if h.Alg != "ES256" || len(sig) != 64 || k.Curve != elliptic.P256() {
			return Principal{}, fail("bad signature")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, digest[:], r, s) {
			return Principal{}, fail("bad signature")
		}
	default:
		return Principal{}, fail("unsupported key type")
	}

	pb, err := b64(parts[1])
	if err != nil {
		return Principal{}, fail("malformed payload")
	}
	var claims map[string]any
	dec := json.NewDecoder(strings.NewReader(string(pb)))
	dec.UseNumber()
	if dec.Decode(&claims) != nil {
		return Principal{}, fail("malformed payload")
	}
	if iss, _ := claims["iss"].(string); iss != o.cfg.Issuer {
		return Principal{}, fail("wrong issuer")
	}
	if !audienceOK(claims["aud"], o.cfg.Audience) {
		return Principal{}, fail("wrong audience")
	}
	now := o.cfg.Now()
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return Principal{}, fail("missing exp")
	}
	if now.After(exp.Add(o.cfg.ClockSkew)) {
		return Principal{}, fail("token expired")
	}
	if nbf, ok := numericDate(claims["nbf"]); ok && now.Add(o.cfg.ClockSkew).Before(nbf) {
		return Principal{}, fail("token not yet valid")
	}
	if iat, ok := numericDate(claims["iat"]); ok && now.Add(o.cfg.ClockSkew).Before(iat) {
		return Principal{}, fail("token issued in the future")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, fail("missing sub")
	}

	p := Principal{Subject: sub}
	for _, r := range rolesFrom(claims, o.cfg.RolesClaim) {
		switch Role(r) {
		case RoleOperator, RoleAdmin:
			p.Roles = append(p.Roles, Role(r))
		}
	}
	if len(p.Roles) > 0 && len(o.cfg.MFAValues) > 0 && !mfaUsed(claims["amr"], o.cfg.MFAValues) {
		o.cfg.Log.Warn("staff roles ignored: token has no MFA", "sub_len", len(sub))
		p.Roles = nil
	}
	if verified, ok := claims["email_verified"].(bool); ok && verified {
		if em, _ := claims["email"].(string); em != "" {
			if norm, valid := cleanVerifiedEmail(em); valid {
				p.VerifiedEmail = norm
			}
		}
	}
	if verified, ok := claims["phone_number_verified"].(bool); ok && verified {
		if ph, _ := claims["phone_number"].(string); ph != "" {
			if norm, valid := cleanVerifiedPhone(ph); valid {
				p.VerifiedPhone = norm
			}
		}
	}
	if name, _ := claims["name"].(string); name != "" {
		p.DisplayName = cleanDisplayName(name)
	} else if given, _ := claims["given_name"].(string); given != "" {
		p.DisplayName = cleanDisplayName(given)
	}
	return p, nil
}

func audienceOK(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, _ := a.(string); s == want {
				return true
			}
		}
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func rolesFrom(claims map[string]any, path string) []string {
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	switch v := cur.(type) {
	case string:
		return strings.Fields(v)
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func mfaUsed(amr any, accepted []string) bool {
	list, ok := amr.([]any)
	if !ok {
		return false
	}
	for _, a := range list {
		if s, ok := a.(string); ok && slices.Contains(accepted, s) {
			return true
		}
	}
	return false
}

// ---- JWKS ------------------------------------------------------------------------

// key returns the public key for kid. The JWKS is refreshed when the cache
// expires, and on an unknown kid (key rotation), but kid-triggered
// refreshes are limited to one per 30 seconds so junk tokens can't make us
// hammer the identity provider.
func (o *OIDC) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.cfg.Now()
	stale := o.keys == nil || now.Sub(o.fetchedAt) > o.cfg.CacheTTL
	_, known := o.keys[kid]
	refresh := stale
	if !stale && !known && now.Sub(o.lastRefresh) > 30*time.Second {
		o.lastRefresh = now // rate-limit kid-triggered refreshes only
		refresh = true
	}
	if refresh {
		if err := o.refresh(ctx); err != nil {
			if k, ok := o.keys[kid]; ok {
				o.cfg.Log.Warn("JWKS refresh failed; using cached key", "err", err)
				return k, nil
			}
			return nil, fmt.Errorf("%w: JWKS unavailable: %v", ErrUnauthenticated, err)
		}
	}
	if k, ok := o.keys[kid]; ok {
		return k, nil
	}
	return nil, fail("unknown key id")
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("GET %s: HTTP %d", url, res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}

func (o *OIDC) refresh(ctx context.Context) error {
	if o.jwksURL == "" {
		var disc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := o.getJSON(ctx, strings.TrimRight(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
			return err
		}
		if disc.Issuer != o.cfg.Issuer || !strings.HasPrefix(disc.JWKSURI, "https://") && !strings.HasPrefix(disc.JWKSURI, "http://127.0.0.1") {
			return errors.New("discovery document does not match the issuer or jwks_uri is not https")
		}
		o.jwksURL = disc.JWKSURI
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := o.getJSON(ctx, o.jwksURL, &set); err != nil {
		return err
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := b64(k.N)
			e, err2 := b64(k.E)
			if err1 != nil || err2 != nil || len(n) < 256 { // RSA >= 2048 bits
				continue
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := b64(k.X)
			y, err2 := b64(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
				continue
			}
			keys[k.Kid] = pub
		}
	}
	if len(keys) == 0 {
		return errors.New("JWKS has no usable signing keys")
	}
	o.keys, o.fetchedAt = keys, o.cfg.Now()
	return nil
}
