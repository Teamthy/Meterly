package privacy_test

import (
	"testing"

	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/migrations"
)

func TestColumnRegistryMatchesMigrations(t *testing.T) {
	got, err := privacy.PIILikeKeys(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, c := range privacy.Columns() {
		k := c.Key()
		if want[k] {
			t.Errorf("duplicate registry entry %s", k)
		}
		want[k] = true
	}
	gotSet := map[string]bool{}
	for _, k := range got {
		gotSet[k] = true
		if !want[k] {
			t.Errorf("migrations have PII-like column %s with no registry entry in internal/privacy", k)
		}
	}
	for k := range want {
		if !gotSet[k] {
			t.Errorf("registry has stale entry %s (not in final schema)", k)
		}
	}
	if t.Failed() {
		t.Logf("schema keys: %v", got)
	}
}

func TestPersonalColumnsHaveErasureAction(t *testing.T) {
	actions := privacy.ErasureActions()
	allowed := map[string]bool{
		privacy.EraseDelete: true, privacy.EraseAnonymise: true,
		privacy.EraseShared: true, privacy.EraseKeep: true,
	}
	for _, c := range privacy.Columns() {
		if !c.Personal {
			continue
		}
		a := actions[c.Key()]
		if a == "" {
			t.Errorf("personal column %s has no erasure action", c.Key())
			continue
		}
		if !allowed[a] {
			t.Errorf("%s erasure action %q is not DELETE/ANONYMISE/SHARED/KEEP", c.Key(), a)
		}
	}
}

func TestPoliciesCoverFinancialAsAnonymise(t *testing.T) {
	var sawFinancial bool
	for _, p := range privacy.Policies(privacy.DefaultSettings()) {
		if p.Financial && p.Action != privacy.ActionAnonymise {
			t.Errorf("%s is financial but action=%s", p.Table, p.Action)
		}
		if p.Table == "electricity_transactions" || p.Table == "payments" || p.Table == "ledger_entries" {
			sawFinancial = true
			if p.Action != privacy.ActionAnonymise {
				t.Errorf("%s must be ANONYMISE in B1", p.Table)
			}
		}
	}
	if !sawFinancial {
		t.Fatal("expected financial table policies")
	}
}
