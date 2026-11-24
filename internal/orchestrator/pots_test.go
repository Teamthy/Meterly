package orchestrator_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (e *env) assertPotFundsZero(t *testing.T) {
	t.Helper()
	e.assertBalanced(t)
	bal, err := e.st.Repos().Ledger().Balance(e.ctx, ledger.PotFundsHeld, money.NGN)
	if err != nil || bal != 0 {
		t.Fatalf("POT_FUNDS_HELD balance = %d (%v), want 0", bal, err)
	}
	clr, err := e.st.Repos().Ledger().Balance(e.ctx, ledger.CustomerFundsClearing, money.NGN)
	if err != nil || clr != 0 {
		t.Fatalf("CUSTOMER_FUNDS_CLEARING balance = %d (%v), want 0", clr, err)
	}
	rp, err := e.st.Repos().Ledger().Balance(e.ctx, ledger.RefundPayable, money.NGN)
	if err != nil || rp != 0 {
		t.Fatalf("REFUND_PAYABLE balance = %d (%v), want 0", rp, err)
	}
}

func (e *env) createPot(t *testing.T, owner, meter string, targetMinor int64, shares int, ttl time.Duration) *orchestrator.CreatedPot {
	t.Helper()
	m := e.register(t, owner, meter)
	cp, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
		OwnerID:         e.user(t, owner),
		MeterID:         m.ID,
		TargetMinor:     targetMinor,
		SuggestedShares: shares,
		Deadline:        e.clock.Add(ttl),
		Note:            "Monthly flat electricity split",
		PublicBaseURL:   "https://meterly.ng",
	})
	if err != nil {
		t.Fatalf("createPot: %v", err)
	}
	return cp
}

func (e *env) contribute(t *testing.T, code string, amount int64, email, name, key string) *orchestrator.ContributePotResult {
	t.Helper()
	res, _, err := e.o.ContributeToPot(e.ctx, orchestrator.ContributePotRequest{
		Code:           code,
		AmountMinor:    amount,
		Email:          email,
		Name:           name,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("contribute(%s): %v", email, err)
	}
	return res
}

// Happy path: 4 roommates split ₦10,100 (₦10,000 electricity + single ₦100 fee).
// Verifies single flat fee, single token vend, POT_FUNDS_HELD nets to zero and
// trial balance is zero.
func TestPotHappyPathFourShares(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		cp := e.createPot(t, "organiser", "45012345678", 1010000, 4, 24*time.Hour)
		if cp.SuggestedShareMinor != 252500 || cp.ElectricityAmountMinor != 1000000 || cp.FeeMinor != 10000 {
			t.Fatalf("unexpected share/fee math: %+v", cp)
		}

		names := []string{"Tunde Okafor", "Amina Bello", "Chinedu Eze", ""}
		var settledTxn *tx.Transaction
		for i, nm := range names {
			c := e.contribute(t, cp.Code, 252500, fmt.Sprintf("r%d@example.com", i+1), nm, fmt.Sprintf("idem-share-00000%d", i+1))
			if err := e.pay.Complete(c.ProviderRef, true, 0); err != nil {
				t.Fatal(err)
			}
			var err error
			settledTxn, err = e.o.HandlePaymentNotification(e.ctx, c.ProviderRef)
			if err != nil {
				t.Fatalf("webhook %d: %v", i+1, err)
			}
			e.later(time.Second)
		}

		if settledTxn == nil || settledTxn.EffectiveFundingSource() != tx.FundingPot {
			t.Fatalf("expected POT-funded transaction, got %+v", settledTxn)
		}
		if settledTxn.Status() != tx.OverallCompleted || settledTxn.Amount.Minor != 1000000 || settledTxn.Fee.Minor != 10000 || settledTxn.Total.Minor != 1010000 {
			t.Fatalf("unexpected settled transaction: status=%s amt=%d fee=%d total=%d",
				settledTxn.Status(), settledTxn.Amount.Minor, settledTxn.Fee.Minor, settledTxn.Total.Minor)
		}
		if e.vend.Calls != 1 || len(e.pay.Refunds) != 0 {
			t.Fatalf("want 1 vend and 0 refunds, got vends=%d refunds=%d", e.vend.Calls, len(e.pay.Refunds))
		}

		detail, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Status != pots.StatusSettled || detail.FundedMinor != 1010000 || detail.RemainingMinor != 0 || len(detail.Allocations) != 1 {
			t.Fatalf("unexpected pot detail: %+v", detail)
		}

		pub, err := e.o.GetPublicPot(e.ctx, cp.Code)
		if err != nil {
			t.Fatal(err)
		}
		if pub.Status != pots.StatusSettled || len(pub.Contributors) != 4 {
			t.Fatalf("unexpected public pot: %+v", pub)
		}
		wantInitials := []string{"T.O.", "A.B.", "C.E.", "Anonymous"}
		for i, want := range wantInitials {
			if pub.Contributors[i].Initials != want {
				t.Fatalf("contributor %d initials = %q, want %q", i, pub.Contributors[i].Initials, want)
			}
		}

		// Ledger().Balance returns debits - credits, so credit revenue is -10000.
		feeRev, _ := e.st.Repos().Ledger().Balance(e.ctx, ledger.PlatformFeeRevenue, money.NGN)
		if feeRev != -10000 {
			t.Fatalf("platform fee revenue = %d, want -10000 (charged once)", feeRev)
		}
		e.assertPotFundsZero(t)
	})
}

