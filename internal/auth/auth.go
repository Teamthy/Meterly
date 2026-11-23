// Package auth authenticates API callers and models their roles.
//
// Production uses OIDC: bearer JWTs from the configured issuer, verified
// against its JWKS (see oidc.go). Dev uses DevAuth, where the bearer token
// is the subject. Roles come from a configurable claim; staff roles
// (operator, admin) are honoured only when the token shows MFA was used.
package auth

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleOperator Role = "operator" // investigate, reconcile, release tokens
	RoleAdmin    Role = "admin"    // plus refunds from review, provider switches
)

// Principal is an authenticated caller. VerifiedEmail and VerifiedPhone are
// populated only when the identity provider attested verification
// (email_verified=true / phone_number_verified=true), or via dev simulation
// headers in MODE=dev.
type Principal struct {
	Subject       string // identity-provider subject; mapped to users.external_id
	Roles         []Role
	VerifiedEmail string // sensitive: never log
	VerifiedPhone string // sensitive E.164: never log
	DisplayName   string
	DevMode       bool
}

// Has reports whether p holds role r. Admin implies operator; everyone
// authenticated is a customer.
func (p Principal) Has(r Role) bool {
	switch r {
	case RoleCustomer:
		return true
	case RoleOperator:
		return slices.Contains(p.Roles, RoleOperator) || slices.Contains(p.Roles, RoleAdmin)
	}
	return slices.Contains(p.Roles, r)
}

var ErrUnauthenticated = errors.New("auth: unauthenticated")

type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

func bearer(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(tok), ok && strings.TrimSpace(tok) != ""
}

// DevAuth treats the bearer token as the subject. Development only.
// Subjects starting with "operator:" or "admin:" get that staff role, so
// local testing of the admin API needs no identity provider.
type DevAuth struct{}

func (DevAuth) Authenticate(r *http.Request) (Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	p := Principal{Subject: tok, DevMode: true}
	name := tok
	switch {
	case strings.HasPrefix(tok, "admin:"):
		p.Roles = []Role{RoleAdmin}
		name = strings.TrimPrefix(tok, "admin:")
	case strings.HasPrefix(tok, "operator:"):
		p.Roles = []Role{RoleOperator}
		name = strings.TrimPrefix(tok, "operator:")
	}
	p.DisplayName = cleanDisplayName(name)
	if dn := cleanDisplayName(r.Header.Get("X-Dev-Display-Name")); dn != "" {
		p.DisplayName = dn
	}
	if em, ok := cleanVerifiedEmail(r.Header.Get("X-Dev-Verified-Email")); ok {
		p.VerifiedEmail = em
	}
	if ph, ok := cleanVerifiedPhone(r.Header.Get("X-Dev-Verified-Phone")); ok {
		p.VerifiedPhone = ph
	}
	return p, nil
}

var e164ClaimRe = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func cleanVerifiedEmail(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if len(s) < 3 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return "", false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at >= len(s)-1 || !strings.Contains(s[at+1:], ".") {
		return "", false
	}
	return s, true
}

func cleanVerifiedPhone(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if !e164ClaimRe.MatchString(s) {
		return "", false
	}
	return s, true
}

func cleanDisplayName(raw string) string {
	var b strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		count++
		if count >= 64 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
