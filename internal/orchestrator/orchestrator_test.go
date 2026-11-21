package orchestrator_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

// ---- fault injection ---------------------------------------------------------

var errCrash = errors.New("simulated crash")

// crashStore makes a chosen future InTx fail without running, simulating
// the process dying between a provider call and the following commit.
type crashStore struct {
	store.Store
	countdown atomic.Int32 // 0 = disarmed; n = fail the n-th next InTx
}

func (c *crashStore) arm(n int32) { c.countdown.Store(n) }

func (c *crashStore) InTx(ctx context.Context, fn func(store.Repos) error) error {
	if n := c.countdown.Load(); n > 0 {
		if c.countdown.Add(-1) == 0 {
			return errCrash
		}
	}
	return c.Store.InTx(ctx, fn)
}

// hookVending / hookPayments / hookRemote run a hook after the real
// provider call returns, e.g. to arm a crash.
type hookVending struct {
	providers.VendingProvider
	after func()
}

func (h *hookVending) Vend(ctx context.Context, r providers.VendRequest) (providers.VendResult, error) {
	res, err := h.VendingProvider.Vend(ctx, r)
	if h.after != nil {
		h.after()
	}
	return res, err
}

type hookPayments struct {
	providers.PaymentProvider
	afterVerify func()
}

func (h *hookPayments) VerifyPayment(ctx context.Context, ref string) (providers.PaymentVerification, error) {
	v, err := h.PaymentProvider.VerifyPayment(ctx, ref)
	if h.afterVerify != nil {
		h.afterVerify()
	}
	return v, err
}

type hookRemote struct {
	providers.RemoteCreditProvider
	after func()
}

func (h *hookRemote) InitiateCredit(ctx context.Context, r providers.RemoteCreditRequest) (providers.RemoteCreditResult, error) {
	res, err := h.RemoteCreditProvider.InitiateCredit(ctx, r)
	if h.after != nil {
		h.after()
	}
	return res, err
}

// ---- environment ---------------------------------------------------------------

type env struct {
	o      *orchestrator.Orchestrator
	st     *crashStore
	pay    *mock.Payments
	vend   *mock.Vending
	remote *mock.RemoteCredit
	hv     *hookVending
	hp     *hookPayments
	hr     *hookRemote
	ctx    context.Context
	users  map[string]string
	clock  time.Time
}

type backend struct {
	name string
	open func(t *testing.T) store.Store
}

var backends = []backend{
	{"memory", func(*testing.T) store.Store { return memstore.New() }},
	{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
}

// each runs fn against every store backend.
func each(t *testing.T, fn func(t *testing.T, e *env)) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) { fn(t, newEnv(t, b.open(t))) })
	}
}

func newEnv(t *testing.T, base store.Store) *env {
	t.Helper()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	e := &env{st: &crashStore{Store: base}, pay: pay, vend: vend, remote: remote, ctx: context.Background(), users: map[string]string{}}
	e.hv = &hookVending{VendingProvider: vend}
	e.hp = &hookPayments{PaymentProvider: pay}
	e.hr = &hookRemote{RemoteCreditProvider: remote}
	reg := registry.New(e.st, box, e.hv, e.hr)
	e.o = orchestrator.New(orchestrator.DefaultConfig(), e.st, reg, box, e.hp, e.hv, e.hr, nil)
	e.clock = time.Now().UTC()
	e.o.Now = func() time.Time { return e.clock }
	return e
}

