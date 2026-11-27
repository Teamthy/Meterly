package partners

import "testing"

func TestScopeMatrix(t *testing.T) {
	routes := RouteCatalog()
	if len(routes) == 0 {
		t.Fatal("empty catalog")
	}
	for _, have := range AllScopes {
		scopes := []string{have}
		for _, rt := range routes {
			got := HasScope(scopes, rt.Scope)
			want := rt.Scope == "" || rt.Scope == have
			if got != want {
				t.Errorf("have %s on %s %s: got %v want %v", have, rt.Method, rt.Path, got, want)
			}
		}
	}
	for _, rt := range routes {
		if rt.Scope == "" {
			if !HasScope(nil, rt.Scope) {
				t.Errorf("%s %s should allow empty scopes", rt.Method, rt.Path)
			}
			continue
		}
		if HasScope(nil, rt.Scope) || HasScope([]string{}, rt.Scope) {
			t.Errorf("%s %s allowed with no scopes", rt.Method, rt.Path)
		}
	}
	if HasScope([]string{ScopePurchasesCreate}, ScopePurchasesRead) {
		t.Fatal("create must not imply read")
	}
	if _, err := NormalizeScopes([]string{"nope"}); err != ErrInvalidScope {
		t.Fatalf("unknown: %v", err)
	}
	got, err := NormalizeScopes([]string{ScopeFloatRead, ScopeFloatRead, ScopeMetersValidate})
	if err != nil || len(got) != 2 {
		t.Fatalf("dedupe %v %v", got, err)
	}
}
