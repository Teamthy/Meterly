package orchestrator_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (e *env) op(t *testing.T, reason string) audit.Actor {
	return audit.Actor{UserID: e.user(t, "ops"), Type: audit.ActorOperator, Reason: reason}
}

func (e *env) auditActions(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	evs, err := e.st.Repos().Audit().ForResource(e.ctx, "electricity_transaction", id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Action)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestOperatorRefundsUnderpaymentWithAmountActuallyPaid(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 100) // paid ₦1 instead of ₦5,100
		_, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef)

		if _, err := e.o.OperatorRefund(e.ctx, txn.ID, e.op(t, "")); !errors.Is(err, orchestrator.ErrReasonRequired) {
			t.Fatalf("reason: %v", err)
		}
		got, err := e.o.OperatorRefund(e.ctx, txn.ID, e.op(t, "customer underpaid; refund and ask to rebuy"))
		if err != nil {
			t.Fatal(err)
		}
		if got.Status() != tx.OverallRefunded || got.ReviewReason != "" {
			t.Fatalf("got %s %q", got.Status(), got.ReviewReason)
		}
		if len(e.pay.Refunds) != 1 || e.pay.Refunds[0].Amount.Minor != 100 {
			t.Fatalf("must refund what was paid, got %+v", e.pay.Refunds)
		}
		e.assertBalanced(t)
		if b := e.balance(t, ledger.PSPSettlementReceivable); b != 0 {
			t.Fatalf("PSP receivable %d", b)
		}
		if !contains(e.auditActions(t, txn.ID), "transaction.refund") {
			t.Fatal("refund not audited")
		}
		if e.vend.Calls != 0 {
			t.Fatal("never vend")
		}
		// A second click does nothing.
		if _, err := e.o.OperatorRefund(e.ctx, txn.ID, e.op(t, "double click")); !errors.Is(err, orchestrator.ErrNotInReview) {
			t.Fatalf("second refund: %v", err)
		}
		if len(e.pay.Refunds) != 1 {
			t.Fatal("refunded twice")
		}
	})
}

func TestOperatorRefundsLatePayment(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		e.later(3 * time.Hour)
		_, _ = e.o.Resume(e.ctx, txn.ID) // expires
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef) // late payment → review
		got, err := e.o.OperatorRefund(e.ctx, txn.ID, e.op(t, "paid after the checkout expired"))
		if err != nil || got.Status() != tx.OverallRefunded || e.pay.Refunds[0].Amount.Minor != 510000 {
			t.Fatalf("%v %s %+v", err, got.Status(), e.pay.Refunds)
		}
		e.assertBalanced(t)
	})
}

func TestNoRefundWhenElectricityMayHaveBeenVended(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012348888", orchestrator.PrefToken) // vend stays UNKNOWN
		for i := 0; i < 40; i++ {
			e.later(time.Hour)
			_, _, _ = e.o.ResumeDue(e.ctx)
		}
		if got := e.get(t, txn.ID); got.Status() != tx.OverallManualReview {
			t.Fatalf("want escalated, got %s", got.Status())
		}
		if _, err := e.o.OperatorRefund(e.ctx, txn.ID, e.op(t, "customer asked for money back")); !errors.Is(err, orchestrator.ErrRefundNotSafe) {
			t.Fatalf("got %v", err)
		}
		// Requeue resumes automation with a fresh attempt budget.
		got, err := e.o.Requeue(e.ctx, txn.ID, e.op(t, "vending provider says the outage is fixed"))
		if err != nil {
			t.Fatal(err)
		}
		if got.ReviewReason != "" || got.CheckAttempts != 0 || got.Status() != tx.OverallReconciliation {
			t.Fatalf("got %s attempts=%d review=%q", got.Status(), got.CheckAttempts, got.ReviewReason)
		}
		if _, err := e.o.Requeue(e.ctx, txn.ID, e.op(t, "again")); !errors.Is(err, orchestrator.ErrNotInReview) {
			t.Fatalf("requeue when not in review: %v", err)
		}
		acts := e.auditActions(t, txn.ID)
		if !contains(acts, "transaction.requeue") || contains(acts, "transaction.refund") {
			t.Fatalf("audit %v", acts)
		}
	})
}

