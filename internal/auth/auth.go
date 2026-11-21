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
	"slices"
	"strings"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleOperator Role = "operator" // investigate, reconcile, release tokens
	RoleAdmin    Role = "admin"    // plus refunds from review, provider switches
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string // identity-provider subject; mapped to users.external_id
	Roles   []Role
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
	p := Principal{Subject: tok}
	switch {
	case strings.HasPrefix(tok, "admin:"):
		p.Roles = []Role{RoleAdmin}
	case strings.HasPrefix(tok, "operator:"):
		p.Roles = []Role{RoleOperator}
	}
	return p, nil
}
