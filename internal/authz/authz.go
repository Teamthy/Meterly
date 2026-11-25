// Package authz enforces organisation-scoped RBAC (OWNER, MANAGER, VIEWER) in
// a single helper (`authz.Require(ctx, orgID, minRole)`).
//
// Zero-IDOR invariant:
//   - If the caller is not an active member of `orgID`, `Require` returns
//     `ErrNotFound` (mapped to HTTP 404), never revealing whether `orgID` or
//     its child resources exist.
//   - Only when active membership in `orgID` is proven and the member's role
//     ranks below `minRole` does `Require` return `ErrForbidden` (HTTP 403).
package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/powerdesk"
)

var (
	ErrUnauthenticated = errors.New("authz: caller missing from context")
	ErrNotFound        = errors.New("authz: organisation not found")
	ErrForbidden       = errors.New("authz: insufficient organisation role")
)

// LookupFunc returns the active organisation role held by userID in orgID.
// When userID is not an active member of orgID, LookupFunc must return
// ErrNotFound (or any error wrapping ErrNotFound).
type LookupFunc func(ctx context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error)

type callerScope struct {
	userID string
	lookup LookupFunc
}

type scopeKey struct{}

// WithCaller binds the authenticated user ID and organisation membership
// lookup to ctx so downstream service methods can call authz.Require(ctx, orgID, role).
func WithCaller(ctx context.Context, userID string, lookup LookupFunc) context.Context {
	return context.WithValue(ctx, scopeKey{}, callerScope{
		userID: userID,
		lookup: lookup,
	})
}

// CallerUserID returns the user ID bound via WithCaller, if present.
func CallerUserID(ctx context.Context) (string, bool) {
	sc, ok := ctx.Value(scopeKey{}).(callerScope)
	return sc.userID, ok && sc.userID != ""
}

// Require verifies that the caller bound to ctx is an active member of orgID
// holding at least minRole. Non-members receive ErrNotFound (404); members
// with insufficient rank receive ErrForbidden (403).
func Require(ctx context.Context, orgID uuid.UUID, minRole powerdesk.Role) error {
	_, err := RequireRole(ctx, orgID, minRole)
	return err
}

// RequireRole is identical to Require and also returns the caller's actual
// organisation role (useful when business rules branch between OWNER and MANAGER).
func RequireRole(ctx context.Context, orgID uuid.UUID, minRole powerdesk.Role) (powerdesk.Role, error) {
	if !minRole.Valid() {
		return "", powerdesk.ErrInvalidRole
	}
	sc, ok := ctx.Value(scopeKey{}).(callerScope)
	if !ok || sc.userID == "" || sc.lookup == nil {
		return "", ErrUnauthenticated
	}
	if orgID == uuid.Nil {
		return "", ErrNotFound
	}
	held, err := sc.lookup(ctx, orgID, sc.userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !held.Allows(minRole) {
		return "", ErrForbidden
	}
	return held, nil
}
