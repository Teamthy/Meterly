package orchestrator_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/secure"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type env struct {
	o      *orchestrator.Orchestrator
	pay    *mock.Payments
	vend   *mock.Vending
	remote *mock.RemoteCredit
	ctx    context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	reg := meters.NewRegistry(box, vend, remote)
	o := orchestrator.New(orchestrator.DefaultConfig(), reg, tx.NewStore(), ledger.New(), events.NewOutbox(), box, pay, vend, remote, nil)
	return &env{o: o, pay: pay, vend: vend, remote: remote, ctx: context.Background()}
}

// buy registers meter, quotes ₦5,000, creates a transaction and pays.
func (e *env) buy(t *testing.T, user, meter string, pref orchestrator.DeliveryPreference) *tx.Transaction {
	t.Helper()
	m, err := e.o.Meters.Register(e.ctx, user, meter, "IKEDC", "Home", meters.PermManage)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	q, err := e.o.CreateQuote(user, m.ID, 500000, pref)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	txn, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: user, QuoteID: q.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if txn.Status() != tx.OverallPaymentPending {
		t.Fatalf("want PAYMENT_PENDING, got %s", txn.Status())
	}
	if err := e.pay.Complete(txn.PaymentProviderRef, true, 0); err != nil {
		t.Fatal(err)
	}
	txn, err = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
	if err != nil {
		t.Fatalf("payment notification: %v", err)
	}
	return txn
}

func (e *env) assertBalanced(t *testing.T) {
	t.Helper()
	if tb := e.o.Ledger.TrialBalance(); tb != 0 {
		t.Fatalf("ledger trial balance %d, want 0", tb)
	}
}

func TestTokenHappyPath(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "45012345678", orchestrator.PrefAuto)
	if txn.Status() != tx.OverallCompleted || txn.DeliveryMethod != tx.MethodToken {
		t.Fatalf("got %s / %s", txn.Status(), txn.DeliveryMethod)
	}
	tok, _, err := e.o.Token("u1", txn.ID)
	if err != nil || len(tok) != 24 {
		t.Fatalf("token %q err %v", tok, err)
	}
	if _, _, err := e.o.Token("someone-else", txn.ID); err == nil {
		t.Fatal("other user must not read token")
	}
	e.assertBalanced(t)
	if got := e.o.Ledger.Balance(ledger.ProviderPayable, money.NGN); got != -500000 {
		t.Fatalf("provider payable %d", got)
	}
	if got := e.o.Ledger.Balance(ledger.PlatformFeeRevenue, money.NGN); got != -10000 {
		t.Fatalf("fee revenue %d", got)
	}
}

func TestDuplicatePaymentWebhookVendsOnce(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "45012345678", orchestrator.PrefToken)
	calls := e.vend.Calls
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef) }()
	}
	wg.Wait()
	if e.vend.Calls != calls {
		t.Fatalf("vend called %d extra times", e.vend.Calls-calls)
	}
	if n := len(e.o.Ledger.BySource(txn.ID)); n != 2 {
		t.Fatalf("want 2 ledger postings, got %d", n)
	}
	e.assertBalanced(t)
}

func TestPaymentFailureNeverVends(t *testing.T) {
	e := newEnv(t)
	m, _ := e.o.Meters.Register(e.ctx, "u1", "45012345678", "IKEDC", "", meters.PermManage)
	q, _ := e.o.CreateQuote("u1", m.ID, 500000, orchestrator.PrefToken)
	txn, _ := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "u1", QuoteID: q.ID})
	_ = e.pay.Complete(txn.PaymentProviderRef, false, 0)
	txn, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
	if txn.Status() != tx.OverallFailed || e.vend.Calls != 0 {
		t.Fatalf("status %s, vend calls %d", txn.Status(), e.vend.Calls)
	}
}

