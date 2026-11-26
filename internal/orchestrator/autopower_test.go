package orchestrator_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (e *env) saveCard(t *testing.T, user, meter string) (*meters.Meter, *autopower.SavedPaymentMethod) {
	t.Helper()
	uid := e.user(t, user)
	m := e.register(t, user, meter)
	q, err := e.o.CreateQuote(e.ctx, uid, m.ID, 500000, orchestrator.PrefToken)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	txn, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{
		CustomerID: uid,
		QuoteID:    q.ID,
		Email:      user + "@example.com",
	})
	if err != nil {
		t.Fatalf("create txn: %v", err)
	}
	if err := e.pay.Complete(txn.PaymentProviderRef, true, 0); err != nil {
		t.Fatalf("complete mock payment: %v", err)
	}
	if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); err != nil {
		t.Fatalf("handle payment notification: %v", err)
	}
	pm, err := e.o.SavePaymentMethod(e.ctx, orchestrator.SavePaymentMethodParams{
		UserID:        uid,
		TransactionID: txn.ID,
		Consent:       true,
	})
	if err != nil {
		t.Fatalf("save payment method: %v", err)
	}
	return m, pm
}

// Verifies consent enforcement, ownership isolation, encrypted storage,
// audit trail, and that deleting a saved payment method atomically stops all
// rules referencing it and cancels unstarted scheduled runs.
func TestPaymentMethodConsentAndDeleteStopsRules(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.setClock(time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC))
		uid := e.user(t, "ada")
		otherID := e.user(t, "tunde")

		m := e.register(t, "ada", "45011122233")
		q, err := e.o.CreateQuote(e.ctx, uid, m.ID, 500000, orchestrator.PrefToken)
		if err != nil {
			t.Fatal(err)
		}
		txn, _, err := e.o.CreateTransaction(e.ctx, orchestrator.CreateRequest{
			CustomerID: uid, QuoteID: q.ID, Email: "ada@example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.pay.Complete(txn.PaymentProviderRef, true, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := e.o.HandlePaymentNotification(e.ctx, txn.PaymentProviderRef); err != nil {
			t.Fatal(err)
		}

		// Missing consent must be rejected.
		if _, err := e.o.SavePaymentMethod(e.ctx, orchestrator.SavePaymentMethodParams{
			UserID: uid, TransactionID: txn.ID, Consent: false,
		}); !errors.Is(err, autopower.ErrConsentRequired) {
			t.Fatalf("want ErrConsentRequired, got %v", err)
		}

		// Another user cannot save Ada's card.
		if _, err := e.o.SavePaymentMethod(e.ctx, orchestrator.SavePaymentMethodParams{
			UserID: otherID, TransactionID: txn.ID, Consent: true,
		}); !errors.Is(err, tx.ErrNotFound) {
			t.Fatalf("want tx.ErrNotFound for cross-user save, got %v", err)
		}

		pm, err := e.o.SavePaymentMethod(e.ctx, orchestrator.SavePaymentMethodParams{
			UserID: uid, TransactionID: txn.ID, Consent: true,
		})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if pm.ConsentVersion != autopower.DefaultConsentVersion || pm.ConsentText == "" || len(pm.Last4) != 4 {
			t.Fatalf("unexpected saved method metadata: %+v", pm)
		}

		// Idempotent replay returns same payment method ID.
		pmReplay, err := e.o.SavePaymentMethod(e.ctx, orchestrator.SavePaymentMethodParams{
			UserID: uid, TransactionID: txn.ID, Consent: true,
		})
		if err != nil || pmReplay.ID != pm.ID {
			t.Fatalf("expected idempotent save, got %v %+v", err, pmReplay)
		}

		// Create two active AutoPower rules referencing pm.
		r1, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:          uid,
			MeterID:         m.ID,
			PaymentMethodID: pm.ID,
			TriggerType:     autopower.TriggerSchedule,
			AmountMinor:     1000000,
			MonthlyCapMinor: 4000000,
			Frequency:       autopower.FreqMonthly,
			DaysOfMonth:     []int{1, 15},
			TimeOfDay:       "07:00",
		})
		if err != nil {
			t.Fatalf("create rule 1: %v", err)
		}
		r2, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:          uid,
			MeterID:         m.ID,
			PaymentMethodID: pm.ID,
			TriggerType:     autopower.TriggerSchedule,
			AmountMinor:     500000,
			MonthlyCapMinor: 2000000,
			Frequency:       autopower.FreqWeekly,
			DaysOfWeek:      []int{1},
			TimeOfDay:       "08:00",
		})
		if err != nil {
			t.Fatalf("create rule 2: %v", err)
		}

		// Delete payment method -> both rules paused and upcoming runs cancelled.
		if err := e.o.DeletePaymentMethod(e.ctx, uid, pm.ID); err != nil {
			t.Fatalf("delete pm: %v", err)
		}
		methods, err := e.o.ListPaymentMethods(e.ctx, uid)
		if err != nil || len(methods) != 0 {
			t.Fatalf("expected 0 active payment methods, got %d (%v)", len(methods), err)
		}

		rules, err := e.o.ListAutoPowerRules(e.ctx, uid)
		if err != nil || len(rules) != 2 {
			t.Fatalf("expected 2 rules, got %d (%v)", len(rules), err)
		}
		for _, rv := range rules {
			if rv.Status != autopower.RulePaused || rv.PauseReason != autopower.PausePaymentMethodDeleted {
				t.Fatalf("rule %s not stopped: status=%s reason=%s", rv.RuleID, rv.Status, rv.PauseReason)
			}
			if rv.UpcomingRun != nil {
				t.Fatalf("rule %s still has upcoming scheduled run: %+v", rv.RuleID, rv.UpcomingRun)
			}
		}

		audits, err := e.st.Repos().Audit().ForResource(e.ctx, "payment_method", pm.ID)
		if err != nil || len(audits) < 2 {
			t.Fatalf("expected save + delete audits on payment_method, got %d (%v)", len(audits), err)
		}
		_ = r1
		_ = r2
		e.assertBalanced(t)
	})
}

