package reconcile_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type fakePay struct {
	txns        []reconcile.ListedPayment
	refunds     []reconcile.ListedRefund
	settlements []reconcile.ListedSettlement
}

func (f fakePay) ListTransactions(context.Context, time.Time, time.Time) ([]reconcile.ListedPayment, error) {
	return f.txns, nil
}
func (f fakePay) ListRefunds(context.Context, time.Time, time.Time) ([]reconcile.ListedRefund, error) {
	return f.refunds, nil
}
func (f fakePay) ListSettlements(context.Context, time.Time, time.Time) ([]reconcile.ListedSettlement, error) {
	return f.settlements, nil
}

type fakeHost struct {
	runs map[string]reconcile.Run
	ex   map[uuid.UUID]reconcile.Exception
	fp   map[string]uuid.UUID
	adj  map[uuid.UUID]reconcile.Adjustment
	ours []reconcile.OurTxn
	led  []ledger.Transaction
}

func newHost(ours []reconcile.OurTxn) *fakeHost {
	return &fakeHost{
		runs: map[string]reconcile.Run{},
		ex:   map[uuid.UUID]reconcile.Exception{},
		fp:   map[string]uuid.UUID{},
		adj:  map[uuid.UUID]reconcile.Adjustment{},
		ours: ours,
	}
}

func (h *fakeHost) key(provider string, date time.Time) string {
	return provider + "|" + reconcile.DateOnly(date).Format("2006-01-02")
}
func (h *fakeHost) InTx(_ context.Context, fn func(reconcile.Tx) error) error { return fn(h) }
func (h *fakeHost) Reconcile() reconcile.Repo                                 { return h }
func (h *fakeHost) ListOur(context.Context, time.Time, time.Time) ([]reconcile.OurTxn, error) {
	return h.ours, nil
}
func (h *fakeHost) Balance(_ context.Context, acct ledger.AccountCode, c money.Currency) (int64, error) {
	var v int64
	for _, t := range h.led {
		for _, e := range t.Entries {
			if e.Account != acct || e.Amount.Currency != c {
				continue
			}
			if e.Direction == ledger.Debit {
				v += e.Amount.Minor
			} else {
				v -= e.Amount.Minor
			}
		}
	}
	return v, nil
}
func (h *fakeHost) PostLedger(_ context.Context, t ledger.Transaction) (ledger.Transaction, error) {
	if err := ledger.Validate(t.Entries); err != nil {
		return ledger.Transaction{}, err
	}
	for _, existing := range h.led {
		if existing.Reference == t.Reference {
			return existing, nil
		}
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	h.led = append(h.led, t)
	return t, nil
}
func (h *fakeHost) AddAudit(context.Context, audit.Event) error { return nil }

func (h *fakeHost) ClaimRun(_ context.Context, provider string, date time.Time, id uuid.UUID) (reconcile.Run, error) {
	k := h.key(provider, date)
	if existing, ok := h.runs[k]; ok {
		if existing.Status == reconcile.RunCompleted {
			return existing, nil
		}
		existing.Status = reconcile.RunRunning
		h.runs[k] = existing
		return existing, nil
	}
	run := reconcile.Run{ID: id, Provider: provider, BusinessDate: reconcile.DateOnly(date), Status: reconcile.RunRunning, StartedAt: time.Now().UTC()}
	h.runs[k] = run
	return run, nil
}
func (h *fakeHost) GetRun(_ context.Context, provider string, date time.Time) (reconcile.Run, error) {
	run, ok := h.runs[h.key(provider, date)]
	if !ok {
		return reconcile.Run{}, store.ErrNotFound
	}
	return run, nil
}
func (h *fakeHost) CompleteRun(_ context.Context, run reconcile.Run) error {
	run.Status = reconcile.RunCompleted
	h.runs[h.key(run.Provider, run.BusinessDate)] = run
	return nil
}
func (h *fakeHost) FailRun(_ context.Context, id uuid.UUID, errText string) error {
	for k, run := range h.runs {
		if run.ID == id {
			run.Status = reconcile.RunFailed
			run.Error = errText
			h.runs[k] = run
			return nil
		}
	}
	return store.ErrNotFound
}
func (h *fakeHost) UpsertException(_ context.Context, e *reconcile.Exception) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	fp := e.Fingerprint()
	if id, ok := h.fp[fp]; ok {
		cur := h.ex[id]
		if cur.Status == reconcile.StatusResolved || cur.Status == reconcile.StatusWrittenOff {
			e.ID = cur.ID
			e.Status = cur.Status
			return nil
		}
		cur.OursMinor, cur.TheirsMinor, cur.RunID = e.OursMinor, e.TheirsMinor, e.RunID
		h.ex[id] = cur
		e.ID, e.Status = cur.ID, cur.Status
		return nil
	}
	h.ex[e.ID] = *e
	h.fp[fp] = e.ID
	return nil
}
func (h *fakeHost) GetException(_ context.Context, id uuid.UUID) (reconcile.Exception, error) {
	e, ok := h.ex[id]
	if !ok {
		return reconcile.Exception{}, store.ErrNotFound
	}
	return e, nil
}
func (h *fakeHost) ListExceptions(_ context.Context, f reconcile.ExceptionFilter) ([]reconcile.Exception, error) {
	var out []reconcile.Exception
	for _, e := range h.ex {
		if f.Provider != "" && e.Provider != f.Provider {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
func (h *fakeHost) UpdateException(_ context.Context, e reconcile.Exception) error {
	if _, ok := h.ex[e.ID]; !ok {
		return store.ErrNotFound
	}
	h.ex[e.ID] = e
	return nil
}
func (h *fakeHost) CountOpen(context.Context) ([]reconcile.OpenCount, error) {
	n := map[string]int64{}
	for _, e := range h.ex {
		if e.Status == reconcile.StatusOpen || e.Status == reconcile.StatusInvestigating {
			n[e.Provider+"|"+string(e.Category)]++
		}
	}
	var out []reconcile.OpenCount
	for k, v := range n {
		out = append(out, reconcile.OpenCount{N: v, Provider: k})
	}
	return out, nil
}
func (h *fakeHost) CreateAdjustment(_ context.Context, a *reconcile.Adjustment) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	for _, ex := range h.adj {
		if ex.ExceptionID == a.ExceptionID && ex.Status == reconcile.AdjProposed {
			return store.ErrDuplicate
		}
	}
	h.adj[a.ID] = *a
	return nil
}
func (h *fakeHost) GetAdjustment(_ context.Context, id uuid.UUID) (reconcile.Adjustment, error) {
	a, ok := h.adj[id]
	if !ok {
		return reconcile.Adjustment{}, store.ErrNotFound
	}
	return a, nil
}
func (h *fakeHost) UpdateAdjustment(_ context.Context, a reconcile.Adjustment) error {
	h.adj[a.ID] = a
	return nil
}

func day() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, reconcile.WAT) }