func TestDisabledVendingPausesWithoutEscalating(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.o.SetProviderEnabled(e.ctx, "mockvend", false, e.op(t, "")); !errors.Is(err, orchestrator.ErrReasonRequired) {
			t.Fatalf("reason: %v", err)
		}
		if err := e.o.SetProviderEnabled(e.ctx, "mockvend", false, e.op(t, "provider outage")); err != nil {
			t.Fatal(err)
		}
		if err := e.o.SetProviderEnabled(e.ctx, "nope", false, e.op(t, "typo")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown provider: %v", err)
		}
		txn := e.create(t, "u1", "45012345678", orchestrator.PrefToken)
		_ = e.pay.Complete(txn.PaymentProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); !errors.Is(err, orchestrator.ErrProviderDisabled) {
			t.Fatalf("got %v", err)
		}
		for i := 0; i < 30; i++ {
			e.later(time.Hour)
			_, _, _ = e.o.ResumeDue(e.ctx)
		}
		got := e.get(t, txn.ID)
		if e.vend.Calls != 0 || got.Payment != tx.PayConfirmed || got.Vending != tx.VendNotStarted || got.ReviewReason != "" || got.CheckAttempts != 0 {
			t.Fatalf("paused: calls=%d %s/%s review=%q attempts=%d", e.vend.Calls, got.Payment, got.Vending, got.ReviewReason, got.CheckAttempts)
		}
		if err := e.o.SetProviderEnabled(e.ctx, "mockvend", true, e.op(t, "outage over")); err != nil {
			t.Fatal(err)
		}
		e.later(time.Hour)
		_, _, _ = e.o.ResumeDue(e.ctx)
		if got := e.get(t, txn.ID); got.Status() != tx.OverallCompleted || e.vend.Calls != 1 {
			t.Fatalf("after re-enable: %s calls=%d", got.Status(), e.vend.Calls)
		}
		evs, _ := e.st.Repos().Audit().ForResource(e.ctx, "provider", orchestrator.ProviderResourceID("mockvend"))
		if len(evs) != 2 || evs[0].Action != "provider.disable" || evs[1].Action != "provider.enable" || evs[0].Metadata["reason"] != "provider outage" {
			t.Fatalf("provider audit %+v", evs)
		}
	})
}

func TestDisabledPaymentProviderBlocksNewCheckouts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_ = e.o.SetProviderEnabled(e.ctx, "mockpay", false, e.op(t, "PSP incident"))
		u := e.user(t, "u1")
		m := e.register(t, "u1", "45012345678")
		q, _ := e.o.CreateQuote(e.ctx, u, m.ID, 500000, orchestrator.PrefToken)
		got, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{CustomerID: u, QuoteID: q.ID})
		if !errors.Is(err, orchestrator.ErrProviderDisabled) || got == nil || got.Payment != tx.PayCreated {
			t.Fatalf("got %v %+v", err, got)
		}
	})
}

func TestTokenReadsAndRevealsAreAudited(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		txn := e.buy(t, "u1", "45012345678", orchestrator.PrefToken)
		cid := uuid.New()
		ctx := audit.WithCorrelation(e.ctx, cid)
		if _, _, err := e.o.Token(ctx, e.user(t, "u1"), txn.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.o.RevealToken(e.ctx, txn.ID, e.op(t, "")); !errors.Is(err, orchestrator.ErrReasonRequired) {
			t.Fatalf("reveal without reason: %v", err)
		}
		if _, _, err := e.o.RevealToken(e.ctx, txn.ID, e.op(t, "customer on the phone, ticket 42")); err != nil {
			t.Fatal(err)
		}
		evs, _ := e.st.Repos().Audit().ForResource(e.ctx, "electricity_transaction", txn.ID)
		if len(evs) != 2 || evs[0].Action != "token.read" || evs[0].CorrelationID == nil || *evs[0].CorrelationID != cid ||
			evs[0].ActorUserID != e.user(t, "u1") || evs[1].Action != "token.reveal" {
			t.Fatalf("audit %+v", evs)
		}
	})
}