// Pot expires with 3 paid contributions; crash injected mid-refund after the 2nd
// PSP refund succeeds before commit. ResumeDue must complete without double-refunding.
func TestPotExpiryThreeContributionsCrashMidRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		cp := e.createPot(t, "organiser", "45012345678", 1010000, 5, 2*time.Hour)
		var contribIDs []uuid.UUID
		for i := 1; i <= 3; i++ {
			c := e.contribute(t, cp.Code, 200000, fmt.Sprintf("c%d@example.com", i), fmt.Sprintf("User %d", i), fmt.Sprintf("idem-exp-0000000%d", i))
			contribIDs = append(contribIDs, c.ContributionID)
			if err := e.pay.Complete(c.ProviderRef, true, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := e.o.HandlePaymentNotification(e.ctx, c.ProviderRef); err != nil {
				t.Fatal(err)
			}
			e.later(time.Second)
		}

		// Inject crash right after the 2nd RefundPayment call returns from the PSP,
		// before its completion transaction commits!
		var refundCalls atomic.Int32
		e.hp.afterRefund = func(_ providers.RefundRequest) {
			if refundCalls.Add(1) == 2 {
				e.st.arm(1)
				e.hp.afterRefund = nil
			}
		}

		// Advance past the 2-hour pot deadline and run ResumeDue.
		e.later(2*time.Hour + time.Minute)
		if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
			t.Fatal(err)
		}

		// Pot must still be REFUNDING because contribution #2 crashed before commit
		// (and contribution #3 was not yet committed).
		midDetail, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if midDetail.Status != pots.StatusRefunding {
			t.Fatalf("expected REFUNDING mid-crash, got %s", midDetail.Status)
		}

		// Advance past ResumeLease so the interrupted claim on #2 expires, then sweep again.
		e.later(5 * time.Minute)
		if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
			t.Fatal(err)
		}

		finalDetail, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if finalDetail.Status != pots.StatusRefunded || finalDetail.RefundedMinor != 600000 || finalDetail.FundedMinor != 0 {
			t.Fatalf("expected REFUNDED with 600000 refunded, got %+v", finalDetail)
		}
		if len(e.pay.Refunds) != 3 {
			t.Fatalf("expected exactly 3 unique PSP refunds, got %d: %+v", len(e.pay.Refunds), e.pay.Refunds)
		}
		for i, cid := range contribIDs {
			wantRef := "refund:contribution:" + cid.String()
			if e.pay.Refunds[i].Reference != wantRef || e.pay.Refunds[i].Amount.Minor != 200000 {
				t.Fatalf("refund[%d] = %+v, want ref=%s amount=200000", i, e.pay.Refunds[i], wantRef)
			}
		}
		e.assertPotFundsZero(t)
	})
}