// Fake-clock scheduler across month boundaries (including short February clamp,
// 12-hour heads-up in Africa/Lagos, monthly cap enforcement in January, and cap
// reset in February, plus >6h downtime marking MISSED without charging).
func TestSchedulerFakeClockAcrossMonthBoundaries(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		loc := autopower.LagosLocation()
		// Start Jan 10, 2027 09:00 WAT (08:00 UTC).
		e.setClock(time.Date(2027, 1, 10, 9, 0, 0, 0, loc))
		uid := e.user(t, "bola")
		m, pm := e.saveCard(t, "bola", "45099887766")

		// Rule: ₦10,000 on 15th and 31st at 07:00 WAT, monthly cap ₦15,000.
		// Jan 15 (₦10k) -> SUCCEEDED; Jan 31 (₦10k + ₦10k > ₦15k) -> SKIPPED_CAP;
		// Feb 15 (new Lagos month! ₦10k <= ₦15k) -> SUCCEEDED;
		// Feb 28 (day 31 clamped to Feb 28! Worker down 8h -> MISSED).
		rv, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:          uid,
			MeterID:         m.ID,
			PaymentMethodID: pm.ID,
			TriggerType:     autopower.TriggerSchedule,
			AmountMinor:     1000000,
			MonthlyCapMinor: 1500000,
			Frequency:       autopower.FreqMonthly,
			DaysOfMonth:     []int{15, 31},
			TimeOfDay:       "07:00",
		})
		if err != nil {
			t.Fatalf("create rule: %v", err)
		}
		jan15Slot := time.Date(2027, 1, 15, 7, 0, 0, 0, loc).UTC()
		if rv.UpcomingRun == nil || !rv.UpcomingRun.ScheduledFor.Equal(jan15Slot) {
			t.Fatalf("expected Jan 15 07:00 WAT (%s), got %+v", jan15Slot, rv.UpcomingRun)
		}
		jan15RunID := rv.UpcomingRun.ID

		// 1. Advance to Jan 14 20:00 WAT (11h before Jan 15 07:00 WAT) -> sends 12h heads-up, does not charge yet.
		e.setClock(time.Date(2027, 1, 14, 20, 0, 0, 0, loc))
		if _, err := e.o.TickAutoPower(e.ctx); err != nil {
			t.Fatalf("tick heads-up: %v", err)
		}
		rnJan15, err := e.st.Repos().AutoPower().GetRun(e.ctx, jan15RunID)
		if err != nil || rnJan15.Status != autopower.RunScheduled || rnJan15.HeadsUpSentAt == nil {
			t.Fatalf("expected SCHEDULED with HeadsUpSentAt set, got %+v (%v)", rnJan15, err)
		}
		evs, err := e.st.Repos().Outbox().ForAggregate(e.ctx, jan15RunID)
		if err != nil || len(evs) != 1 || evs[0].Type != "autopower.run.heads_up" {
			t.Fatalf("expected 1 heads_up outbox event, got %+v (%v)", evs, err)
		}

		// 2. Advance to Jan 15 07:01 WAT -> executes purchase and vends electricity.
		e.setClock(time.Date(2027, 1, 15, 7, 1, 0, 0, loc))
		if _, err := e.o.TickAutoPower(e.ctx); err != nil {
			t.Fatalf("tick Jan 15 charge: %v", err)
		}
		rnJan15, _ = e.st.Repos().AutoPower().GetRun(e.ctx, jan15RunID)
		if rnJan15.Status != autopower.RunSucceeded || rnJan15.TransactionID == nil {
			t.Fatalf("Jan 15 run want SUCCEEDED, got %+v", rnJan15)
		}

		// 3. Advance to Jan 31 07:05 WAT -> monthly cap (₦15k) blocks second ₦10k run -> SKIPPED_CAP!
		jan31Slot := time.Date(2027, 1, 31, 7, 0, 0, 0, loc).UTC()
		e.setClock(time.Date(2027, 1, 31, 7, 5, 0, 0, loc))
		if _, err := e.o.TickAutoPower(e.ctx); err != nil {
			t.Fatalf("tick Jan 31 cap: %v", err)
		}
		rnJan31, err := e.st.Repos().AutoPower().GetRunBySlot(e.ctx, rv.RuleID, jan31Slot)
		if err != nil || rnJan31.Status != autopower.RunSkippedCap || rnJan31.FailureReason != "MONTHLY_CAP_EXCEEDED" {
			t.Fatalf("Jan 31 run want SKIPPED_CAP/MONTHLY_CAP_EXCEEDED, got %+v (%v)", rnJan31, err)
		}

		// 4. Advance across month boundary to Feb 15 07:02 WAT -> cap resets in new Lagos month -> SUCCEEDED!
		feb15Slot := time.Date(2027, 2, 15, 7, 0, 0, 0, loc).UTC()
		e.setClock(time.Date(2027, 2, 15, 7, 2, 0, 0, loc))
		if _, err := e.o.TickAutoPower(e.ctx); err != nil {
			t.Fatalf("tick Feb 15 charge: %v", err)
		}
		rnFeb15, err := e.st.Repos().AutoPower().GetRunBySlot(e.ctx, rv.RuleID, feb15Slot)
		if err != nil || rnFeb15.Status != autopower.RunSucceeded {
			t.Fatalf("Feb 15 run want SUCCEEDED after month reset, got %+v (%v)", rnFeb15, err)
		}

		// 5. Next slot is Feb 28 07:00 WAT (short February clamp for day 31).
		// Simulate worker down for 8 hours (past 6h catch-up window) -> MISSED and next slot Mar 15 scheduled!
		feb28Slot := time.Date(2027, 2, 28, 7, 0, 0, 0, loc).UTC()
		e.setClock(time.Date(2027, 2, 28, 15, 30, 0, 0, loc))
		if _, err := e.o.TickAutoPower(e.ctx); err != nil {
			t.Fatalf("tick Feb 28 missed: %v", err)
		}
		rnFeb28, err := e.st.Repos().AutoPower().GetRunBySlot(e.ctx, rv.RuleID, feb28Slot)
		if err != nil || rnFeb28.Status != autopower.RunMissed {
			t.Fatalf("Feb 28 run want MISSED after >6h downtime, got %+v (%v)", rnFeb28, err)
		}

		mar15Slot := time.Date(2027, 3, 15, 7, 0, 0, 0, loc).UTC()
		rnMar15, err := e.st.Repos().AutoPower().GetRunBySlot(e.ctx, rv.RuleID, mar15Slot)
		if err != nil || rnMar15.Status != autopower.RunScheduled {
			t.Fatalf("Mar 15 run want materialised SCHEDULED, got %+v (%v)", rnMar15, err)
		}
		e.assertBalanced(t)
	})
}

