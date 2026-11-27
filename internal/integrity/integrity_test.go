package integrity_test

import (
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/integrity"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestEmptyStoreIsHealthy(t *testing.T) {
	st := memstore.New()
	rep, err := integrity.Check(t.Context(), st, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	found := map[string]bool{}
	for _, f := range rep.Findings {
		found[f.Name] = f.OK
	}
	if !found["trial_balance"] || !found["metrics_snapshot"] {
		t.Fatalf("findings %+v", rep.Findings)
	}
}