// Concurrent overfund race: two contributors simultaneously pay ₦4,000 when only
// ₦3,000 remains on a ₦5,100 pot. Retained total is capped at ₦5,100 and the
// ₦5,000 excess is automatically refunded.
func TestPotConcurrentOverfundAutoRefundsExcess(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		cp := e.createPot(t, "organiser", "45012345678", 510000, 3, 24*time.Hour)

		c1 := e.contribute(t, cp.Code, 210000, "p1@example.com", "Ada Obi", "idem-over-0000001")
		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef); err != nil {
			t.Fatal(err)
		}

		c2 := e.contribute(t, cp.Code, 400000, "p2@example.com", "Bola Ade", "idem-over-0000002")
		c3 := e.contribute(t, cp.Code, 400000, "p3@example.com", "Chike Nwa", "idem-over-0000003")
		_ = e.pay.Complete(c2.ProviderRef, true, 0)
		_ = e.pay.Complete(c3.ProviderRef, true, 0)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.o.HandlePaymentNotification(e.ctx, c2.ProviderRef)
		}()
		go func() {
			defer wg.Done()
			_, _ = e.o.HandlePaymentNotification(e.ctx, c3.ProviderRef)
		}()
		wg.Wait()

		// Run an AdvancePot pass to ensure any racing excess refund completes cleanly.
		if _, err := e.o.AdvancePot(e.ctx, cp.ID); err != nil {
			t.Fatal(err)
		}

		detail, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Status != pots.StatusSettled {
			t.Fatalf("want SETTLED, got %s", detail.Status)
		}
		if detail.FundedMinor != 510000 || detail.RefundedMinor != 500000 {
			t.Fatalf("want funded=510000 refunded=500000, got funded=%d refunded=%d", detail.FundedMinor, detail.RefundedMinor)
		}
		if e.vend.Calls != 1 {
			t.Fatalf("want exactly 1 vend call, got %d", e.vend.Calls)
		}
		var totalRefundedByMock int64
		for _, r := range e.pay.Refunds {
			totalRefundedByMock += r.Amount.Minor
		}
		if totalRefundedByMock != 500000 {
			t.Fatalf("mock PSP refunded %d, want 500000", totalRefundedByMock)
		}
		e.assertPotFundsZero(t)
	})
}

// Late payment arriving after a pot already expired and settled to REFUNDED:
// verifies at PSP, records receipt in POT_FUNDS_HELD, refunds 100% immediately
// and returns pot to REFUNDED with POT_FUNDS_HELD == 0.
func TestPotLatePaymentAfterExpiryRefundedImmediately(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		cp := e.createPot(t, "organiser", "45012345678", 510000, 2, time.Hour)
		c1 := e.contribute(t, cp.Code, 200000, "early@example.com", "Early Payer", "idem-late-0000001")
		cLate := e.contribute(t, cp.Code, 200000, "late@example.com", "Late Payer", "idem-late-0000002")

		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef); err != nil {
			t.Fatal(err)
		}

		// Expire the pot while cLate is still unpaid at the PSP.
		e.later(time.Hour + time.Minute)
		if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
			t.Fatal(err)
		}
		d1, _ := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if d1.Status != pots.StatusRefunded || d1.RefundedMinor != 200000 {
			t.Fatalf("expected REFUNDED after deadline, got %+v", d1)
		}

		// Now the second contributor finishes their open Paystack tab after expiry!
		_ = e.pay.Complete(cLate.ProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, cLate.ProviderRef); err != nil {
			t.Fatal(err)
		}

		d2, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d2.Status != pots.StatusRefunded || d2.RefundedMinor != 400000 || len(e.pay.Refunds) != 2 {
			t.Fatalf("expected late payment refunded (total 400000, 2 refunds), got status=%s refunded=%d refunds=%d",
				d2.Status, d2.RefundedMinor, len(e.pay.Refunds))
		}
		e.assertPotFundsZero(t)
	})
}