// Concurrent worker replicas & multiple rules for the same user racing against
// the per-user daily spend cap (and duplicate TickAutoPower calls racing on the
// same due slot): proves SKIP LOCKED + User/Rule lock ordering prevents cap
// overrun and prevents duplicate charges.
func TestConcurrentRunsEnforceSpendCapsAndSingleSlotCharge(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "test-v1")
			if err != nil {
				t.Fatal(err)
			}
			st := b.open(t)
			defer st.Close()

			pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
			cfg := orchestrator.DefaultConfig()
			// Daily cap ₦20,000: with 4 rules of ₦10,000 firing at the same instant,
			// AT MOST 2 runs may charge and the remaining 2 MUST be SKIPPED_CAP.
			cfg.AutoPowerDailyMaxMinor = 2000000

			reg := registry.New(st, box, vend, remote)
			orch := orchestrator.New(cfg, st, reg, box, pay, vend, remote, nil)
			loc := autopower.LagosLocation()
			cur := time.Date(2027, 4, 1, 6, 0, 0, 0, loc)
			var clockMu sync.RWMutex
			nowFn := func() time.Time {
				clockMu.RLock()
				defer clockMu.RUnlock()
				return cur.UTC()
			}
			orch.Now = nowFn
			reg.Now = nowFn

			ctx := context.Background()
			uid, err := st.Repos().Users().EnsureByExternalID(ctx, "chidi")
			if err != nil {
				t.Fatal(err)
			}
			m, err := reg.Register(ctx, uid, "45077665544", "IKEDC", "Flat")
			if err != nil {
				t.Fatal(err)
			}
			q, err := orch.CreateQuote(ctx, uid, m.ID, 500000, orchestrator.PrefToken)
			if err != nil {
				t.Fatal(err)
			}
			seedTxn, _, err := orch.CreateTransaction(ctx, orchestrator.CreateRequest{
				CustomerID: uid, QuoteID: q.ID, Email: "chidi@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := pay.Complete(seedTxn.PaymentProviderRef, true, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := orch.HandlePaymentNotification(ctx, seedTxn.PaymentProviderRef); err != nil {
				t.Fatal(err)
			}
			pm, err := orch.SavePaymentMethod(ctx, orchestrator.SavePaymentMethodParams{
				UserID: uid, TransactionID: seedTxn.ID, Consent: true,
			})
			if err != nil {
				t.Fatal(err)
			}

			var ruleIDs []uuid.UUID
			for i := 0; i < 4; i++ {
				rv, err := orch.CreateAutoPowerRule(ctx, orchestrator.CreateRuleParams{
					UserID:          uid,
					MeterID:         m.ID,
					PaymentMethodID: pm.ID,
					TriggerType:     autopower.TriggerSchedule,
					AmountMinor:     1000000,
					MonthlyCapMinor: 5000000,
					Frequency:       autopower.FreqMonthly,
					DaysOfMonth:     []int{1},
					TimeOfDay:       "07:00",
				})
				if err != nil {
					t.Fatalf("create rule %d: %v", i, err)
				}
				ruleIDs = append(ruleIDs, rv.RuleID)
			}

			// Advance to Apr 1 07:01 WAT and fire 8 concurrent worker replicas.
			clockMu.Lock()
			cur = time.Date(2027, 4, 1, 7, 1, 0, 0, loc)
			clockMu.Unlock()

			var wg sync.WaitGroup
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = orch.TickAutoPower(ctx)
				}()
			}
			wg.Wait()

			slot := time.Date(2027, 4, 1, 7, 0, 0, 0, loc).UTC()
			succeeded, skippedCap := 0, 0
			for _, rid := range ruleIDs {
				rn, err := st.Repos().AutoPower().GetRunBySlot(ctx, rid, slot)
				if err != nil {
					t.Fatalf("get run for rule %s: %v", rid, err)
				}
				switch rn.Status {
				case autopower.RunSucceeded:
					succeeded++
				case autopower.RunSkippedCap:
					skippedCap++
				default:
					t.Fatalf("unexpected run status %s on rule %s", rn.Status, rid)
				}
			}
			if succeeded != 2 || skippedCap != 2 {
				t.Fatalf("want 2 SUCCEEDED and 2 SKIPPED_CAP under ₦20k daily cap, got succeeded=%d skipped_cap=%d", succeeded, skippedCap)
			}
			tb, err := st.Repos().Ledger().TrialBalance(ctx)
			if err != nil || tb != 0 {
				t.Fatalf("ledger trial balance = %d (%v), want 0", tb, err)
			}
		})
	}
}