// later moves the orchestrator clock forward (transactions' own
// timestamps stay on wall time, so they become "stale").
func (e *env) later(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *env) user(t *testing.T, name string) string {
	t.Helper()
	if id, ok := e.users[name]; ok {
		return id
	}
	id, err := e.st.Repos().Users().EnsureByExternalID(e.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	e.users[name] = id
	return id
}

func (e *env) register(t *testing.T, user, meter string) *meters.Meter {
	t.Helper()
	m, err := e.o.Meters.Register(e.ctx, e.user(t, user), meter, "IKEDC", "Home", meters.PermManage)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return m
}

// create registers the meter, quotes ₦5,000 and creates a transaction.
func (e *env) create(t *testing.T, user, meter string, pref orchestrator.DeliveryPreference) *tx.Transaction {
	t.Helper()
	m := e.register(t, user, meter)
	q, err := e.o.CreateQuote(e.ctx, e.user(t, user), m.ID, 500000, pref)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	txn, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: e.user(t, user), QuoteID: q.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if txn.Status() != tx.OverallPaymentPending || txn.Payment != tx.PayPending {
		t.Fatalf("want PAYMENT_PENDING, got %s/%s", txn.Status(), txn.Payment)
	}
	return txn
}

// buy creates a transaction and pays it.
func (e *env) buy(t *testing.T, user, meter string, pref orchestrator.DeliveryPreference) *tx.Transaction {
	t.Helper()
	txn := e.create(t, user, meter, pref)
	if err := e.pay.Complete(txn.PaymentProviderRef, true, 0); err != nil {
		t.Fatal(err)
	}
	got, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
	if err != nil {
		t.Fatalf("payment notification: %v", err)
	}
	return got
}

func (e *env) get(t *testing.T, id uuid.UUID) *tx.Transaction {
	t.Helper()
	txn, err := e.o.Get(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return txn
}

func (e *env) assertBalanced(t *testing.T) {
	t.Helper()
	tb, err := e.st.Repos().Ledger().TrialBalance(e.ctx)
	if err != nil || tb != 0 {
		t.Fatalf("ledger trial balance %d (%v), want 0", tb, err)
	}
}

func (e *env) balance(t *testing.T, a ledger.AccountCode) int64 {
	t.Helper()
	v, err := e.st.Repos().Ledger().Balance(e.ctx, a, money.NGN)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) postings(t *testing.T, id uuid.UUID) int {
	t.Helper()
	l, err := e.st.Repos().Ledger().BySource(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return len(l)
}

// ---- happy paths ---------------------------------------------------------------

func TestTokenHappyPath(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012345678", orchestrator.PrefAuto)
		if txn.Status() != tx.OverallCompleted || txn.DeliveryMethod != tx.MethodToken {
			t.Fatalf("got %s / %s", txn.Status(), txn.DeliveryMethod)
		}
		tok, _, err := e.o.Token(e.ctx, e.user(t, "u1"), txn.ID)
		if err != nil || len(tok) != 24 {
			t.Fatalf("token %q err %v", tok, err)
		}
		if _, _, err := e.o.Token(e.ctx, e.user(t, "someone-else"), txn.ID); err == nil {
			t.Fatal("other user must not read token")
		}
		e.assertBalanced(t)
		if got := e.balance(t, ledger.ProviderPayable); got != -500000 {
			t.Fatalf("provider payable %d", got)
		}
		if got := e.balance(t, ledger.PlatformFeeRevenue); got != -10000 {
			t.Fatalf("fee revenue %d", got)
		}
		// History is persisted in order with every transition.
		if n := len(e.get(t, txn.ID).History); n != 6 {
			t.Fatalf("history %d", n)
		}
		evs, _ := e.st.Repos().Outbox().ForAggregate(e.ctx, txn.ID)
		if len(evs) < 6 {
			t.Fatalf("outbox events %d", len(evs))
		}
	})
}

func TestRemoteAcceptedIsNotConfirmed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "71012345678", orchestrator.PrefAuto)
		if txn.DeliveryMethod != tx.MethodRemote || txn.Delivery != tx.DelAccepted {
			t.Fatalf("got %s/%s", txn.DeliveryMethod, txn.Delivery)
		}
		if txn.Status() == tx.OverallCompleted {
			t.Fatal("ACCEPTED must not count as completed")
		}
		if _, _, err := e.o.Token(e.ctx, e.user(t, "u1"), txn.ID); !errors.Is(err, orchestrator.ErrTokenUnavailable) {
			t.Fatalf("token should not be shown while remote delivery is in flight: %v", err)
		}
		e.remote.Settle()
		txn, _ = e.o.Resume(e.ctx, txn.ID)
		if txn.Status() != tx.OverallCompleted {
			t.Fatalf("got %s", txn.Status())
		}
		m, _ := e.o.Meters.Get(e.ctx, txn.MeterID)
		if !m.Capabilities.RemoteCredit.Confirmed {
			t.Fatal("meter remote credit should be marked confirmed")
		}
		e.assertBalanced(t)
	})
}

