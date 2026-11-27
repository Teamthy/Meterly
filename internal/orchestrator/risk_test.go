package orchestrator_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store/memstore"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestRiskDenyNeverCharges(t *testing.T) {
	e := newEnv(t, memstore.New())
	e.o.SetFeatureRiskEngine(true)
	if err := e.st.Repos().Risk().UpdateRule(e.ctx, risk.Rule{
		Code: risk.RuleVelocityUser, Enabled: true, Action: risk.Deny, WindowSeconds: 3600, Threshold: 0,
	}); err != nil {
		t.Fatal(err)
	}
	u := e.user(t, "ada")
	m := e.register(t, "ada", "61100000011")
	q, err := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefToken)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: q.ID, Email: "ada@example.com"})
	if !errors.Is(err, orchestrator.ErrRiskDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	if n := len(e.pay.Snapshot()); n != 0 {
		t.Fatalf("DENY must not initialise checkout, got %d payments", n)
	}
}

func TestRiskReviewNeverVends(t *testing.T) {
	e := newEnv(t, memstore.New())
	e.o.SetFeatureRiskEngine(true)
	if err := e.st.Repos().Risk().UpdateRule(e.ctx, risk.Rule{
		Code: risk.RuleVelocityUser, Enabled: true, Action: risk.Review, WindowSeconds: 3600, Threshold: 0,
	}); err != nil {
		t.Fatal(err)
	}
	txn := e.buy(t, "ada", "61100000012", orchestrator.PrefToken)
	if txn.Payment != tx.PayConfirmed {
		t.Fatalf("payment should be captured: %s", txn.Payment)
	}
	if txn.ReviewReason == "" || txn.Vending != tx.VendNotStarted {
		t.Fatalf("REVIEW must hold vending: reason=%q vend=%s", txn.ReviewReason, txn.Vending)
	}
	if txn.Status() != tx.OverallManualReview {
		t.Fatalf("status %s", txn.Status())
	}
}

func TestRiskConcurrentVelocity(t *testing.T) {
	e := newEnv(t, memstore.New())
	e.o.SetFeatureRiskEngine(true)
	if err := e.st.Repos().Risk().UpdateRule(e.ctx, risk.Rule{
		Code: risk.RuleVelocityUser, Enabled: true, Action: risk.Deny, WindowSeconds: 3600, Threshold: 1,
	}); err != nil {
		t.Fatal(err)
	}
	u := e.user(t, "ada")
	m1 := e.register(t, "ada", "61100000013")
	m2 := e.register(t, "ada", "61100000014")
	q1, err := e.o.CreateQuote(e.ctx, u, m1.ID, 500000, orchestrator.PrefToken)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := e.o.CreateQuote(e.ctx, u, m2.ID, 500000, orchestrator.PrefToken)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var allow, deny int
	var wg sync.WaitGroup
	wg.Add(2)
	for _, qid := range []uuid.UUID{q1.ID, q2.ID} {
		qid := qid
		go func() {
			defer wg.Done()
			_, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: qid, Email: "ada@example.com"})
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, orchestrator.ErrRiskDenied) {
				deny++
			} else if err == nil {
				allow++
			} else {
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	if allow != 1 || deny != 1 {
		t.Fatalf("want 1 allow 1 deny, got allow=%d deny=%d", allow, deny)
	}
}

func TestDisputeWebhookLedgerAndReplay(t *testing.T) {
	e := newEnv(t, memstore.New())
	txn := e.buy(t, "ada", "61100000015", orchestrator.PrefToken)
	n := disputes.Notice{
		Type: "charge.dispute.create", PSPDisputeID: "2867",
		Reference: txn.PaymentProviderRef, AmountMinor: txn.Total.Minor, Currency: "NGN",
	}
	if err := e.o.HandlePaymentDispute(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	// Liability credit is negative; expense debit is positive.
	if e.balance(t, ledger.DisputeReserve) != -txn.Total.Minor {
		t.Fatalf("reserve %d", e.balance(t, ledger.DisputeReserve))
	}
	if e.balance(t, ledger.DisputeLoss) != txn.Total.Minor {
		t.Fatalf("loss provision %d", e.balance(t, ledger.DisputeLoss))
	}
	got, err := e.st.Repos().Users().Status(e.ctx, txn.CustomerID)
	if err != nil || got != "SUSPENDED" {
		t.Fatalf("suspended: %s %v", got, err)
	}
	if err := e.o.HandlePaymentDispute(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	if e.balance(t, ledger.DisputeReserve) != -txn.Total.Minor {
		t.Fatal("replay doubled the open posting")
	}
	n.Type = "charge.dispute.resolve"
	n.Resolution = "declined"
	n.PSPStatus = "resolved"
	if err := e.o.HandlePaymentDispute(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	if e.balance(t, ledger.DisputeReserve) != 0 || e.balance(t, ledger.DisputeLoss) != 0 {
		t.Fatalf("win should release reserve: reserve=%d loss=%d", e.balance(t, ledger.DisputeReserve), e.balance(t, ledger.DisputeLoss))
	}
	e.assertBalanced(t)
}

func TestDisputeLossPosting(t *testing.T) {
	e := newEnv(t, memstore.New())
	txn := e.buy(t, "ada", "61100000016", orchestrator.PrefToken)
	n := disputes.Notice{
		Type: "charge.dispute.create", PSPDisputeID: "99",
		Reference: txn.PaymentProviderRef, AmountMinor: txn.Total.Minor, Currency: "NGN",
	}
	if err := e.o.HandlePaymentDispute(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Type = "charge.dispute.resolve"
	n.Resolution = "merchant-accepted"
	n.PSPStatus = "resolved"
	if err := e.o.HandlePaymentDispute(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	if e.balance(t, ledger.DisputeReserve) != 0 {
		t.Fatalf("reserve %d", e.balance(t, ledger.DisputeReserve))
	}
	if e.balance(t, ledger.DisputeLoss) != txn.Total.Minor {
		t.Fatalf("loss %d", e.balance(t, ledger.DisputeLoss))
	}
	e.assertBalanced(t)
}