func TestAdminConsoleCountsAndInvestigationActions(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_ = e.buy(t, "u1", "45012345678", orchestrator.PrefToken)
		bad := e.create(t, "u2", "45012345679", orchestrator.PrefToken)
		_ = e.pay.Complete(bad.PaymentProviderRef, true, 100)
		_, _ = e.o.HandlePaymentNotification(e.ctx, bad.PaymentProviderRef)

		opCons, err := e.o.AdminConsole(e.ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if opCons.Role != "OPERATOR" || opCons.CanAdmin {
			t.Fatalf("operator console %+v", opCons)
		}
		if len(opCons.Review) != 1 || opCons.Review[0].TransactionID != bad.ID || opCons.Review[0].Status != "MANUAL_REVIEW" {
			t.Fatalf("review %+v", opCons.Review)
		}
		if len(opCons.Summary) == 0 || opCons.Summary[0].Value != "1" {
			t.Fatalf("summary %+v", opCons.Summary)
		}
		adminCons, err := e.o.AdminConsole(e.ctx, true)
		if err != nil || !adminCons.CanAdmin || adminCons.Role != "ADMIN" {
			t.Fatalf("admin console %v %+v", err, adminCons)
		}

		d, err := e.o.Detail(e.ctx, bad.ID)
		if err != nil {
			t.Fatal(err)
		}
		invOp := orchestrator.InvestigationFrom(d, false)
		invAd := orchestrator.InvestigationFrom(d, true)
		if invOp.TokenAvailable {
			t.Fatal("underpaid review must not expose a token")
		}
		refundOp, refundAd := false, false
		for _, a := range invOp.Actions {
			if a.ID == "refund" {
				refundOp = a.Enabled
			}
		}
		for _, a := range invAd.Actions {
			if a.ID == "refund" {
				refundAd = a.Enabled
			}
		}
		if refundOp {
			t.Fatal("operator investigation must not enable refund")
		}
		if !refundAd {
			t.Fatalf("admin investigation should enable refund %+v", invAd.Actions)
		}
	})
}

func TestSearchAndDetail(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		done := e.buy(t, "u1", "45012345678", orchestrator.PrefToken)
		e.later(time.Second) // distinct creation times
		bad := e.create(t, "u2", "45012345679", orchestrator.PrefToken)
		_ = e.pay.Complete(bad.PaymentProviderRef, true, 100)
		_, _ = e.o.HandlePaymentNotification(e.ctx, bad.PaymentProviderRef)

		review, _ := e.o.Search(e.ctx, store.TxnFilter{ReviewOnly: true})
		if len(review) != 1 || review[0].ID != bad.ID {
			t.Fatalf("review queue %v", review)
		}
		byCustomer, _ := e.o.Search(e.ctx, store.TxnFilter{CustomerID: e.user(t, "u1")})
		if len(byCustomer) != 1 || byCustomer[0].ID != done.ID {
			t.Fatalf("by customer %v", byCustomer)
		}
		all, _ := e.o.Search(e.ctx, store.TxnFilter{Limit: 1})
		if len(all) != 1 || all[0].ID != bad.ID { // newest first
			t.Fatalf("limit/order %v", all)
		}
		older, _ := e.o.Search(e.ctx, store.TxnFilter{Before: all[0].CreatedAt})
		if len(older) != 1 || older[0].ID != done.ID {
			t.Fatalf("keyset page %v", older)
		}
		d, err := e.o.Detail(e.ctx, done.ID)
		if err != nil || len(d.Ledger) != 2 || len(d.Events) == 0 {
			t.Fatalf("detail %v ledger=%d events=%d", err, len(d.Ledger), len(d.Events))
		}
	})
}
