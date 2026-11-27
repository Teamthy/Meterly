package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// ListedPayment is one PSP transaction (Paystack GET /transaction).
type ListedPayment struct {
	Reference string
	Status    string
	Amount    int64
	Currency  string
	PaidAt    *time.Time
}

// ListedRefund is one PSP refund (Paystack GET /refund).
type ListedRefund struct {
	TransactionRef string
	Amount         int64
	Status         string
}

// ListedSettlement is one PSP settlement batch (Paystack GET /settlement).
type ListedSettlement struct {
	ID              int64
	Status          string
	Currency        string
	TotalAmount     int64
	EffectiveAmount int64
	TotalFees       int64
	SettlementDate  time.Time
}

// PaymentStatement is the Paystack settlements / list API. Optional.
type PaymentStatement interface {
	ListTransactions(ctx context.Context, from, to time.Time) ([]ListedPayment, error)
	ListRefunds(ctx context.Context, from, to time.Time) ([]ListedRefund, error)
	ListSettlements(ctx context.Context, from, to time.Time) ([]ListedSettlement, error)
}

// OurTxn is the slice of a purchase the matcher needs. No tokens or meters.
type OurTxn struct {
	ID             uuid.UUID
	PaymentRef     string
	Payment        tx.PaymentState
	Vending        tx.VendingState
	DeliveryMethod tx.DeliveryMethod
	VendKey        string
	TotalMinor     int64
	AmountMinor    int64
	Currency       string
	RefundMinor    int64
	HasToken       bool
	CreatedAt      time.Time
}

// Tx is the in-transaction surface the engine writes through.
type Tx interface {
	Reconcile() Repo
	PostLedger(ctx context.Context, t ledger.Transaction) (ledger.Transaction, error)
	AddAudit(ctx context.Context, e audit.Event) error
}

// Host is persistence plus our own transactions. Implemented outside this
// package so store can import reconcile types without a cycle.
type Host interface {
	InTx(ctx context.Context, fn func(Tx) error) error
	Reconcile() Repo
	ListOur(ctx context.Context, from, to time.Time) ([]OurTxn, error)
	Balance(ctx context.Context, acct ledger.AccountCode, c money.Currency) (int64, error)
}

type Engine struct {
	Host   Host
	Pay    PaymentStatement
	Vend   providers.VendingProvider
	Now    func() time.Time
	OnOpen func([]OpenCount)
}

func New(h Host, pay PaymentStatement, vend providers.VendingProvider) *Engine {
	return &Engine{Host: h, Pay: pay, Vend: vend, Now: func() time.Time { return time.Now().UTC() }}
}

func FromTransaction(t *tx.Transaction) OurTxn {
	if t == nil {
		return OurTxn{}
	}
	return OurTxn{
		ID: t.ID, PaymentRef: t.PaymentProviderRef, Payment: t.Payment, Vending: t.Vending,
		DeliveryMethod: t.DeliveryMethod, VendKey: t.VendIdempotencyKey,
		TotalMinor: t.Total.Minor, AmountMinor: t.Amount.Minor, Currency: string(t.Total.Currency),
		RefundMinor: t.RefundAmount.Minor, HasToken: len(t.TokenCiphertext) > 0, CreatedAt: t.CreatedAt,
	}
}