// ---- payment ------------------------------------------------------------------

func TestDuplicatePaymentWebhooksVendOnce(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef) }()
		}
		wg.Wait()
		if e.vend.Calls != 1 {
			t.Fatalf("vend called %d times", e.vend.Calls)
		}
		if got := e.get(t, txn.ID); got.Status() != tx.OverallCompleted {
			t.Fatalf("got %s", got.Status())
		}
		if n := e.postings(t, txn.ID); n != 2 {
			t.Fatalf("want 2 ledger postings, got %d", n)
		}
		e.assertBalanced(t)
	})
}

func TestPaymentFailureNeverVends(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, false, 0)
		txn, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
		if txn.Status() != tx.OverallFailed || e.vend.Calls != 0 {
			t.Fatalf("status %s, vend calls %d", txn.Status(), e.vend.Calls)
		}
	})
}

func TestUnderpaymentGoesToReviewAndNeverVends(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 100)
		_, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
		if !errors.Is(err, orchestrator.ErrPaymentMismatch) || e.vend.Calls != 0 {
			t.Fatalf("err %v, vend calls %d", err, e.vend.Calls)
		}
		got := e.get(t, txn.ID)
		if got.Status() != tx.OverallManualReview || got.ReviewReason == "" {
			t.Fatalf("got %s %q", got.Status(), got.ReviewReason)
		}
		// Further notifications and resumes do nothing.
		_, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
		_, _ = e.o.Resume(e.ctx, txn.ID)
		if e.vend.Calls != 0 {
			t.Fatal("must never vend a mismatched payment")
		}
	})
}

func TestMissedWebhookIsPickedUpBySweeper(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0) // paid, but no webhook arrives
		e.later(5 * time.Minute)
		if _, progressed, err := e.o.ResumeDue(e.ctx); err != nil || progressed != 1 {
			t.Fatalf("progressed %d err %v", progressed, err)
		}
		if got := e.get(t, txn.ID); got.Status() != tx.OverallCompleted {
			t.Fatalf("got %s", got.Status())
		}
	})
}

func TestUnpaidExpiresThenLatePaymentGoesToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		e.later(30 * time.Minute)
		_, _ = e.o.Resume(e.ctx, txn.ID)
		if got := e.get(t, txn.ID); got.Payment != tx.PayPending {
			t.Fatalf("must not expire early: %s", got.Payment)
		}
		e.later(3 * time.Hour)
		_, _ = e.o.Resume(e.ctx, txn.ID)
		if got := e.get(t, txn.ID); got.Status() != tx.OverallFailed {
			t.Fatalf("want expired FAILED, got %s", got.Status())
		}
		// The customer pays after all: money must not be silently dropped.
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
		got := e.get(t, txn.ID)
		if got.Status() != tx.OverallManualReview || e.vend.Calls != 0 {
			t.Fatalf("got %s, vend calls %d", got.Status(), e.vend.Calls)
		}
	})
}

// ---- vending ---------------------------------------------------------------------

func TestVendFailureRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012340000", orchestrator.PrefToken)
		if txn.Status() != tx.OverallRefunded {
			t.Fatalf("got %s", txn.Status())
		}
		if len(e.pay.Refunds) != 1 || e.pay.Refunds[0].Amount.Minor != 510000 {
			t.Fatalf("refunds %+v", e.pay.Refunds)
		}
		e.assertBalanced(t)
		if b := e.balance(t, ledger.PSPSettlementReceivable); b != 0 {
			t.Fatalf("PSP receivable should net to zero after refund, got %d", b)
		}
	})
}

func TestVendTimeoutButVendedIsReconciledNotRefunded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012349999", orchestrator.PrefToken)
		if txn.Vending != tx.VendUnknown || txn.Status() != tx.OverallReconciliation {
			t.Fatalf("timeout must be UNKNOWN, got %s/%s", txn.Vending, txn.Status())
		}
		if len(e.pay.Refunds) != 0 {
			t.Fatal("must not refund on ambiguous timeout")
		}
		calls := e.vend.Calls
		txn, err := e.o.Resume(e.ctx, txn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if txn.Status() != tx.OverallCompleted || e.vend.Calls != calls {
			t.Fatalf("got %s; reconciliation must query status, not re-vend (%d extra)", txn.Status(), e.vend.Calls-calls)
		}
		e.assertBalanced(t)
	})
}