// Organiser loses PURCHASE access on the meter:
// (a) while OPEN, next contribution cancels the pot and refunds prior contributors;
// (b) right before allocation (FUNDED -> AdvancePot), pot moves to REFUNDING -> REFUNDED.
func TestPotOrganiserLosesPurchaseCancelsAndRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		tenantID := e.user(t, "tenant")
		m := e.register(t, "tenant", "45012345678")

		// Case A: OPEN pot with one paid contribution, then PURCHASE is revoked.
		cp, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID:     tenantID,
			MeterID:     m.ID,
			TargetMinor: 510000,
			Deadline:    e.clock.Add(12 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		c1 := e.contribute(t, cp.Code, 200000, "a@example.com", "Ayo", "idem-revoke-00001")
		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef)

		// Revoke PURCHASE via RemoveSavedMeter.
		if err := e.o.Meters.RemoveSavedMeter(e.ctx, tenantID, m.ID); err != nil {
			t.Fatal(err)
		}

		_, _, err = e.o.ContributeToPot(e.ctx, orchestrator.ContributePotRequest{
			Code:           cp.Code,
			AmountMinor:    310000,
			Email:          "b@example.com",
			Name:           "Bisi",
			IdempotencyKey: "idem-revoke-00002",
		})
		if !errors.Is(err, orchestrator.ErrPotUnusable) {
			t.Fatalf("want ErrPotUnusable, got %v", err)
		}
		detA, _ := e.o.GetPotForOwner(e.ctx, tenantID, cp.ID)
		if detA.Status != pots.StatusRefunded || detA.RefundedMinor != 200000 {
			t.Fatalf("expected cancelled/refunded pot after access loss, got %+v", detA)
		}
		e.assertPotFundsZero(t)

		// Case B: Re-register meter, create pot & contributor checkout, revoke PURCHASE before final payment webhook allocates.
		m, err = e.o.Meters.Register(e.ctx, tenantID, "45012345678", "IKEDC", "Home")
		if err != nil {
			t.Fatal(err)
		}
		cpB, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID:     tenantID,
			MeterID:     m.ID,
			TargetMinor: 510000,
			Deadline:    e.clock.Add(12 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		cb1 := e.contribute(t, cpB.Code, 510000, "full@example.com", "Full Payer", "idem-revoke-00003")
		if err := e.o.Meters.RemoveSavedMeter(e.ctx, tenantID, m.ID); err != nil {
			t.Fatal(err)
		}
		_ = e.pay.Complete(cb1.ProviderRef, true, 0)
		if _, err := e.o.HandlePaymentNotification(e.ctx, cb1.ProviderRef); err != nil {
			t.Fatal(err)
		}
		detB, _ := e.o.GetPotForOwner(e.ctx, tenantID, cpB.ID)
		if detB.Status != pots.StatusRefunded || detB.RefundedMinor != 510000 || e.vend.Calls != 0 {
			t.Fatalf("expected FUNDED -> REFUNDING -> REFUNDED with 0 vends, got status=%s refunded=%d vends=%d",
				detB.Status, detB.RefundedMinor, e.vend.Calls)
		}
		e.assertPotFundsZero(t)
	})
}

// Vend failure on a POT-funded purchase returns CUSTOMER_FUNDS_CLEARING -> POT_FUNDS_HELD
// (never calling PSP RefundPayment on the POT transaction) and refunds contributors.
func TestPotVendFailureReturnsToPotAndRefundsContributors(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// Meter ending in 0000 triggers deterministic VendFailed (METER_BLOCKED) in mockvend.
		cp := e.createPot(t, "organiser", "45012340000", 510000, 2, 24*time.Hour)
		c1 := e.contribute(t, cp.Code, 250000, "u1@example.com", "Uche One", "idem-vfail-000001")
		c2 := e.contribute(t, cp.Code, 260000, "u2@example.com", "Uche Two", "idem-vfail-000002")

		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef)
		_ = e.pay.Complete(c2.ProviderRef, true, 0)
		failedTxn, err := e.o.HandlePaymentNotification(e.ctx, c2.ProviderRef)
		if err != nil {
			t.Fatal(err)
		}
		if failedTxn == nil || failedTxn.Vending != tx.VendFailed || failedTxn.Payment != tx.PayRefunded {
			t.Fatalf("expected POT transaction VendFailed + PayRefunded, got %+v", failedTxn)
		}

		detail, err := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Status != pots.StatusRefunded || detail.RefundedMinor != 510000 {
			t.Fatalf("expected pot REFUNDED with 510000 refunded, got %+v", detail)
		}
		if len(detail.Allocations) != 1 || detail.Allocations[0].Status != pots.AllocReturned {
			t.Fatalf("expected allocation RETURNED, got %+v", detail.Allocations)
		}
		// Verify PSP refunds were issued ONLY for the 2 contributions (never "refund:<txn_id>").
		if len(e.pay.Refunds) != 2 {
			t.Fatalf("want 2 contributor refunds, got %d: %+v", len(e.pay.Refunds), e.pay.Refunds)
		}
		for _, r := range e.pay.Refunds {
			if r.Reference == "refund:"+failedTxn.ID.String() {
				t.Fatalf("orchestrator called PSP RefundPayment directly on POT-funded transaction %s", failedTxn.ID)
			}
		}
		e.assertPotFundsZero(t)
	})
}