// RunDay matches one provider for one Africa/Lagos date. Completed days are
// no-ops. Provider HTTP is outside InTx; writes commit in one transaction.
func (e *Engine) RunDay(ctx context.Context, provider string, date time.Time) (Run, error) {
	date = DateOnly(date)
	from, to := DayBounds(date)
	id := uuid.New()
	var run Run
	err := e.Host.InTx(ctx, func(t Tx) error {
		var err error
		run, err = t.Reconcile().ClaimRun(ctx, provider, date, id)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	if run.Status == RunCompleted {
		return run, nil
	}

	ours, err := e.Host.ListOur(ctx, from, to)
	if err != nil {
		_ = e.fail(ctx, run.ID, err.Error())
		return run, err
	}

	var (
		exceptions  []Exception
		matched     int
		matchedAmt  int64
		settlements []ListedSettlement
	)
	switch provider {
	case ProviderPaystack:
		if e.Pay == nil {
			_ = e.fail(ctx, run.ID, ErrNoSource.Error())
			return run, ErrNoSource
		}
		listed, err := e.Pay.ListTransactions(ctx, from, to)
		if err != nil {
			_ = e.fail(ctx, run.ID, err.Error())
			return run, err
		}
		refunds, err := e.Pay.ListRefunds(ctx, from, to)
		if err != nil {
			_ = e.fail(ctx, run.ID, err.Error())
			return run, err
		}
		settlements, err = e.Pay.ListSettlements(ctx, from, to)
		if err != nil {
			_ = e.fail(ctx, run.ID, err.Error())
			return run, err
		}
		exceptions, matched, matchedAmt = matchPaystack(run.ID, date, listed, refunds, ours)
	case ProviderVTPass:
		ex, m, amt, err := e.matchVending(ctx, run.ID, date, ours)
		if err != nil {
			_ = e.fail(ctx, run.ID, err.Error())
			return run, err
		}
		exceptions, matched, matchedAmt = ex, m, amt
	default:
		return run, fmt.Errorf("reconcile: unknown provider %s", provider)
	}

	err = e.Host.InTx(ctx, func(t Tx) error {
		for i := range exceptions {
			exceptions[i].RunID = run.ID
			if err := t.Reconcile().UpsertException(ctx, &exceptions[i]); err != nil {
				return err
			}
		}
		if provider == ProviderPaystack {
			extra, err := postSettlements(ctx, t, run.ID, date, settlements)
			if err != nil {
				return err
			}
			for i := range extra {
				extra[i].RunID = run.ID
				if err := t.Reconcile().UpsertException(ctx, &extra[i]); err != nil {
					return err
				}
			}
			exceptions = append(exceptions, extra...)
		}
		if provider == ProviderVTPass {
			if err := postWalletVends(ctx, t, ours); err != nil {
				return err
			}
		}
		run.Status = RunCompleted
		run.CompletedAt = e.Now().UTC()
		run.MatchedCount = matched
		run.MatchedMinor = matchedAmt
		run.ExceptionCount = len(exceptions)
		run.Error = ""
		return t.Reconcile().CompleteRun(ctx, run)
	})
	if err != nil {
		_ = e.fail(ctx, run.ID, err.Error())
		return run, err
	}
	e.publishOpen(ctx)
	return run, nil
}

func (e *Engine) fail(ctx context.Context, id uuid.UUID, text string) error {
	return e.Host.InTx(ctx, func(t Tx) error { return t.Reconcile().FailRun(ctx, id, text) })
}

func (e *Engine) publishOpen(ctx context.Context) {
	if e.OnOpen == nil {
		return
	}
	counts, err := e.Host.Reconcile().CountOpen(ctx)
	if err == nil {
		e.OnOpen(counts)
	}
}

func matchPaystack(runID uuid.UUID, date time.Time, listed []ListedPayment, refunds []ListedRefund, ours []OurTxn) ([]Exception, int, int64) {
	byRef := map[string]OurTxn{}
	for _, o := range ours {
		if o.PaymentRef != "" {
			byRef[o.PaymentRef] = o
		}
	}
	seen := map[string]bool{}
	var ex []Exception
	matched, amt := 0, int64(0)
	for _, p := range listed {
		ref := strings.TrimSpace(p.Reference)
		if ref == "" {
			continue
		}
		seen[ref] = true
		o, ok := byRef[ref]
		if !ok {
			ex = append(ex, mismatch(runID, date, ProviderPaystack, CatInPSPNotOurs, ref, uuid.Nil, 0, p.Amount, p.Currency))
			continue
		}
		cur := o.Currency
		if cur == "" {
			cur = "NGN"
		}
		if p.Currency != "" && !strings.EqualFold(p.Currency, cur) {
			ex = append(ex, mismatch(runID, date, ProviderPaystack, CatCurrencyMismatch, ref, o.ID, o.TotalMinor, p.Amount, p.Currency))
			continue
		}
		if p.Amount != o.TotalMinor {
			ex = append(ex, mismatch(runID, date, ProviderPaystack, CatAmountMismatch, ref, o.ID, o.TotalMinor, p.Amount, cur))
			continue
		}
		if paystackStatusMismatch(p.Status, o.Payment) {
			ex = append(ex, mismatch(runID, date, ProviderPaystack, CatStatusMismatch, ref, o.ID, o.TotalMinor, p.Amount, cur))
			continue
		}
		matched++
		amt += o.TotalMinor
	}
	for _, o := range ours {
		if o.PaymentRef == "" || seen[o.PaymentRef] {
			continue
		}
		if o.Payment == tx.PayCreated || o.Payment == tx.PayPending {
			continue
		}
		ex = append(ex, mismatch(runID, date, ProviderPaystack, CatInOursNotPSP, o.PaymentRef, o.ID, o.TotalMinor, 0, o.Currency))
	}

	refundedOurs := map[string]OurTxn{}
	for _, o := range ours {
		if o.Payment == tx.PayRefunded || o.Payment == tx.PayRefundPending {
			refundedOurs[o.PaymentRef] = o
		}
	}
	pspRefund := map[string]ListedRefund{}
	for _, r := range refunds {
		if r.TransactionRef == "" || r.Status == "failed" {
			continue
		}
		pspRefund[r.TransactionRef] = r
	}
	for ref, o := range refundedOurs {
		r, ok := pspRefund[ref]
		want := o.RefundMinor
		if want <= 0 {
			want = o.TotalMinor
		}
		if !ok || r.Amount != want {
			ex = append(ex, mismatch(runID, date, ProviderPaystack, CatRefundMismatch, ref, o.ID, want, r.Amount, o.Currency))
		}
	}
	for ref, r := range pspRefund {
		if _, ok := refundedOurs[ref]; ok {
			continue
		}
		o := byRef[ref]
		ex = append(ex, mismatch(runID, date, ProviderPaystack, CatRefundMismatch, ref, o.ID, 0, r.Amount, r.Status))
	}
	return ex, matched, amt
}

func paystackStatusMismatch(psp string, ours tx.PaymentState) bool {
	switch strings.ToLower(psp) {
	case "success":
		return ours != tx.PayConfirmed && ours != tx.PayRefundPending && ours != tx.PayRefunded
	case "failed", "reversed", "abandoned":
		return ours == tx.PayConfirmed
	default:
		return ours == tx.PayConfirmed
	}
}

func (e *Engine) matchVending(ctx context.Context, runID uuid.UUID, date time.Time, ours []OurTxn) ([]Exception, int, int64, error) {
	if e.Vend == nil {
		return nil, 0, 0, ErrNoSource
	}
	var ex []Exception
	matched, amt := 0, int64(0)
	for _, o := range ours {
		if strings.TrimSpace(o.VendKey) == "" {
			continue
		}
		res, err := e.Vend.GetVendStatus(ctx, o.VendKey)
		if err != nil {
			if providers.IsAmbiguous(err) {
				continue
			}
			return nil, 0, 0, err
		}
		switch res.Status {
		case providers.VendNotFound:
			if o.Vending == tx.VendConfirmed || o.Vending == tx.VendPending || o.Vending == tx.VendUnknown {
				ex = append(ex, mismatch(runID, date, ProviderVTPass, CatInOursNotPSP, o.VendKey, o.ID, o.AmountMinor, 0, o.Currency))
			}
		case providers.VendSucceeded:
			if o.DeliveryMethod == tx.MethodToken && res.Token == "" && !o.HasToken {
				ex = append(ex, mismatch(runID, date, ProviderVTPass, CatTokenMissing, o.VendKey, o.ID, o.AmountMinor, 0, o.Currency))
				continue
			}
			if o.Vending != tx.VendConfirmed {
				ex = append(ex, mismatch(runID, date, ProviderVTPass, CatStatusMismatch, o.VendKey, o.ID, o.AmountMinor, 0, o.Currency))
				continue
			}
			if o.DeliveryMethod == tx.MethodToken && !o.HasToken {
				ex = append(ex, mismatch(runID, date, ProviderVTPass, CatTokenMissing, o.VendKey, o.ID, o.AmountMinor, 0, o.Currency))
				continue
			}
			matched++
			amt += o.AmountMinor
		case providers.VendFailed:
			if o.Vending == tx.VendConfirmed {
				ex = append(ex, mismatch(runID, date, ProviderVTPass, CatStatusMismatch, o.VendKey, o.ID, o.AmountMinor, 0, o.Currency))
			}
		}
	}
	return ex, matched, amt, nil
}

func mismatch(runID uuid.UUID, date time.Time, provider string, cat Category, ref string, txn uuid.UUID, ours, theirs int64, cur string) Exception {
	if cur == "" {
		cur = "NGN"
	}
	now := time.Now().UTC()
	return Exception{
		ID: uuid.New(), RunID: runID, Provider: provider, BusinessDate: date,
		Category: cat, Severity: SeverityOf(cat), ProviderRef: ref, OurTxnID: txn,
		OursMinor: ours, TheirsMinor: theirs, Currency: cur, Status: StatusOpen,
		CreatedAt: now, UpdatedAt: now,
	}
}

func postSettlements(ctx context.Context, t Tx, runID uuid.UUID, date time.Time, batches []ListedSettlement) ([]Exception, error) {
	var extra []Exception
	for _, s := range batches {
		st := strings.ToLower(s.Status)
		if st != "success" && st != "processed" {
			continue
		}
		if s.EffectiveAmount+s.TotalFees != s.TotalAmount || s.TotalAmount <= 0 {
			extra = append(extra, mismatch(runID, date, ProviderPaystack, CatSettlementUnbalanced,
				fmt.Sprintf("settlement:%d", s.ID), uuid.Nil, s.TotalAmount, s.EffectiveAmount, s.Currency))
			continue
		}
		cur := money.Currency(s.Currency)
		if cur == "" {
			cur = money.NGN
		}
		src := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("paystack.settlement.%d", s.ID)))
		entries := []ledger.Entry{
			{Account: ledger.CashAtBank, Direction: ledger.Debit, Amount: money.New(s.EffectiveAmount, cur)},
			{Account: ledger.PSPSettlementReceivable, Direction: ledger.Credit, Amount: money.New(s.TotalAmount, cur)},
		}
		if s.TotalFees > 0 {
			entries = append(entries, ledger.Entry{Account: ledger.PSPFeesExpense, Direction: ledger.Debit, Amount: money.New(s.TotalFees, cur)})
		}
		_, err := t.PostLedger(ctx, ledger.Transaction{
			Reference:   fmt.Sprintf("psp-settle:%d", s.ID),
			SourceType:  "paystack_settlement",
			SourceID:    src,
			Description: "Paystack settlement (default fee treatment; founder/finance may revise)",
			PostedAt:    date.UTC(),
			Entries:     entries,
		})
		if err != nil {
			return extra, err
		}
	}
	return extra, nil
}