func TestUnderpaymentIsNotVended(t *testing.T) {
	e := newEnv(t)
	m, _ := e.o.Meters.Register(e.ctx, "u1", "45012345678", "IKEDC", "", meters.PermManage)
	q, _ := e.o.CreateQuote("u1", m.ID, 500000, orchestrator.PrefToken)
	txn, _ := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "u1", QuoteID: q.ID})
	_ = e.pay.Complete(txn.PaymentProviderRef, true, 100)
	_, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)
	if !errors.Is(err, orchestrator.ErrPaymentMismatch) || e.vend.Calls != 0 {
		t.Fatalf("err %v, vend calls %d", err, e.vend.Calls)
	}
}

func TestVendFailureRefunds(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "45012340000", orchestrator.PrefToken)
	if txn.Status() != tx.OverallRefunded {
		t.Fatalf("got %s", txn.Status())
	}
	if len(e.pay.Refunds) != 1 || e.pay.Refunds[0].Amount.Minor != 510000 {
		t.Fatalf("refunds %+v", e.pay.Refunds)
	}
	e.assertBalanced(t)
	if b := e.o.Ledger.Balance(ledger.PSPSettlementReceivable, money.NGN); b != 0 {
		t.Fatalf("PSP receivable should net to zero after refund, got %d", b)
	}
}

func TestVendTimeoutButVendedIsReconciledNotRefunded(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "45012349999", orchestrator.PrefToken)
	if txn.Vending != tx.VendUnknown || txn.Status() != tx.OverallReconciliation {
		t.Fatalf("timeout must be UNKNOWN, got %s/%s", txn.Vending, txn.Status())
	}
	if len(e.pay.Refunds) != 0 {
		t.Fatal("must not refund on ambiguous timeout")
	}
	calls := e.vend.Calls
	txn, err := e.o.Reconcile(e.ctx, txn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if txn.Status() != tx.OverallCompleted {
		t.Fatalf("got %s", txn.Status())
	}
	if e.vend.Calls != calls {
		t.Fatal("reconciliation must query status, not re-vend")
	}
	e.assertBalanced(t)
}

func TestVendTimeoutNotVendedIsSafelyRetried(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "45012348888", orchestrator.PrefToken)
	if txn.Vending != tx.VendUnknown {
		t.Fatalf("got %s", txn.Vending)
	}
	txn, _ = e.o.Reconcile(e.ctx, txn.ID)
	// mock 8888 always times out, so after a confirmed NOT_FOUND we retried
	// once and are UNKNOWN again — never FAILED, never refunded.
	if txn.Vending != tx.VendUnknown || len(e.pay.Refunds) != 0 {
		t.Fatalf("got %s, refunds %d", txn.Vending, len(e.pay.Refunds))
	}
}

func TestRemoteAcceptedIsNotConfirmed(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "71012345678", orchestrator.PrefAuto)
	if txn.DeliveryMethod != tx.MethodRemote || txn.Delivery != tx.DelAccepted {
		t.Fatalf("got %s/%s", txn.DeliveryMethod, txn.Delivery)
	}
	if txn.Status() == tx.OverallCompleted {
		t.Fatal("ACCEPTED must not count as completed")
	}
	if _, _, err := e.o.Token("u1", txn.ID); !errors.Is(err, orchestrator.ErrTokenUnavailable) {
		t.Fatalf("token should not be shown while remote delivery is in flight: %v", err)
	}
	e.remote.Settle()
	txn, _ = e.o.Reconcile(e.ctx, txn.ID)
	if txn.Status() != tx.OverallCompleted {
		t.Fatalf("got %s", txn.Status())
	}
	m, _ := e.o.Meters.Get(txn.MeterID)
	if !m.Capabilities.RemoteCredit.Confirmed {
		t.Fatal("meter remote credit should be marked confirmed")
	}
	e.assertBalanced(t)
}