// OperatorRefund on a POT-funded transaction in manual review (before vend)
// returns funds to POT_FUNDS_HELD and refunds contributors.
func TestPotOperatorRefundBranchesOnFundingSource(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		opID := e.user(t, "operator-1")
		actor := audit.Actor{UserID: opID, Type: audit.ActorOperator, Reason: "pause vend"}
		if err := e.o.SetProviderEnabled(e.ctx, e.vend.Name(), false, actor); err != nil {
			t.Fatal(err)
		}
		cp := e.createPot(t, "organiser", "45012345678", 510000, 2, 24*time.Hour)
		c1 := e.contribute(t, cp.Code, 510000, "solo@example.com", "Solo Contributor", "idem-opref-000001")
		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		potTxn, err := e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef)
		if !errors.Is(err, orchestrator.ErrProviderDisabled) || potTxn == nil {
			t.Fatalf("expected ErrProviderDisabled with allocated potTxn, got txn=%v err=%v", potTxn, err)
		}

		// Put the unvended POT transaction into manual review and run OperatorRefund.
		if err := e.st.InTx(e.ctx, func(r store.Repos) error {
			cur, gErr := r.Transactions().GetForUpdate(e.ctx, potTxn.ID)
			if gErr != nil {
				return gErr
			}
			cur.FlagForReview("customer requested cancellation while vend provider paused")
			return r.Transactions().Save(e.ctx, cur)
		}); err != nil {
			t.Fatal(err)
		}

		refundedTxn, err := e.o.OperatorRefund(e.ctx, potTxn.ID, audit.Actor{
			UserID: opID, Type: audit.ActorOperator, Reason: "approved pot return",
		})
		if err != nil {
			t.Fatalf("OperatorRefund: %v", err)
		}
		if refundedTxn.Payment != tx.PayRefunded {
			t.Fatalf("want PayRefunded, got %s", refundedTxn.Payment)
		}
		detail, _ := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if detail.Status != pots.StatusRefunded || detail.RefundedMinor != 510000 {
			t.Fatalf("expected pot REFUNDED after OperatorRefund, got %+v", detail)
		}
		e.assertPotFundsZero(t)
	})
}

// UNKNOWN vend on a POT-funded purchase holds the pot in ALLOCATING and blocks
// both organiser cancellation and operator refunds until reconciled.
func TestPotUnknownVendBlocksContributorRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// Meter ending in 8888 returns ErrAmbiguous on Vend and VendPending on GetVendStatus.
		cp := e.createPot(t, "organiser", "45012348888", 510000, 2, 24*time.Hour)
		c1 := e.contribute(t, cp.Code, 510000, "unk@example.com", "Unknown Case", "idem-unkn-0000001")
		_ = e.pay.Complete(c1.ProviderRef, true, 0)

		potTxn, err := e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef)
		if err != nil {
			t.Fatal(err)
		}
		if potTxn == nil || potTxn.Vending != tx.VendUnknown {
			t.Fatalf("expected VendUnknown, got %+v", potTxn)
		}

		// Organiser cancel must be rejected while ALLOCATING.
		if _, err := e.o.CancelPot(e.ctx, e.user(t, "organiser"), cp.ID); !errors.Is(err, orchestrator.ErrPotNotCancellable) {
			t.Fatalf("want ErrPotNotCancellable during VendUnknown, got %v", err)
		}

		// Sweeping past the pot deadline must NOT expire or refund an ALLOCATING pot!
		e.later(25 * time.Hour)
		_, _, _ = e.o.ResumeDue(e.ctx)

		detail, _ := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
		if detail.Status != pots.StatusAllocating || len(e.pay.Refunds) != 0 {
			t.Fatalf("pot must stay ALLOCATING with 0 refunds during VendUnknown, got status=%s refunds=%d",
				detail.Status, len(e.pay.Refunds))
		}
		e.assertBalanced(t)
	})
}