func postWalletVends(ctx context.Context, t Tx, ours []OurTxn) error {
	for _, o := range ours {
		if o.Vending != tx.VendConfirmed || o.AmountMinor <= 0 {
			continue
		}
		cur := money.Currency(o.Currency)
		if cur == "" {
			cur = money.NGN
		}
		_, err := t.PostLedger(ctx, ledger.Transaction{
			Reference:   "vend-wallet:" + o.ID.String(),
			SourceType:  "electricity_transaction",
			SourceID:    o.ID,
			Description: "Recognise vend against vendor wallet",
			PostedAt:    o.CreatedAt.UTC(),
			Entries: []ledger.Entry{
				{Account: ledger.ProviderPayable, Direction: ledger.Debit, Amount: money.New(o.AmountMinor, cur)},
				{Account: ledger.VendorWalletPrefund, Direction: ledger.Credit, Amount: money.New(o.AmountMinor, cur)},
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// CatchUp runs yesterday and the day before for each configured provider.
func (e *Engine) CatchUp(ctx context.Context) error {
	day := BusinessDate(e.Now())
	for _, d := range []time.Time{day.AddDate(0, 0, -1), day.AddDate(0, 0, -2)} {
		if e.Pay != nil {
			if _, err := e.RunDay(ctx, ProviderPaystack, d); err != nil {
				return err
			}
		}
		if e.Vend != nil {
			if _, err := e.RunDay(ctx, ProviderVTPass, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) DailySummary(ctx context.Context, date time.Time) (Summary, error) {
	date = DateOnly(date)
	s := Summary{BusinessDate: date, ExceptionsByCat: map[string]int{}}
	for _, p := range []string{ProviderPaystack, ProviderVTPass} {
		run, err := e.Host.Reconcile().GetRun(ctx, p, date)
		if err == nil {
			s.Runs = append(s.Runs, run)
			s.MatchedCount += run.MatchedCount
			s.MatchedMinor += run.MatchedMinor
		}
	}
	list, err := e.Host.Reconcile().ListExceptions(ctx, ExceptionFilter{BusinessDate: date, Limit: 500})
	if err != nil {
		return s, err
	}
	for _, e := range list {
		s.ExceptionsByCat[string(e.Category)]++
		if e.Status == StatusOpen || e.Status == StatusInvestigating {
			s.OpenCount++
		}
	}
	s.ReceivableMinor, _ = e.Host.Balance(ctx, ledger.PSPSettlementReceivable, money.NGN)
	return s, nil
}

func (e *Engine) ProposeAdjustment(ctx context.Context, actor audit.Actor, exceptionID uuid.UUID, debit, credit ledger.AccountCode, amount int64, currency, reason string) (Adjustment, error) {
	if !KnownAccount(debit) || !KnownAccount(credit) || debit == credit {
		return Adjustment{}, ErrUnknownAccount
	}
	if amount <= 0 {
		return Adjustment{}, ErrUnknownAccount
	}
	if currency == "" {
		currency = "NGN"
	}
	var out Adjustment
	err := e.Host.InTx(ctx, func(t Tx) error {
		ex, err := t.Reconcile().GetException(ctx, exceptionID)
		if err != nil {
			return err
		}
		out = Adjustment{
			ID: uuid.New(), ExceptionID: ex.ID, ProposedBy: actor.UserID, Status: AdjProposed,
			DebitAccount: debit, CreditAccount: credit, AmountMinor: amount, Currency: currency,
			Reason: reason, CreatedAt: e.Now().UTC(),
		}
		if err := t.Reconcile().CreateAdjustment(ctx, &out); err != nil {
			return err
		}
		return t.AddAudit(ctx, actor.Event("reconciliation.adjustment.proposed", "reconciliation_exception", ex.ID, map[string]any{
			"adjustment_id": out.ID.String(), "category": string(ex.Category),
		}))
	})
	return out, err
}

func (e *Engine) ApproveAdjustment(ctx context.Context, actor audit.Actor, adjustmentID uuid.UUID) (Adjustment, error) {
	var out Adjustment
	err := e.Host.InTx(ctx, func(t Tx) error {
		a, err := t.Reconcile().GetAdjustment(ctx, adjustmentID)
		if err != nil {
			return err
		}
		if a.Status != AdjProposed {
			return ErrNotProposed
		}
		if a.ProposedBy == "" || a.ProposedBy == actor.UserID {
			return ErrSameApprover
		}
		ex, err := t.Reconcile().GetException(ctx, a.ExceptionID)
		if err != nil {
			return err
		}
		cur := money.Currency(a.Currency)
		if cur == "" {
			cur = money.NGN
		}
		ref := "recon-adj:" + a.ID.String()
		_, err = t.PostLedger(ctx, ledger.Transaction{
			Reference: ref, SourceType: "reconciliation_exception", SourceID: ex.ID,
			Description: "Reviewed adjusting entry", PostedAt: e.Now().UTC(),
			Entries: []ledger.Entry{
				{Account: a.DebitAccount, Direction: ledger.Debit, Amount: money.New(a.AmountMinor, cur)},
				{Account: a.CreditAccount, Direction: ledger.Credit, Amount: money.New(a.AmountMinor, cur)},
			},
		})
		if err != nil {
			return err
		}
		now := e.Now().UTC()
		a.Status = AdjApproved
		a.ApprovedBy = actor.UserID
		a.ApprovedAt = now
		a.LedgerReference = ref
		if err := t.Reconcile().UpdateAdjustment(ctx, a); err != nil {
			return err
		}
		ex.Status = StatusResolved
		ex.UpdatedAt = now
		if err := t.Reconcile().UpdateException(ctx, ex); err != nil {
			return err
		}
		if err := t.AddAudit(ctx, actor.Event("reconciliation.adjustment.approved", "reconciliation_exception", ex.ID, map[string]any{
			"adjustment_id": a.ID.String(),
		})); err != nil {
			return err
		}
		out = a
		return nil
	})
	if err == nil {
		e.publishOpen(ctx)
	}
	return out, err
}

func (e *Engine) RecordWalletTopup(ctx context.Context, actor audit.Actor, amount int64, currency string) (ledger.Transaction, error) {
	if amount <= 0 {
		return ledger.Transaction{}, ErrUnknownAccount
	}
	if currency == "" {
		currency = "NGN"
	}
	cur := money.Currency(currency)
	id := uuid.New()
	var posted ledger.Transaction
	err := e.Host.InTx(ctx, func(t Tx) error {
		var err error
		posted, err = t.PostLedger(ctx, ledger.Transaction{
			Reference: "vtpass-topup:" + id.String(), SourceType: "vendor_wallet_topup", SourceID: id,
			Description: "VTpass wallet top-up from bank", PostedAt: e.Now().UTC(),
			Entries: []ledger.Entry{
				{Account: ledger.VendorWalletPrefund, Direction: ledger.Debit, Amount: money.New(amount, cur)},
				{Account: ledger.CashAtBank, Direction: ledger.Credit, Amount: money.New(amount, cur)},
			},
		})
		if err != nil {
			return err
		}
		return t.AddAudit(ctx, actor.Event("reconciliation.wallet_topup", "ledger", posted.ID, map[string]any{
			"amount_minor": amount,
		}))
	})
	return posted, err
}

func (e *Engine) Assign(ctx context.Context, actor audit.Actor, exceptionID uuid.UUID, owner string) (Exception, error) {
	if strings.TrimSpace(owner) == "" {
		owner = actor.UserID
	}
	var out Exception
	err := e.Host.InTx(ctx, func(t Tx) error {
		ex, err := t.Reconcile().GetException(ctx, exceptionID)
		if err != nil {
			return err
		}
		ex.Owner = owner
		if ex.Status == StatusOpen {
			ex.Status = StatusInvestigating
		}
		ex.UpdatedAt = e.Now().UTC()
		if err := t.Reconcile().UpdateException(ctx, ex); err != nil {
			return err
		}
		if err := t.AddAudit(ctx, actor.Event("reconciliation.exception.assigned", "reconciliation_exception", ex.ID, map[string]any{
			"owner": owner,
		})); err != nil {
			return err
		}
		out = ex
		return nil
	})
	e.publishOpen(ctx)
	return out, err
}

func (e *Engine) Resolve(ctx context.Context, actor audit.Actor, exceptionID uuid.UUID, status ExceptionStatus, notes string) (Exception, error) {
	if status != StatusResolved && status != StatusWrittenOff {
		return Exception{}, ErrBadStatus
	}
	var out Exception
	err := e.Host.InTx(ctx, func(t Tx) error {
		ex, err := t.Reconcile().GetException(ctx, exceptionID)
		if err != nil {
			return err
		}
		ex.Status = status
		if notes != "" {
			ex.Notes = notes
		}
		ex.UpdatedAt = e.Now().UTC()
		if err := t.Reconcile().UpdateException(ctx, ex); err != nil {
			return err
		}
		if err := t.AddAudit(ctx, actor.Event("reconciliation.exception.resolved", "reconciliation_exception", ex.ID, map[string]any{
			"status": string(status),
		})); err != nil {
			return err
		}
		out = ex
		return nil
	})
	e.publishOpen(ctx)
	return out, err
}