func TestVendTimeoutNotVendedIsSafelyRetried(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012348888", orchestrator.PrefToken)
		if txn.Vending != tx.VendUnknown {
			t.Fatalf("got %s", txn.Vending)
		}
		txn, _ = e.o.Resume(e.ctx, txn.ID)
		// mock 8888 always times out, so after a confirmed NOT_FOUND we retried
		// once and are UNKNOWN again — never FAILED, never refunded.
		if txn.Vending != tx.VendUnknown || len(e.pay.Refunds) != 0 || e.vend.Calls != 2 {
			t.Fatalf("got %s, refunds %d, calls %d", txn.Vending, len(e.pay.Refunds), e.vend.Calls)
		}
	})
}

// ---- remote delivery ------------------------------------------------------------------

func TestRemoteTimeoutIsUnknownAndNotResent(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "77012345678", orchestrator.PrefRemote)
		if txn.Delivery != tx.DelUnknown {
			t.Fatalf("got %s", txn.Delivery)
		}
		calls := e.remote.Calls
		txn, _ = e.o.Resume(e.ctx, txn.ID)
		if txn.Status() != tx.OverallCompleted || e.remote.Calls != calls {
			t.Fatalf("got %s, resent %d", txn.Status(), e.remote.Calls-calls)
		}
	})
}

func TestRemoteFailureThenTokenFallback(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "76012345678", orchestrator.PrefRemote)
		e.remote.FailPending()
		txn, _ = e.o.Resume(e.ctx, txn.ID)
		if txn.Status() != tx.OverallManualReview {
			t.Fatalf("got %s", txn.Status())
		}
		if len(e.pay.Refunds) != 0 {
			t.Fatal("electricity was vended; must not auto-refund")
		}
		txn, err := e.o.FallbackToToken(e.ctx, txn.ID, audit.Actor{UserID: e.user(t, "ops"), Type: audit.ActorOperator, Reason: "remote delivery failed"})
		if err != nil || txn.Status() != tx.OverallCompleted {
			t.Fatalf("fallback: %v", err)
		}
		if _, _, err := e.o.Token(e.ctx, e.user(t, "u1"), txn.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRemoteNotEligible(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		m := e.register(t, "u1", "45012345678")
		if _, err := e.o.CreateQuote(e.ctx, e.user(t, "u1"), m.ID, 500000, orchestrator.PrefRemote); !errors.Is(err, orchestrator.ErrNotEligible) {
			t.Fatalf("got %v", err)
		}
	})
}

// ---- crash safety --------------------------------------------------------------------

// The process dies after the provider vended but before the result was
// committed. Resume must find the vend by its idempotency key, not vend again.
func TestCrashAfterVendBeforeCommit(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.hv.after = func() { e.st.arm(1); e.hv.after = nil }
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); !errors.Is(err, errCrash) {
			t.Fatalf("want simulated crash, got %v", err)
		}
		if got := e.get(t, txn.ID); got.Vending != tx.VendPending {
			t.Fatalf("committed state should be vend PENDING, got %s", got.Vending)
		}
		e.later(5 * time.Minute)
		if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, txn.ID)
		if got.Status() != tx.OverallCompleted || e.vend.Calls != 1 {
			t.Fatalf("got %s with %d vend calls", got.Status(), e.vend.Calls)
		}
		if n := e.postings(t, txn.ID); n != 2 {
			t.Fatalf("postings %d", n)
		}
		e.assertBalanced(t)
	})
}

