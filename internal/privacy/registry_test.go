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