func TestRemoteTimeoutIsUnknownAndNotResent(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "77012345678", orchestrator.PrefRemote)
	if txn.Delivery != tx.DelUnknown {
		t.Fatalf("got %s", txn.Delivery)
	}
	calls := e.remote.Calls
	txn, _ = e.o.Reconcile(e.ctx, txn.ID)
	if txn.Status() != tx.OverallCompleted || e.remote.Calls != calls {
		t.Fatalf("got %s, resent %d", txn.Status(), e.remote.Calls-calls)
	}
}

func TestRemoteFailureThenTokenFallback(t *testing.T) {
	e := newEnv(t)
	txn := e.buy(t, "u1", "76012345678", orchestrator.PrefRemote)
	e.remote.FailPending()
	txn, _ = e.o.Reconcile(e.ctx, txn.ID)
	if txn.Status() != tx.OverallManualReview {
		t.Fatalf("got %s", txn.Status())
	}
	if len(e.pay.Refunds) != 0 {
		t.Fatal("electricity was vended; must not auto-refund")
	}
	txn, err := e.o.FallbackToToken(txn.ID, "ops@test")
	if err != nil || txn.Status() != tx.OverallCompleted {
		t.Fatalf("fallback: %v %s", err, txn.Status())
	}
	if _, _, err := e.o.Token("u1", txn.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteNotEligible(t *testing.T) {
	e := newEnv(t)
	m, _ := e.o.Meters.Register(e.ctx, "u1", "45012345678", "IKEDC", "", meters.PermManage)
	if _, err := e.o.CreateQuote("u1", m.ID, 500000, orchestrator.PrefRemote); !errors.Is(err, orchestrator.ErrNotEligible) {
		t.Fatalf("got %v", err)
	}
}

func TestAccessControl(t *testing.T) {
	e := newEnv(t)
	m, _ := e.o.Meters.Register(e.ctx, "owner", "45012345678", "IKEDC", "", meters.PermManage)
	if _, err := e.o.CreateQuote("stranger", m.ID, 500000, orchestrator.PrefAuto); !errors.Is(err, meters.ErrForbidden) {
		t.Fatalf("stranger quote: %v", err)
	}
	// Grant, quote, then revoke before purchase: purchase must be rejected.
	if _, err := e.o.Meters.Register(e.ctx, "member", "45012345678", "IKEDC", "Mum", meters.PermPurchase); err != nil {
		t.Fatal(err)
	}
	q, err := e.o.CreateQuote("member", m.ID, 500000, orchestrator.PrefAuto)
	if err != nil {
		t.Fatal(err)
	}
	e.o.Meters.Revoke(m.ID, "member")
	if _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "member", QuoteID: q.ID}); !errors.Is(err, meters.ErrForbidden) {
		t.Fatalf("revoked purchase: %v", err)
	}
}

func TestQuoteSingleUseAndAmountLimits(t *testing.T) {
	e := newEnv(t)
	m, _ := e.o.Meters.Register(e.ctx, "u1", "45012345678", "IKEDC", "", meters.PermManage)
	if _, err := e.o.CreateQuote("u1", m.ID, 100, orchestrator.PrefAuto); !errors.Is(err, orchestrator.ErrAmountOutOfRange) {
		t.Fatalf("got %v", err)
	}
	q, _ := e.o.CreateQuote("u1", m.ID, 500000, orchestrator.PrefAuto)
	if _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "u1", QuoteID: q.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "u1", QuoteID: q.ID}); !errors.Is(err, orchestrator.ErrQuoteExpired) {
		t.Fatalf("reused quote: %v", err)
	}
	if _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: "u1", QuoteID: uuid.New()}); !errors.Is(err, orchestrator.ErrQuoteNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidMeterRejected(t *testing.T) {
	e := newEnv(t)
	if _, err := e.o.Meters.Register(e.ctx, "u1", "01234567890", "IKEDC", "", meters.PermManage); !errors.Is(err, meters.ErrInvalidMeter) {
		t.Fatalf("got %v", err)
	}
}
