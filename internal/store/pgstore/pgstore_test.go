package pgstore_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

func TestDatabaseRejectsUnbalancedAndEditedLedger(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	// Bypass the Go validation to prove the database enforces balance.
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, reference, source_type, source_id) VALUES ($1, 'x', 't', $1)`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, direction, amount_minor, currency)
			SELECT $1, id, 'DEBIT', 100, 'NGN' FROM ledger_accounts WHERE account_code = 'PSP_SETTLEMENT_RECEIVABLE'`, id)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "does not balance") {
		t.Fatalf("want balance violation at commit, got %v", err)
	}

	st := pgstore.New(pool, "SANDBOX")
	ngn := money.New(500, money.NGN)
	posted, err := st.Repos().Ledger().Post(ctx, ledger.Transaction{Reference: "r1", SourceType: "t", SourceID: uuid.New(), Entries: []ledger.Entry{
		{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: ngn},
		{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: ngn},
	}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.Repos().Ledger().Post(ctx, ledger.Transaction{Reference: "r1", SourceType: "t", SourceID: uuid.New(), Entries: posted.Entries})
	if err != nil || again.ID != posted.ID {
		t.Fatalf("re-post must return the original: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET amount_minor = 1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("want immutability error, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("delete must be rejected")
	}
}

func TestOutboxClaimSkipsLockedRows(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := pgstore.New(pool, "SANDBOX")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := st.Repos().Outbox().Add(ctx, events.New("test", uuid.New(), map[string]any{"i": i})); err != nil {
			t.Fatal(err)
		}
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- st.InTx(ctx, func(r store.Repos) error {
			evs, err := r.Outbox().ClaimUnpublished(ctx, 2)
			if err != nil || len(evs) != 2 {
				return errors.New("first claim should get 2")
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := st.InTx(ctx, func(r store.Repos) error {
		evs, err := r.Outbox().ClaimUnpublished(ctx, 10)
		if err != nil {
			return err
		}
		if len(evs) != 1 {
			t.Errorf("second worker should only see the 1 unlocked event, got %d", len(evs))
		}
		return nil
	})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyExpiredKeyIsReplaced(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := pgstore.New(pool, "SANDBOX")
	ctx := context.Background()
	past := time.Now().Add(-2 * time.Hour)
	rec := idempotency.Record{Scope: "s", Operation: "op", Key: "k", RequestHash: "h1", CreatedAt: past, ExpiresAt: past.Add(time.Hour)}
	if ex, err := st.Repos().Idempotency().Reserve(ctx, rec); err != nil || ex != nil {
		t.Fatalf("%v %v", ex, err)
	}
	rec.RequestHash, rec.CreatedAt, rec.ExpiresAt = "h2", time.Now(), time.Now().Add(time.Hour)
	if ex, err := st.Repos().Idempotency().Reserve(ctx, rec); err != nil || ex != nil {
		t.Fatalf("expired key should be replaced: %v %v", ex, err)
	}
	if ex, _ := st.Repos().Idempotency().Reserve(ctx, rec); ex == nil || ex.RequestHash != "h2" {
		t.Fatalf("live key should be returned, got %+v", ex)
	}
}

// Ordinary requests bind a session's network only once; only a passkey
// step-up may move it.
func TestSessionNetworkBindingOnlyMovesOnStepUp(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := pgstore.New(pool, "SANDBOX")
	ctx := context.Background()
	uid, err := st.Repos().Users().EnsureByExternalID(ctx, "binding-user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sess := identity.Session{ID: uuid.New(), UserID: uid, TokenHMAC: "tok-hmac-binding", ExpiresAt: now.Add(time.Hour), LastSeenAt: now, CreatedAt: now}
	auth := st.Repos().Auth()
	if err := auth.InsertSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ip := func() string {
		t.Helper()
		got, err := auth.GetSession(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.IPHMAC
	}
	if err := auth.TouchSession(ctx, sess.ID, "net-A", "ua", now); err != nil {
		t.Fatal(err)
	}
	if err := auth.TouchSession(ctx, sess.ID, "net-B", "ua", now); err != nil {
		t.Fatal(err)
	}
	if got := ip(); got != "net-A" {
		t.Fatalf("a plain request moved the bound network to %q", got)
	}
	if err := auth.MarkSessionStepUp(ctx, sess.ID, "net-B", now); err != nil {
		t.Fatal(err)
	}
	if got := ip(); got != "net-B" {
		t.Fatalf("step-up did not re-bind the network, got %q", got)
	}
}
