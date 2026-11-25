package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/authz"
	"github.com/teamthy/meterly/internal/powerdesk"
)

func TestRequireZeroIDORAndRoleMatrix(t *testing.T) {
	orgA := uuid.New()
	orgB := uuid.New()

	members := map[uuid.UUID]map[string]powerdesk.Role{
		orgA: {
			"u-owner":   powerdesk.RoleOwner,
			"u-manager": powerdesk.RoleManager,
			"u-viewer":  powerdesk.RoleViewer,
		},
		orgB: {
			"u-other": powerdesk.RoleOwner,
		},
	}
	lookup := func(_ context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error) {
		if m, ok := members[orgID]; ok {
			if role, ok := m[userID]; ok {
				return role, nil
			}
		}
		return "", authz.ErrNotFound
	}

	if err := authz.Require(context.Background(), orgA, powerdesk.RoleViewer); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}

	// Cross-organisation access must always return ErrNotFound (404), never ErrForbidden (403).
	for _, uid := range []string{"u-owner", "u-manager", "u-viewer"} {
		ctx := authz.WithCaller(context.Background(), uid, lookup)
		for _, want := range []powerdesk.Role{powerdesk.RoleViewer, powerdesk.RoleManager, powerdesk.RoleOwner} {
			if err := authz.Require(ctx, orgB, want); !errors.Is(err, authz.ErrNotFound) {
				t.Fatalf("user %s accessing orgB (%s) got %v, want ErrNotFound", uid, want, err)
			}
		}
	}

	// Intra-organisation role matrix: insufficient rank returns ErrForbidden (403).
	ctxViewer := authz.WithCaller(context.Background(), "u-viewer", lookup)
	if err := authz.Require(ctxViewer, orgA, powerdesk.RoleViewer); err != nil {
		t.Fatalf("viewer reading orgA failed: %v", err)
	}
	if err := authz.Require(ctxViewer, orgA, powerdesk.RoleManager); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("viewer acting as manager got %v, want ErrForbidden", err)
	}

	ctxManager := authz.WithCaller(context.Background(), "u-manager", lookup)
	if got, err := authz.RequireRole(ctxManager, orgA, powerdesk.RoleManager); err != nil || got != powerdesk.RoleManager {
		t.Fatalf("manager acting as manager got (%s, %v)", got, err)
	}
	if err := authz.Require(ctxManager, orgA, powerdesk.RoleOwner); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("manager acting as owner got %v, want ErrForbidden", err)
	}

	ctxOwner := authz.WithCaller(context.Background(), "u-owner", lookup)
	if got, err := authz.RequireRole(ctxOwner, orgA, powerdesk.RoleViewer); err != nil || got != powerdesk.RoleOwner {
		t.Fatalf("owner acting as viewer got (%s, %v)", got, err)
	}
}