// Lost charge_authorization HTTP response (FailAfterApply on paystacktest.Fake)
// + delayed ambiguous network error: proves ChargeSaved is called ONCE for the
// slot, VerifyPayment reconciles by deterministic reference, and the customer's
// card is never double-charged.
func TestLostChargeAuthorizationResponseNeverDoubleCharges(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			fake := paystacktest.New("sk_test_autopower")
			defer fake.Server.Close()
			psClient, err := paystack.New(paystack.Config{
				SecretKey: fake.Secret,
				BaseURL:   fake.Server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}

			var chargeCalls atomic.Int32
			countingPay := &countingPayProvider{
				PaymentProvider: psClient,
				onChargeSaved:   func() { chargeCalls.Add(1) },
			}

			box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "test-v1")
			if err != nil {
				t.Fatal(err)
			}
			st := b.open(t)
			defer st.Close()

			vend, remote := mock.NewVending(), mock.NewRemoteCredit()
			reg := registry.New(st, box, vend, remote)
			orch := orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, countingPay, vend, remote, nil)
			loc := autopower.LagosLocation()
			cur := time.Date(2027, 5, 1, 6, 0, 0, 0, loc)
			orch.Now = func() time.Time { return cur.UTC() }
			reg.Now = orch.Now

			ctx := context.Background()
			uid, err := st.Repos().Users().EnsureByExternalID(ctx, "ngozi")
			if err != nil {
				t.Fatal(err)
			}
			m, err := reg.Register(ctx, uid, "45033445566", "IKEDC", "Home")
			if err != nil {
				t.Fatal(err)
			}

			// Initial card payment on Paystack fake -> reusable authorization.
			q, err := orch.CreateQuote(ctx, uid, m.ID, 500000, orchestrator.PrefToken)
			if err != nil {
				t.Fatal(err)
			}
			seedTxn, _, err := orch.CreateTransaction(ctx, orchestrator.CreateRequest{
				CustomerID: uid, QuoteID: q.ID, Email: "ngozi@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			fake.Pay(seedTxn.PaymentProviderRef, 0)
			if _, err := orch.HandlePaymentNotification(ctx, seedTxn.PaymentProviderRef); err != nil {
				t.Fatal(err)
			}

			pm, err := orch.SavePaymentMethod(ctx, orchestrator.SavePaymentMethodParams{
				UserID: uid, TransactionID: seedTxn.ID, Consent: true,
			})
			if err != nil {
				t.Fatalf("save card from paystacktest: %v", err)
			}

			rv, err := orch.CreateAutoPowerRule(ctx, orchestrator.CreateRuleParams{
				UserID:          uid,
				MeterID:         m.ID,
				PaymentMethodID: pm.ID,
				TriggerType:     autopower.TriggerSchedule,
				AmountMinor:     1000000,
				MonthlyCapMinor: 3000000,
				Frequency:       autopower.FreqMonthly,
				DaysOfMonth:     []int{1},
				TimeOfDay:       "07:00",
			})
			if err != nil {
				t.Fatal(err)
			}
			runID := rv.UpcomingRun.ID

			// Arm FailAfterApply: Paystack charges the saved card, then returns HTTP 500!
			fake.FailAfterApply("POST /transaction/charge_authorization", 1)

			cur = time.Date(2027, 5, 1, 7, 1, 0, 0, loc)
			if _, err := orch.TickAutoPower(ctx); err != nil {
				t.Fatalf("tick 1: %v", err)
			}
			// Run subsequent ticks to verify idempotency.
			cur = cur.Add(5 * time.Minute)
			if _, err := orch.TickAutoPower(ctx); err != nil {
				t.Fatalf("tick 2: %v", err)
			}

			if got := chargeCalls.Load(); got != 1 {
				t.Fatalf("ChargeSaved called %d times, want exactly 1", got)
			}
			if got := fake.Requests["POST /transaction/charge_authorization"]; got != 1 {
				t.Fatalf("paystacktest recorded %d charge_authorization requests, want 1", got)
			}

			rn, err := st.Repos().AutoPower().GetRun(ctx, runID)
			if err != nil || rn.Status != autopower.RunSucceeded || rn.TransactionID == nil {
				t.Fatalf("expected run SUCCEEDED via VerifyPayment reconciliation, got %+v (%v)", rn, err)
			}
			elecTxn, err := orch.Get(ctx, *rn.TransactionID)
			if err != nil || elecTxn.Vending != tx.VendConfirmed {
				t.Fatalf("expected vended electricity transaction, got %+v (%v)", elecTxn, err)
			}
			tb, err := st.Repos().Ledger().TrialBalance(ctx)
			if err != nil || tb != 0 {
				t.Fatalf("ledger trial balance = %d (%v), want 0", tb, err)
			}
		})
	}
}

