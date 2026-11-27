package orchestrator_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (e *env) partner(t *testing.T, org string, capMinor int64) partners.Partner {
	t.Helper()
	ada := e.user(t, "operator:ada")
	eng := &partners.Engine{Repo: e.st.Repos().Partners(), Environment: partners.EnvSandbox}
	p, err := eng.Create(e.ctx, e.st.Repos().Partners(), ada, org, partners.EnvSandbox, "t-1", capMinor, 60)
	if err != nil {
		t.Fatalf("create partner: %v", err)
	}
	return p
}

func (e *env) topupFloat(t *testing.T, partnerID uuid.UUID, receipt string, amount int64) {
	t.Helper()
	ada := audit.Actor{UserID: e.user(t, "operator:ada"), Type: audit.ActorOperator, Reason: "bank receipt " + receipt}
	bola := audit.Actor{UserID: e.user(t, "operator:bola"), Type: audit.ActorOperator, Reason: "approve float " + receipt}
	got, err := e.o.ProposePartnerFloatTopup(e.ctx, ada, partnerID, receipt, amount, "NGN")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := e.o.ApprovePartnerFloatTopup(e.ctx, ada, got.ID); !errors.Is(err, partners.ErrSameApprover) {
		t.Fatalf("same approver: %v", err)
	}
	if _, err := e.o.ApprovePartnerFloatTopup(e.ctx, bola, got.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func TestPartnerFloatPurchaseRefundAndCap(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.partner(t, "Acme Float", 0)
		e.topupFloat(t, p.ID, "RCPT-ACME-0001", 1_000_000)

		q, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: p.ID, MeterNumber: "45012345678", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("quote: %v", err)
		}
		txn, replayed, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "acme-order-0001",
			IdempotencyKey: "partner-idem-00000001", RequestHash: "h1",
		})
		if err != nil || replayed || txn == nil {
			t.Fatalf("purchase: replayed=%v err=%v txn=%v", replayed, err, txn)
		}
		if txn.EffectiveFundingSource() != tx.FundingPartnerFloat || txn.Payment != tx.PayConfirmed {
			t.Fatalf("funding %+v", txn)
		}
		if txn.Vending != tx.VendConfirmed {
			t.Fatalf("vend %s", txn.Vending)
		}
		e.assertBalanced(t)
		f, err := e.o.PartnerFloat(e.ctx, p.ID)
		if err != nil || f.AvailableMinor != 1_000_000-txn.Total.Minor {
			t.Fatalf("float %d err=%v total=%d", f.AvailableMinor, err, txn.Total.Minor)
		}

		again, replayed, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "acme-order-0001",
			IdempotencyKey: "partner-idem-00000001", RequestHash: "h1",
		})
		if err != nil || !replayed || again.ID != txn.ID {
			t.Fatalf("replay: %v replayed=%v id=%v", err, replayed, again)
		}
		posts := e.postings(t, txn.ID)
		if posts < 2 {
			t.Fatalf("expected float debit + vend postings, got %d", posts)
		}

		events, err := e.st.Repos().Outbox().ForAggregate(e.ctx, txn.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(events)
		if tok, _, _ := e.o.PartnerToken(e.ctx, p.ID, txn.ID); tok != "" && containsToken(string(raw), tok) {
			t.Fatalf("token leaked in outbox: %s", raw)
		}
	})
}

func TestPartnerFloatVendFailureRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.partner(t, "Acme Fail", 0)
		e.topupFloat(t, p.ID, "RCPT-FAIL-0001", 1_000_000)
		q, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: p.ID, MeterNumber: "45012340000", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("quote: %v", err)
		}
		txn, _, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "acme-fail-0001",
			IdempotencyKey: "partner-idem-fail00001", RequestHash: "hf",
		})
		if err != nil {
			t.Fatalf("purchase: %v", err)
		}
		if txn.Vending != tx.VendFailed || txn.Payment != tx.PayRefunded {
			t.Fatalf("want failed+refunded, got vend=%s pay=%s", txn.Vending, txn.Payment)
		}
		e.assertBalanced(t)
		f, err := e.o.PartnerFloat(e.ctx, p.ID)
		if err != nil || f.AvailableMinor != 1_000_000 {
			t.Fatalf("float restored %d (%v)", f.AvailableMinor, err)
		}
		if e.balance(t, ledger.PartnerFloat) != -1_000_000 {
			t.Fatalf("PARTNER_FLOAT ledger %d", e.balance(t, ledger.PartnerFloat))
		}
	})
}