func TestPaystackMatchAndSettlement(t *testing.T) {
	txnID := uuid.New()
	h := newHost([]reconcile.OurTxn{{
		ID: txnID, PaymentRef: "psk_ok", Payment: tx.PayConfirmed, TotalMinor: 500000, Currency: "NGN",
		CreatedAt: day(),
	}})
	pay := fakePay{
		txns: []reconcile.ListedPayment{
			{Reference: "psk_ok", Status: "success", Amount: 500000, Currency: "NGN"},
			{Reference: "psk_ghost", Status: "success", Amount: 100, Currency: "NGN"},
		},
		settlements: []reconcile.ListedSettlement{{
			ID: 77, Status: "success", Currency: "NGN", TotalAmount: 500000, EffectiveAmount: 490000, TotalFees: 10000,
			SettlementDate: day(),
		}},
	}
	eng := reconcile.New(h, pay, nil)
	eng.Now = func() time.Time { return day().UTC() }
	run, err := eng.RunDay(context.Background(), reconcile.ProviderPaystack, day())
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != reconcile.RunCompleted || run.MatchedCount != 1 || run.ExceptionCount != 1 {
		t.Fatalf("run %+v", run)
	}
	again, err := eng.RunDay(context.Background(), reconcile.ProviderPaystack, day())
	if err != nil || again.ID != run.ID || len(h.led) != 1 {
		t.Fatalf("completed day must be a no-op: %+v %v posts=%d", again, err, len(h.led))
	}
	if h.led[0].Reference != "psp-settle:77" || len(h.led[0].Entries) != 3 {
		t.Fatalf("settlement posting %+v", h.led[0])
	}
}

func TestSettlementUnbalancedIsException(t *testing.T) {
	h := newHost(nil)
	pay := fakePay{settlements: []reconcile.ListedSettlement{{
		ID: 9, Status: "success", Currency: "NGN", TotalAmount: 100, EffectiveAmount: 80, TotalFees: 10,
	}}}
	eng := reconcile.New(h, pay, nil)
	run, err := eng.RunDay(context.Background(), reconcile.ProviderPaystack, day())
	if err != nil || run.ExceptionCount != 1 || len(h.led) != 0 {
		t.Fatalf("want unbalanced exception and no post: %+v %v posts=%d", run, err, len(h.led))
	}
}

func TestTwoPersonAdjustment(t *testing.T) {
	h := newHost(nil)
	eng := reconcile.New(h, fakePay{}, nil)
	ex := &reconcile.Exception{
		ID: uuid.New(), Provider: reconcile.ProviderPaystack, BusinessDate: reconcile.DateOnly(day()),
		Category: reconcile.CatAmountMismatch, Status: reconcile.StatusOpen, Currency: "NGN",
	}
	if err := h.UpsertException(context.Background(), ex); err != nil {
		t.Fatal(err)
	}
	op := audit.Actor{UserID: uuid.NewString(), Type: audit.ActorOperator, Reason: "books off by 50 naira"}
	adj, err := eng.ProposeAdjustment(context.Background(), op, ex.ID, ledger.PSPFeesExpense, ledger.CashAtBank, 50, "NGN", "fee")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ApproveAdjustment(context.Background(), op, adj.ID); !errors.Is(err, reconcile.ErrSameApprover) {
		t.Fatalf("same approver: %v", err)
	}
	admin := audit.Actor{UserID: uuid.NewString(), Type: audit.ActorOperator, Reason: "reviewed the batch"}
	got, err := eng.ApproveAdjustment(context.Background(), admin, adj.ID)
	if err != nil || got.Status != reconcile.AdjApproved || len(h.led) != 1 {
		t.Fatalf("approve %+v %v posts=%d", got, err, len(h.led))
	}
	resolved, _ := h.GetException(context.Background(), ex.ID)
	if resolved.Status != reconcile.StatusResolved {
		t.Fatalf("status %s", resolved.Status)
	}
}