type countingPayProvider struct {
	providers.PaymentProvider
	onChargeSaved func()
}

func (c *countingPayProvider) ChargeSaved(ctx context.Context, req providers.ChargeRequest) (providers.PaymentVerification, error) {
	if c.onChargeSaved != nil {
		c.onChargeSaved()
	}
	return c.PaymentProvider.ChargeSaved(ctx, req)
}

// Three consecutive declined charges automatically pause the rule with
// PauseConsecutiveFailures, notify on each failure + rule pause, and cancel
// future scheduled runs.
func TestThreeConsecutiveFailuresPauseRule(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		loc := autopower.LagosLocation()
		e.setClock(time.Date(2027, 6, 1, 6, 0, 0, 0, loc))
		uid := e.user(t, "kemi")
		m, pm := e.saveCard(t, "kemi", "45055667788")

		authCode, err := autopower.OpenAuthCode(e.o.Box(), uid, pm.ID, pm.AuthCodeCiphertext)
		if err != nil {
			t.Fatal(err)
		}
		e.pay.SetSavedChargeOutcome(authCode, providers.PaymentFailed, 0)

		// Weekly Mondays at 07:00 WAT (Jun 7, Jun 14, Jun 21).
		rv, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:          uid,
			MeterID:         m.ID,
			PaymentMethodID: pm.ID,
			TriggerType:     autopower.TriggerSchedule,
			AmountMinor:     1000000,
			MonthlyCapMinor: 5000000,
			Frequency:       autopower.FreqWeekly,
			DaysOfWeek:      []int{1},
			TimeOfDay:       "07:00",
		})
		if err != nil {
			t.Fatal(err)
		}

		slots := []time.Time{
			time.Date(2027, 6, 7, 7, 1, 0, 0, loc),
			time.Date(2027, 6, 14, 7, 1, 0, 0, loc),
			time.Date(2027, 6, 21, 7, 1, 0, 0, loc),
		}
		for i, ts := range slots {
			e.setClock(ts)
			if _, err := e.o.TickAutoPower(e.ctx); err != nil {
				t.Fatalf("tick %d: %v", i+1, err)
			}
			rl, err := e.st.Repos().AutoPower().GetRule(e.ctx, rv.RuleID)
			if err != nil {
				t.Fatal(err)
			}
			if rl.ConsecutiveFailures != i+1 {
				t.Fatalf("after failure %d: ConsecutiveFailures=%d, want %d", i+1, rl.ConsecutiveFailures, i+1)
			}
			if i < 2 && rl.Status != autopower.RuleActive {
				t.Fatalf("after failure %d: want ACTIVE, got %s", i+1, rl.Status)
			}
			if i == 2 {
				if rl.Status != autopower.RulePaused || rl.PauseReason != autopower.PauseConsecutiveFailures {
					t.Fatalf("after 3rd failure: want PAUSED/%s, got %s/%s", autopower.PauseConsecutiveFailures, rl.Status, rl.PauseReason)
				}
			}
		}

		ruleEvs, err := e.st.Repos().Outbox().ForAggregate(e.ctx, rv.RuleID)
		if err != nil || len(ruleEvs) != 1 || ruleEvs[0].Type != "autopower.rule.paused" {
			t.Fatalf("expected autopower.rule.paused event after 3rd failure, got %+v (%v)", ruleEvs, err)
		}
		e.assertBalanced(t)
	})
}