// The process dies after payment was confirmed but before vending was
// claimed. Resume must vend exactly once.
func TestCrashAfterPaymentConfirmed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.hp.afterVerify = func() { e.st.arm(2); e.hp.afterVerify = nil } // 1st InTx confirms, 2nd (vend claim) crashes
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); !errors.Is(err, errCrash) {
			t.Fatalf("want simulated crash, got %v", err)
		}
		if got := e.get(t, txn.ID); got.Payment != tx.PayConfirmed || got.Vending != tx.VendNotStarted {
			t.Fatalf("got %s/%s", got.Payment, got.Vending)
		}
		e.later(5 * time.Minute)
		_, _, _ = e.o.ResumeDue(e.ctx)
		if got := e.get(t, txn.ID); got.Status() != tx.OverallCompleted || e.vend.Calls != 1 {
			t.Fatalf("got %s, vend calls %d", got.Status(), e.vend.Calls)
		}
		e.assertBalanced(t)
	})
}

// The process dies after the HES accepted the credit command but before we
// recorded it. Resume must query the HES, not send a second command.
func TestCrashAfterRemoteSend(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.hr.after = func() { e.st.arm(1); e.hr.after = nil }
		txn := e.create(t, "u1", "71012345678", orchestrator.PrefRemote)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); !errors.Is(err, errCrash) {
			t.Fatalf("want simulated crash, got %v", err)
		}
		if got := e.get(t, txn.ID); got.Delivery != tx.DelPending {
			t.Fatalf("got %s", got.Delivery)
		}
		e.remote.Settle()
		e.later(5 * time.Minute)
		_, _, _ = e.o.ResumeDue(e.ctx)
		if got := e.get(t, txn.ID); got.Status() != tx.OverallCompleted || e.remote.Calls != 1 {
			t.Fatalf("got %s, credit calls %d", got.Status(), e.remote.Calls)
		}
	})
}

// A failed unit of work must leave no partial writes: no ledger posting
// without its state change.
func TestFailedCommitWritesNothing(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.hp.afterVerify = func() { e.st.arm(1); e.hp.afterVerify = nil } // the payment-confirm commit itself fails
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
		if got := e.get(t, txn.ID); got.Payment != tx.PayPending {
			t.Fatalf("got %s", got.Payment)
		}
		if n := e.postings(t, txn.ID); n != 0 {
			t.Fatalf("ledger has %d postings for an uncommitted change", n)
		}
	})
}

// ---- resume worker -------------------------------------------------------------------

func TestResumeEscalatesAfterMaxAttempts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012348888", orchestrator.PrefToken) // vend never resolves
		for i := 0; i < 40; i++ {
			e.later(time.Hour)
			if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
				t.Fatal(err)
			}
		}
		got := e.get(t, txn.ID)
		if got.Status() != tx.OverallManualReview || got.CheckAttempts != orchestrator.DefaultConfig().ResumeMaxAttempts {
			t.Fatalf("got %s after %d attempts (%q)", got.Status(), got.CheckAttempts, got.ReviewReason)
		}
		if len(e.pay.Refunds) != 0 {
			t.Fatal("must never refund an unresolved vend")
		}
	})
}

func TestResumeDueSkipsFreshAndSettled(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		done := e.buy(t, "u1", "45012345678", orchestrator.PrefToken)
		pending := e.create(t, "u2", "45012345679", orchestrator.PrefToken)
		_ = done
		if n, _, _ := e.o.ResumeDue(e.ctx); n != 0 {
			t.Fatalf("fresh transactions must not be claimed, got %d", n)
		}
		e.later(5 * time.Minute)
		if n, _, _ := e.o.ResumeDue(e.ctx); n != 1 {
			t.Fatalf("want only the unsettled one, got %d", n)
		}
		// Backoff: an immediate second sweep finds nothing due.
		if n, _, _ := e.o.ResumeDue(e.ctx); n != 0 {
			t.Fatalf("want backoff, got %d", n)
		}
		if got := e.get(t, pending.ID); got.CheckAttempts != 1 {
			t.Fatalf("attempts %d", got.CheckAttempts)
		}
	})
}

// ---- idempotency, quotes, access --------------------------------------------------------

