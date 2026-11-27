package reconcile_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func eachStore(t *testing.T, fn func(t *testing.T, st store.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, memstore.New()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("DATABASE_URL") == "" {
			t.Skip("DATABASE_URL not set")
		}
		fn(t, pgstore.New(pgtest.NewPool(t), "SANDBOX"))
	})
}

func TestClaimRunAndExceptionFingerprint(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		repo := st.Repos().Reconcile()
		date := time.Date(2026, 1, 15, 0, 0, 0, 0, reconcile.WAT)
		id := uuid.New()
		run, err := repo.ClaimRun(ctx, reconcile.ProviderPaystack, date, id)
		if err != nil || run.Status != reconcile.RunRunning {
			t.Fatalf("claim %+v %v", run, err)
		}
		ex := &reconcile.Exception{
			RunID: run.ID, Provider: reconcile.ProviderPaystack, BusinessDate: date,
			Category: reconcile.CatInPSPNotOurs, Severity: reconcile.SeverityMedium,
			ProviderRef: "ghost", Currency: "NGN", Status: reconcile.StatusOpen,
		}
		if err := repo.UpsertException(ctx, ex); err != nil {
			t.Fatal(err)
		}
		first := ex.ID
		ex2 := *ex
		ex2.ID = uuid.New()
		ex2.OursMinor = 9
		if err := repo.UpsertException(ctx, &ex2); err != nil || ex2.ID != first {
			t.Fatalf("fingerprint upsert %v id=%s want=%s", err, ex2.ID, first)
		}
		got, err := repo.GetException(ctx, first)
		if err != nil || got.OursMinor != 9 {
			t.Fatalf("updated %+v %v", got, err)
		}
		got.Status = reconcile.StatusResolved
		if err := repo.UpdateException(ctx, got); err != nil {
			t.Fatal(err)
		}
		ex3 := *ex
		ex3.ID = uuid.New()
		ex3.OursMinor = 99
		if err := repo.UpsertException(ctx, &ex3); err != nil || ex3.ID != first {
			t.Fatalf("must not reopen %v %s", err, ex3.ID)
		}
		still, _ := repo.GetException(ctx, first)
		if still.Status != reconcile.StatusResolved || still.OursMinor != 9 {
			t.Fatalf("resolved row mutated %+v", still)
		}

		run.MatchedCount = 3
		run.CompletedAt = time.Now().UTC()
		if err := repo.CompleteRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		again, err := repo.ClaimRun(ctx, reconcile.ProviderPaystack, date, uuid.New())
		if err != nil || again.Status != reconcile.RunCompleted || again.ID != run.ID {
			t.Fatalf("completed claim %+v %v", again, err)
		}
	})
}