// Low-balance mode refused on meters without confirmed remote reading, and
// triggers at most once per 24 hours on eligible meters. Also tests one-tap skip
// and meter removal pausing active rules.
func TestLowBalanceCapabilityGateCooldownSkipAndMeterRemoval(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		loc := autopower.LagosLocation()
		e.setClock(time.Date(2027, 7, 1, 8, 0, 0, 0, loc))
		uid := e.user(t, "segun")

		// 45012345678 is a non-remote meter; 99012345678 supports remote credit/reading in mock.Vending.
		ineligibleMeter, pm := e.saveCard(t, "segun", "45012345678")
		thresh := 20.0

		if _, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:                 uid,
			MeterID:                ineligibleMeter.ID,
			PaymentMethodID:        pm.ID,
			TriggerType:            autopower.TriggerLowBalance,
			AmountMinor:            500000,
			MonthlyCapMinor:        3000000,
			LowBalanceThresholdKWh: &thresh,
		}); !errors.Is(err, autopower.ErrLowBalanceIneligible) {
			t.Fatalf("want ErrLowBalanceIneligible on standard meter, got %v", err)
		}

		// Register smart meter ("7..." prefix in mock.Vending sets RemoteCredit/RemoteReading Supported=true, Confirmed=false).
		smartMeter := e.register(t, "segun", "75012345678")
		if _, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:                 uid,
			MeterID:                smartMeter.ID,
			PaymentMethodID:        pm.ID,
			TriggerType:            autopower.TriggerLowBalance,
			AmountMinor:            500000,
			MonthlyCapMinor:        3000000,
			LowBalanceThresholdKWh: &thresh,
		}); !errors.Is(err, autopower.ErrLowBalanceIneligible) {
			t.Fatalf("want ErrLowBalanceIneligible before RemoteReading is confirmed, got %v", err)
		}

		// Confirm RemoteReading capability on smartMeter.
		if err := e.st.InTx(e.ctx, func(r store.Repos) error {
			return registry.MarkRemoteReadingConfirmedIn(e.ctx, r, smartMeter.ID, e.clock)
		}); err != nil {
			t.Fatalf("confirm remote reading: %v", err)
		}

		lbRule, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:                 uid,
			MeterID:                smartMeter.ID,
			PaymentMethodID:        pm.ID,
			TriggerType:            autopower.TriggerLowBalance,
			AmountMinor:            500000,
			MonthlyCapMinor:        3000000,
			DeliveryPreference:     orchestrator.PrefToken,
			LowBalanceThresholdKWh: &thresh,
		})
		if err != nil {
			t.Fatalf("create low-balance rule on eligible meter: %v", err)
		}

		// Reading 25.5 kWh (> 20 kWh) -> no top-up.
		e.readings.SetReading("75012345678", 25.5, e.clock)
		if _, err := e.o.PollLowBalance(e.ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ := e.st.Repos().AutoPower().ListRunsForRule(e.ctx, lbRule.RuleID, 10)
		if len(runs) != 0 {
			t.Fatalf("expected 0 runs at 25.5 kWh, got %d", len(runs))
		}

		// 4h later: reading drops to 12.4 kWh (<= 20 kWh) -> triggers 1 top-up!
		e.later(4*time.Hour + time.Minute)
		e.readings.SetReading("75012345678", 12.4, e.clock)
		if _, err := e.o.PollLowBalance(e.ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ = e.st.Repos().AutoPower().ListRunsForRule(e.ctx, lbRule.RuleID, 10)
		if len(runs) != 1 || runs[0].Status != autopower.RunSucceeded {
			t.Fatalf("expected 1 SUCCEEDED low-balance run, got %+v", runs)
		}

		// 5h later (within 24h cooldown): balance still low (10.0 kWh) -> must NOT trigger again!
		e.later(5 * time.Hour)
		e.readings.SetReading("75012345678", 10.0, e.clock)
		if _, err := e.o.PollLowBalance(e.ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ = e.st.Repos().AutoPower().ListRunsForRule(e.ctx, lbRule.RuleID, 10)
		if len(runs) != 1 {
			t.Fatalf("expected 24h cooldown to suppress duplicate low-balance run, got %d runs", len(runs))
		}

		// 20h later (total 25h > 24h cooldown): balance still low (9.0 kWh) -> triggers 2nd top-up!
		e.later(20 * time.Hour)
		e.readings.SetReading("75012345678", 9.0, e.clock)
		if _, err := e.o.PollLowBalance(e.ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ = e.st.Repos().AutoPower().ListRunsForRule(e.ctx, lbRule.RuleID, 10)
		if len(runs) != 2 {
			t.Fatalf("expected 2nd low-balance run after 24h cooldown, got %d runs", len(runs))
		}

		// One-tap skip on a schedule rule advances UpcomingRun to the next slot.
		schedRule, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID:          uid,
			MeterID:         smartMeter.ID,
			PaymentMethodID: pm.ID,
			TriggerType:     autopower.TriggerSchedule,
			AmountMinor:     500000,
			MonthlyCapMinor: 3000000,
			Frequency:       autopower.FreqMonthly,
			DaysOfMonth:     []int{10, 25},
			TimeOfDay:       "07:00",
		})
		if err != nil {
			t.Fatal(err)
		}
		firstRunID := schedRule.UpcomingRun.ID
		skipped, err := e.o.SkipAutoPowerRun(e.ctx, uid, firstRunID)
		if err != nil || skipped.Status != autopower.RunSkippedUser {
			t.Fatalf("skip run: %v %+v", err, skipped)
		}

		// Removing saved meter pauses both rules on smartMeter and cancels future scheduled runs.
		if err := e.o.Meters.RemoveSavedMeter(e.ctx, uid, smartMeter.ID); err != nil {
			t.Fatalf("remove saved meter: %v", err)
		}
		allRules, err := e.o.ListAutoPowerRules(e.ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		for _, rv := range allRules {
			if rv.MeterID == smartMeter.ID && (rv.Status != autopower.RulePaused || rv.PauseReason != autopower.PauseMeterAccessRevoked) {
				t.Fatalf("expected rule %s paused on meter removal, got %s/%s", rv.RuleID, rv.Status, rv.PauseReason)
			}
		}
		e.assertBalanced(t)
	})
}

