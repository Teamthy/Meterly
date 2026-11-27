package pgstore_test

import (
	"testing"

	"github.com/teamthy/meterly/internal/pilot"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestPilotAllowlist(t *testing.T) {
	st := pgstore.New(pgtest.NewPool(t), "SANDBOX")
	ctx := t.Context()
	repo := st.Repos().Pilot()
	if err := repo.Add(ctx, pilot.Entry{Kind: pilot.KindUser, Value: "user-1", Reason: "cohort"}); err != nil {
		t.Fatal(err)
	}
	ok, err := repo.Has(ctx, pilot.KindUser, "user-1")
	if err != nil || !ok {
		t.Fatalf("has %v %v", ok, err)
	}
	n, err := repo.CountKind(ctx, pilot.KindUser)
	if err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
	if err := repo.Add(ctx, pilot.Entry{Kind: pilot.KindUser, Value: "user-1"}); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx, pilot.KindUser)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err := repo.Remove(ctx, pilot.KindUser, "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Remove(ctx, pilot.KindUser, "user-1"); err != pilot.ErrNotFound {
		t.Fatalf("second remove %v", err)
	}
}
