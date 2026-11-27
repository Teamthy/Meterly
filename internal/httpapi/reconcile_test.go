package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func newReconEnv(t *testing.T, st store.Store) (client, *reconcile.Engine) {
	t.Helper()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := reconcile.New(store.ReconcileHost(st), nil, vend)
	s := &httpapi.Server{O: o, Reconcile: eng, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return client{t, srv}, eng
}

func seedException(t *testing.T, st store.Store) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	repo := st.Repos().Reconcile()
	date := time.Date(2026, 1, 15, 0, 0, 0, 0, reconcile.WAT)
	run, err := repo.ClaimRun(ctx, reconcile.ProviderPaystack, date, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	ex := &reconcile.Exception{
		RunID: run.ID, Provider: reconcile.ProviderPaystack, BusinessDate: date,
		Category: reconcile.CatAmountMismatch, Severity: reconcile.SeverityHigh,
		ProviderRef: "psk_mismatch", Currency: "NGN", Status: reconcile.StatusOpen,
		OursMinor: 500, TheirsMinor: 400,
	}
	if err := repo.UpsertException(ctx, ex); err != nil {
		t.Fatal(err)
	}
	return ex.ID
}

func TestReconciliationAdmin(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		c, _ := newReconEnv(t, st)
		id := seedException(t, st)
		reason := map[string]any{"reason": "investigating settlement batch 77"}

		if code, _ := c.do("GET", "/v1/admin/reconciliation/exceptions", "ada", nil, nil); code != 403 {
			t.Fatalf("customer list: %d", code)
		}
		code, list := c.do("GET", "/v1/admin/reconciliation/exceptions?status=OPEN", "operator:ola", nil, nil)
		if code != 200 || len(list["data"].([]any)) != 1 {
			t.Fatalf("list %d %v", code, list)
		}
		code, detail := c.do("GET", "/v1/admin/reconciliation/exceptions/"+id.String(), "operator:ola", nil, nil)
		if code != 200 || detail["category"] != "AMOUNT_MISMATCH" {
			t.Fatalf("detail %d %v", code, detail)
		}
		code, assigned := c.do("POST", "/v1/admin/reconciliation/exceptions/"+id.String()+"/assign", "operator:ola",
			map[string]any{"reason": "taking this mismatch", "owner": "ola"}, nil)
		if code != 200 || assigned["status"] != "INVESTIGATING" {
			t.Fatalf("assign %d %v", code, assigned)
		}

		code, adj := c.do("POST", "/v1/admin/reconciliation/exceptions/"+id.String()+"/adjustments", "operator:ola",
			map[string]any{
				"reason": "paystack fee not accrued", "debit_account": string(ledger.PSPFeesExpense),
				"credit_account": string(ledger.CashAtBank), "amount_minor": 100, "currency": "NGN",
			}, nil)
		if code != 201 {
			t.Fatalf("propose %d %v", code, adj)
		}
		adjID := adj["id"].(string)
		if code, _ := c.do("POST", "/v1/admin/reconciliation/adjustments/"+adjID+"/approve", "operator:ola", reason, nil); code != 403 {
			t.Fatalf("operator cannot approve, got %d", code)
		}
		code, approved := c.do("POST", "/v1/admin/reconciliation/adjustments/"+adjID+"/approve", "admin:ada", reason, nil)
		if code != 200 || approved["status"] != "APPROVED" {
			t.Fatalf("approve %d %v", code, approved)
		}
		_, detail = c.do("GET", "/v1/admin/reconciliation/exceptions/"+id.String(), "operator:ola", nil, nil)
		if detail["status"] != "RESOLVED" {
			t.Fatalf("resolved %v", detail)
		}
		code, sum := c.do("GET", "/v1/admin/reconciliation/summary?date=2026-01-15", "operator:ola", nil, nil)
		if code != 200 || sum["business_date"] != "2026-01-15" {
			t.Fatalf("summary %d %v", code, sum)
		}
	})
}

func TestReconciliationWalletTopupRequiresAdmin(t *testing.T) {
	st := memstore.New()
	c, _ := newReconEnv(t, st)
	body := map[string]any{"reason": "funded vtpass wallet from zenith", "amount_minor": 500000, "currency": "NGN"}
	if code, _ := c.do("POST", "/v1/admin/reconciliation/wallet-topup", "operator:ola", body, nil); code != 403 {
		t.Fatalf("operator topup: %d", code)
	}
	code, got := c.do("POST", "/v1/admin/reconciliation/wallet-topup", "admin:ada", body, nil)
	if code != 201 || got["reference"] == nil {
		t.Fatalf("admin topup %d %v", code, got)
	}
}