func TestPartnerFloatInsufficientAndCapAndUnknown(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.partner(t, "Acme Cap", 510000)
		q, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: p.ID, MeterNumber: "45012345678", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("quote: %v", err)
		}
		_, _, err = e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "cap-order-0001",
			IdempotencyKey: "partner-idem-cap000001", RequestHash: "hc",
		})
		if !errors.Is(err, orchestrator.ErrInsufficientFloat) {
			t.Fatalf("want insufficient, got %v", err)
		}

		e.topupFloat(t, p.ID, "RCPT-CAP-0001", 2_000_000)
		txn, _, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "cap-order-0001",
			IdempotencyKey: "partner-idem-cap000001", RequestHash: "hc",
		})
		if err != nil {
			t.Fatalf("first purchase: %v", err)
		}
		if txn.Status() != tx.OverallCompleted {
			t.Fatalf("status %s", txn.Status())
		}

		q2, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: p.ID, MeterNumber: "45012345679", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("quote2: %v", err)
		}
		_, _, err = e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: p.ID, QuoteID: q2.ID, PartnerRef: "cap-order-0002",
			IdempotencyKey: "partner-idem-cap000002", RequestHash: "hc2",
		})
		if !errors.Is(err, orchestrator.ErrPartnerDailyCap) {
			t.Fatalf("want cap, got %v", err)
		}

		unk := e.partner(t, "Acme Unknown", 0)
		e.topupFloat(t, unk.ID, "RCPT-UNK-0001", 1_000_000)
		uq, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: unk.ID, MeterNumber: "45012348888", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("unknown quote: %v", err)
		}
		ut, _, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
			PartnerID: unk.ID, QuoteID: uq.ID, PartnerRef: "unk-order-0001",
			IdempotencyKey: "partner-idem-unk000001", RequestHash: "hu",
		})
		if err != nil {
			t.Fatalf("unknown purchase: %v", err)
		}
		if ut.Vending != tx.VendUnknown {
			t.Fatalf("want UNKNOWN, got %s", ut.Vending)
		}
		if ut.Payment != tx.PayConfirmed {
			t.Fatalf("UNKNOWN must not refund, pay=%s", ut.Payment)
		}
		e.assertBalanced(t)
	})
}

func TestPartnerPurchaseConcurrentIdempotent(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.partner(t, "Acme Race", 0)
		e.topupFloat(t, p.ID, "RCPT-RACE-0001", 5_000_000)
		q, err := e.o.IssuePartnerQuote(e.ctx, orchestrator.PartnerQuoteRequest{
			PartnerID: p.ID, MeterNumber: "45012345678", DiscoCode: "IKEDC", AmountMinor: 500000,
		})
		if err != nil {
			t.Fatalf("quote: %v", err)
		}
		const n = 8
		var wg sync.WaitGroup
		ids := make([]uuid.UUID, n)
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				txn, _, err := e.o.CreatePartnerPurchase(e.ctx, orchestrator.PartnerPurchaseRequest{
					PartnerID: p.ID, QuoteID: q.ID, PartnerRef: "race-order-0001",
					IdempotencyKey: "partner-idem-race00001", RequestHash: "hr",
				})
				errs[i] = err
				if txn != nil {
					ids[i] = txn.ID
				}
			}(i)
		}
		wg.Wait()
		seen := map[uuid.UUID]bool{}
		for i, id := range ids {
			if errs[i] != nil && !errors.Is(errs[i], idempotency.ErrInProgress) &&
				!errors.Is(errs[i], store.ErrConflict) &&
				!errors.Is(errs[i], orchestrator.ErrQuoteExpired) {
				t.Fatalf("worker %d: %v", i, errs[i])
			}
			if id != uuid.Nil {
				seen[id] = true
			}
		}
		if len(seen) != 1 {
			t.Fatalf("want 1 purchase, got %d ids=%v errs=%v", len(seen), ids, errs)
		}
		var id uuid.UUID
		for k := range seen {
			id = k
		}
		posts := e.postings(t, id)
		debits := 0
		rows, err := e.st.Repos().Ledger().BySource(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Reference == "partner-float:"+id.String() {
				debits++
			}
		}
		if debits != 1 {
			t.Fatalf("float debits %d posts=%d", debits, posts)
		}
		e.assertBalanced(t)
	})
}

func containsToken(s, tok string) bool {
	return tok != "" && (len(s) >= len(tok) && (s == tok || (len(s) > 0 && containsFold(s, tok))))
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