// Repeated PSP refund failures on a REFUNDING pot escalate the pot to manual review.
func TestPotRepeatedRefundFailuresEscalateToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		reg := observability.NewRegistry()
		e.o.SetMetrics(reg)

		cp := e.createPot(t, "organiser", "45012345678", 510000, 2, time.Hour)
		c1 := e.contribute(t, cp.Code, 200000, "esc@example.com", "Escalate User", "idem-escl-0000001")
		_ = e.pay.Complete(c1.ProviderRef, true, 0)
		_, _ = e.o.HandlePaymentNotification(e.ctx, c1.ProviderRef)

		e.hp.refundErr = errors.New("psp bank rail down")
		if _, err := e.o.CancelPot(e.ctx, e.user(t, "organiser"), cp.ID); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < 15; i++ {
			e.later(35 * time.Minute)
			if _, _, err := e.o.ResumeDue(e.ctx); err != nil {
				t.Fatal(err)
			}
			d, _ := e.o.GetPotForOwner(e.ctx, e.user(t, "organiser"), cp.ID)
			if d.ReviewReason != "" {
				return
			}
		}
		t.Fatalf("expected pot to escalate to manual review after repeated refund failures")
	})
}

// Validation bounds: past deadline, deadline exceeding POT_MAX_HOLD, min/max target
// bounds, invalid suggested shares and URL-containing phishing notes.
func TestPotValidationBounds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		ownerID := e.user(t, "organiser")
		m := e.register(t, "organiser", "45012345678")

		// Past deadline.
		if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID: ownerID, MeterID: m.ID, TargetMinor: 200000, Deadline: e.clock.Add(-time.Minute),
		}); !errors.Is(err, pots.ErrInvalidDeadline) {
			t.Fatalf("past deadline: got %v, want ErrInvalidDeadline", err)
		}
		// Exceeds POT_MAX_HOLD (default 72h).
		if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID: ownerID, MeterID: m.ID, TargetMinor: 200000, Deadline: e.clock.Add(73 * time.Hour),
		}); !errors.Is(err, pots.ErrInvalidDeadline) {
			t.Fatalf(">72h deadline: got %v, want ErrInvalidDeadline", err)
		}
		// Below MinAmountMinor (50000) + FeeMinor (10000) = 60000.
		if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID: ownerID, MeterID: m.ID, TargetMinor: 59999, Deadline: e.clock.Add(time.Hour),
		}); !errors.Is(err, orchestrator.ErrAmountOutOfRange) {
			t.Fatalf("below min target: got %v, want ErrAmountOutOfRange", err)
		}
		// Above MaxAmountMinor (50000000) + FeeMinor (10000) = 50010000.
		if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID: ownerID, MeterID: m.ID, TargetMinor: 50010001, Deadline: e.clock.Add(time.Hour),
		}); !errors.Is(err, orchestrator.ErrAmountOutOfRange) {
			t.Fatalf("above max target: got %v, want ErrAmountOutOfRange", err)
		}
		// Invalid suggested shares (1 and 51).
		for _, badShares := range []int{-1, 1, 51} {
			if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
				OwnerID: ownerID, MeterID: m.ID, TargetMinor: 200000, SuggestedShares: badShares, Deadline: e.clock.Add(time.Hour),
			}); !errors.Is(err, pots.ErrInvalidShares) {
				t.Fatalf("shares=%d: got %v, want ErrInvalidShares", badShares, err)
			}
		}
		// Anti-phishing URL in note.
		if _, err := e.o.CreatePot(e.ctx, orchestrator.CreatePotParams{
			OwnerID: ownerID, MeterID: m.ID, TargetMinor: 200000, Deadline: e.clock.Add(time.Hour), Note: "Pay at https://evil.example",
		}); !errors.Is(err, payrequests.ErrInvalidNote) {
			t.Fatalf("phishing note: got %v, want ErrInvalidNote", err)
		}
	})
}