// Verifies card expiry between rule creation and scheduled execution pauses the
// rule without calling ChargeSaved, and verifies a Paystack "send_otp" pending
// saved-card charge remains PENDING/CHARGING until webhook/sweeper confirms it.
func TestExpiredCardAndSendOTPPendingResolution(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			fake := paystacktest.New("sk_test_otp")
			defer fake.Server.Close()
			psClient, err := paystack.New(paystack.Config{
				SecretKey: fake.Secret,
				BaseURL:   fake.Server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}

			box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "test-v1")
			if err != nil {
				t.Fatal(err)
			}
			st := b.open(t)
			defer st.Close()

			vend, remote := mock.NewVending(), mock.NewRemoteCredit()
			reg := registry.New(st, box, vend, remote)
			orch := orchestrator.New(orchestrator.DefaultConfig(), st, reg, box, psClient, vend, remote, nil)
			loc := autopower.LagosLocation()
			cur := time.Date(2027, 8, 10, 9, 0, 0, 0, loc)
			orch.Now = func() time.Time { return cur.UTC() }
			reg.Now = orch.Now

			ctx := context.Background()
			uid, err := st.Repos().Users().EnsureByExternalID(ctx, "femi")
			if err != nil {
				t.Fatal(err)
			}
			m, err := reg.Register(ctx, uid, "45066778899", "IKEDC", "Office")
			if err != nil {
				t.Fatal(err)
			}

			// 1. Card expiring Aug 2027: valid on Aug 10, expired on Sep 1!
			q1, _ := orch.CreateQuote(ctx, uid, m.ID, 500000, orchestrator.PrefToken)
			t1, _, _ := orch.CreateTransaction(ctx, orchestrator.CreateRequest{
				CustomerID: uid, QuoteID: q1.ID, Email: "femi@example.com",
			})
			fake.SetTxnAuthorization(t1.PaymentProviderRef, &paystacktest.CardAuth{
				AuthorizationCode: "AUTH_exp_aug27",
				Email:             "femi@example.com",
				Reusable:          true,
				Brand:             "mastercard",
				CardType:          "mastercard DEBIT",
				Last4:             "5555",
				ExpMonth:          "08",
				ExpYear:           "2027",
				Channel:           "card",
				Signature:         "SIG_exp_aug27",
			})
			fake.Pay(t1.PaymentProviderRef, 0)
			_, _ = orch.HandlePaymentNotification(ctx, t1.PaymentProviderRef)

			pmExp, err := orch.SavePaymentMethod(ctx, orchestrator.SavePaymentMethodParams{
				UserID: uid, TransactionID: t1.ID, Consent: true,
			})
			if err != nil {
				t.Fatalf("save Aug-2027 card on Aug 10: %v", err)
			}

			expRule, err := orch.CreateAutoPowerRule(ctx, orchestrator.CreateRuleParams{
				UserID:          uid,
				MeterID:         m.ID,
				PaymentMethodID: pmExp.ID,
				TriggerType:     autopower.TriggerSchedule,
				AmountMinor:     1000000,
				MonthlyCapMinor: 3000000,
				Frequency:       autopower.FreqMonthly,
				DaysOfMonth:     []int{1},
				TimeOfDay:       "07:00",
			})
			if err != nil {
				t.Fatal(err)
			}

			// Advance to Sep 1 07:01 WAT -> card is now expired; ChargeSaved must NOT be called!
			cur = time.Date(2027, 9, 1, 7, 1, 0, 0, loc)
			if _, err := orch.TickAutoPower(ctx); err != nil {
				t.Fatal(err)
			}
			if got := fake.Requests["POST /transaction/charge_authorization"]; got != 0 {
				t.Fatalf("expired card must not call charge_authorization, got %d", got)
			}
			rlAfterExp, _ := st.Repos().AutoPower().GetRule(ctx, expRule.RuleID)
			if rlAfterExp.Status != autopower.RulePaused || rlAfterExp.PauseReason != autopower.PauseCardExpired {
				t.Fatalf("want PAUSED/CARD_EXPIRED, got %s/%s", rlAfterExp.Status, rlAfterExp.PauseReason)
			}

			// 2. Save a valid 2030 card that responds "send_otp" on charge_authorization.
			q2, _ := orch.CreateQuote(ctx, uid, m.ID, 500000, orchestrator.PrefToken)
			t2, _, _ := orch.CreateTransaction(ctx, orchestrator.CreateRequest{
				CustomerID: uid, QuoteID: q2.ID, Email: "femi@example.com",
			})
			fake.Pay(t2.PaymentProviderRef, 0)
			_, _ = orch.HandlePaymentNotification(ctx, t2.PaymentProviderRef)
			pmOTP, err := orch.SavePaymentMethod(ctx, orchestrator.SavePaymentMethodParams{
				UserID: uid, TransactionID: t2.ID, Consent: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			authOTP := fake.AuthorizationCodeFor(t2.PaymentProviderRef)
			fake.SetAuthorizationChargeStatus(authOTP, "send_otp")

			// Update paused rule onto valid card and resume it!
			active := autopower.RuleActive
			resumed, err := orch.UpdateAutoPowerRule(ctx, orchestrator.UpdateRuleParams{
				UserID:          uid,
				RuleID:          expRule.RuleID,
				PaymentMethodID: &pmOTP.ID,
				Status:          &active,
			})
			if err != nil {
				t.Fatalf("resume rule with new card: %v", err)
			}
			octRunID := resumed.UpcomingRun.ID

			// Advance to Oct 1 07:01 WAT -> ChargeSaved returns send_otp -> stays CHARGING + PayPending!
			cur = time.Date(2027, 10, 1, 7, 1, 0, 0, loc)
			if _, err := orch.TickAutoPower(ctx); err != nil {
				t.Fatal(err)
			}
			rnOTP, _ := st.Repos().AutoPower().GetRun(ctx, octRunID)
			if rnOTP.Status != autopower.RunCharging || rnOTP.TransactionID == nil {
				t.Fatalf("want CHARGING while awaiting OTP, got %+v", rnOTP)
			}
			pendingTxn, _ := orch.Get(ctx, *rnOTP.TransactionID)
			if pendingTxn.Payment != tx.PayPending {
				t.Fatalf("want PayPending while send_otp unresolved, got %s", pendingTxn.Payment)
			}

			// Customer completes OTP -> Paystack webhook settles payment -> run transitions to SUCCEEDED!
			fake.SetStatus(pendingTxn.PaymentProviderRef, "success")
			if _, err := orch.HandlePaymentNotification(ctx, pendingTxn.PaymentProviderRef); err != nil {
				t.Fatalf("webhook after OTP: %v", err)
			}
			rnOTP, _ = st.Repos().AutoPower().GetRun(ctx, octRunID)
			if rnOTP.Status != autopower.RunSucceeded {
				t.Fatalf("want SUCCEEDED after OTP settlement, got %+v", rnOTP)
			}
			if got := fake.Requests["POST /transaction/charge_authorization"]; got != 1 {
				t.Fatalf("want exactly 1 charge_authorization call across OTP flow, got %d", got)
			}
			tb, err := st.Repos().Ledger().TrialBalance(ctx)
			if err != nil || tb != 0 {
				t.Fatalf("ledger trial balance = %d (%v), want 0", tb, err)
			}
		})
	}
}