func TestIdempotentCreate(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		u := e.user(t, "u1")
		m := e.register(t, "u1", "45012345678")
		q, _ := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefToken)
		req := orchestrator.CreateRequest{CustomerID: u, QuoteID: q.ID, IdempotencyKey: "key-0123456789abcdef", RequestHash: "h1"}

		var wg sync.WaitGroup
		ids := make([]uuid.UUID, 8)
		errs := make([]error, 8)
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				t, _, err := e.o.CreateTransaction(e.ctx, req)
				errs[i] = err
				if t != nil {
					ids[i] = t.ID
				}
			}(i)
		}
		wg.Wait()
		for i := range ids {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("request %d: id %s err %v (want %s)", i, ids[i], errs[i], ids[0])
			}
		}
		req.RequestHash = "different"
		if _, _, err := e.o.CreateTransaction(e.ctx, req); !errors.Is(err, idempotency.ErrConflict) {
			t.Fatalf("want conflict, got %v", err)
		}
		txns, _ := e.o.ListForCustomer(e.ctx, u)
		if len(txns) != 1 {
			t.Fatalf("want 1 transaction, got %d", len(txns))
		}
	})
}

func TestFailedCreateReleasesIdempotencyKey(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		u := e.user(t, "u1")
		req := orchestrator.CreateRequest{CustomerID: u, QuoteID: uuid.New(), IdempotencyKey: "key-0123456789abcdef", RequestHash: "h"}
		if _, _, err := e.o.CreateTransaction(e.ctx, req); !errors.Is(err, orchestrator.ErrQuoteNotFound) {
			t.Fatalf("got %v", err)
		}
		m := e.register(t, "u1", "45012345678")
		q, _ := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefToken)
		req.QuoteID = q.ID
		if _, _, err := e.o.CreateTransaction(e.ctx, req); err != nil {
			t.Fatalf("key should be reusable after a failed attempt: %v", err)
		}
	})
}

func TestAccessControl(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		m := e.register(t, "owner", "45012345678")
		if _, err := e.o.CreateQuote(e.ctx, e.user(t, "stranger"), m.ID, 500000, orchestrator.PrefAuto); !errors.Is(err, meters.ErrForbidden) {
			t.Fatalf("stranger quote: %v", err)
		}
		member := e.user(t, "member")
		if _, err := e.o.Meters.Register(e.ctx, member, "45012345678", "IKEDC", "Mum", meters.PermPurchase); err != nil {
			t.Fatal(err)
		}
		q, err := e.o.CreateQuote(e.ctx, member, m.ID, 500000, orchestrator.PrefAuto)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.o.Meters.Revoke(e.ctx, m.ID, member); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: member, QuoteID: q.ID}); !errors.Is(err, meters.ErrForbidden) {
			t.Fatalf("revoked purchase: %v", err)
		}
		// The rejected attempt must not consume the quote or leave a transaction.
		if txns, _ := e.o.ListForCustomer(e.ctx, member); len(txns) != 0 {
			t.Fatalf("got %d transactions", len(txns))
		}
		list, _ := e.o.Meters.ListForUser(e.ctx, e.user(t, "owner"))
		if len(list) != 1 || list[0].Permission != meters.PermManage {
			t.Fatalf("owner list %+v", list)
		}
	})
}

func TestQuoteSingleUseAndLimits(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		u := e.user(t, "u1")
		m := e.register(t, "u1", "45012345678")
		if _, err := e.o.CreateQuote(e.ctx, u, m.ID, 100, orchestrator.PrefAuto); !errors.Is(err, orchestrator.ErrAmountOutOfRange) {
			t.Fatalf("got %v", err)
		}
		if _, err := e.o.CreateQuote(e.ctx, u, m.ID, 500000, "SOMETHING"); !errors.Is(err, orchestrator.ErrInvalidPreference) {
			t.Fatalf("got %v", err)
		}
		q, _ := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefAuto)
		if _, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: q.ID}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: q.ID}); !errors.Is(err, orchestrator.ErrQuoteExpired) {
			t.Fatalf("reused quote: %v", err)
		}
		q2, _ := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefAuto)
		e.later(time.Hour)
		if _, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: q2.ID}); !errors.Is(err, orchestrator.ErrQuoteExpired) {
			t.Fatalf("expired quote: %v", err)
		}
	})
}

func TestInvalidMeterRejected(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if _, err := e.o.Meters.Register(e.ctx, e.user(t, "u1"), "01234567890", "IKEDC", "", meters.PermManage); !errors.Is(err, meters.ErrInvalidMeter) {
			t.Fatalf("got %v", err)
		}
	})
}
