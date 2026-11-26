package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	"github.com/teamthy/meterly/internal/support"
)

func TestSupportRepoParity(t *testing.T) {
	cases := []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memstore", func(*testing.T) store.Store { return memstore.New() }},
		{"pgstore", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runSupportParity(t, tc.open(t)) })
	}
}

func runSupportParity(t *testing.T, st store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cust, err := st.Repos().Users().EnsureByExternalID(ctx, "cust-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.Repos().Users().EnsureByExternalID(ctx, "op-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Repos().Support().UpsertIdentityKey(ctx, support.IdentityKey{
		UserID: op, PublicKey: bytesOf(91, 7), Algorithm: support.AlgorithmP256, Staff: true, CreatedAt: now, RotatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	staff, err := st.Repos().Support().ListStaffKeys(ctx)
	if err != nil || len(staff) != 1 || staff[0].UserID != op {
		t.Fatalf("staff keys: %v %v", staff, err)
	}

	th := &support.Thread{
		ID: uuid.New(), CustomerID: cust, Mode: support.ModeAgent, Status: support.StatusOpen,
		CreatedAt: now, UpdatedAt: now, LastMessageAt: now,
	}
	if err := st.InTx(ctx, func(repos store.Repos) error {
		if err := repos.Support().CreateThread(ctx, th); err != nil {
			return err
		}
		return repos.Support().AddMessage(ctx, &support.Message{
			ID: uuid.New(), ThreadID: th.ID, SenderID: "agent", SenderKind: support.SenderAgent,
			BodyEnc: bytesOf(32, 1), CreatedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Repos().Support().GetThread(ctx, th.ID)
	if err != nil || got.CustomerID != cust {
		t.Fatalf("get thread: %v %v", got, err)
	}
	list, err := st.Repos().Support().ListMessages(ctx, th.ID, time.Time{}, 10)
	if err != nil || len(list) != 1 || list[0].EndToEnd() {
		t.Fatalf("messages: %v %v", list, err)
	}

	// Rollback must drop the wrap.
	if err := st.InTx(ctx, func(repos store.Repos) error {
		if err := repos.Support().UpsertWrappedKey(ctx, support.WrappedKey{
			ThreadID: th.ID, HolderUserID: cust, WrappedKey: bytesOf(40, 2), Nonce: bytesOf(12, 3), CreatedAt: now,
		}); err != nil {
			return err
		}
		return store.ErrConflict
	}); err != store.ErrConflict {
		t.Fatalf("want conflict rollback, got %v", err)
	}
	if _, err := st.Repos().Support().GetWrappedKey(ctx, th.ID, cust); err != store.ErrNotFound {
		t.Fatalf("wrap survived rollback: %v", err)
	}

	if err := st.Repos().Support().UpsertWrappedKey(ctx, support.WrappedKey{
		ThreadID: th.ID, HolderUserID: op, WrappedKey: bytesOf(40, 9), Nonce: bytesOf(12, 8), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	wraps, err := st.Repos().Support().ListWrappedKeys(ctx, th.ID)
	if err != nil || len(wraps) != 1 || wraps[0].HolderUserID != op {
		t.Fatalf("wraps: %v %v", wraps, err)
	}
}

func bytesOf(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}