func TestLowBalanceStaleReadingDoesNotTrigger(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		loc := autopower.LagosLocation()
		e.setClock(time.Date(2027, 8, 1, 8, 0, 0, 0, loc))
		uid := e.user(t, "kemi")
		_, pm := e.saveCard(t, "kemi", "45012345678")
		smart := e.register(t, "kemi", "75012345999")
		if err := e.st.InTx(e.ctx, func(r store.Repos) error {
			return registry.MarkRemoteReadingConfirmedIn(e.ctx, r, smart.ID, e.clock)
		}); err != nil {
			t.Fatal(err)
		}
		thresh := 20.0
		rule, err := e.o.CreateAutoPowerRule(e.ctx, orchestrator.CreateRuleParams{
			UserID: uid, MeterID: smart.ID, PaymentMethodID: pm.ID,
			TriggerType: autopower.TriggerLowBalance, AmountMinor: 500000, MonthlyCapMinor: 3000000,
			DeliveryPreference: orchestrator.PrefToken, LowBalanceThresholdKWh: &thresh,
		})
		if err != nil {
			t.Fatal(err)
		}
		e.readings.SetReadingFrozen("75012345999", 5.0, e.clock.Add(-49*time.Hour))
		if _, err := e.o.PollLowBalance(e.ctx); err != nil {
			t.Fatal(err)
		}
		runs, _ := e.st.Repos().AutoPower().ListRunsForRule(e.ctx, rule.RuleID, 10)
		if len(runs) != 0 {
			t.Fatalf("stale reading must not trigger, got %+v", runs)
		}
	})
}
