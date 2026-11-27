package httpapi_test

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func partnerAPI(t *testing.T, st store.Store, floatOff bool) (*httpapi.Server, client, *syncBuf) {
	t.Helper()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &partners.Engine{Repo: st.Repos().Partners(), Box: box, Environment: partners.EnvSandbox}
	s := &httpapi.Server{
		O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Partners: eng,
		PartnerEnvironment:   partners.EnvSandbox,
		PartnerFloatDisabled: floatOff,
		Limits:               httpapi.Limits{WritesPerMinute: 6000, AuthFailuresPerMinute: 6000, PartnerPerMinute: 6000},
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return s, client{t, srv}, logs
}

func issuePartner(t *testing.T, c client, org string, cap int64, scopes []string) (pid, secret string) {
	t.Helper()
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "onboard " + org, "organisation": org, "environment": "SANDBOX",
		"contact_ref": "t-1", "daily_value_cap_minor": cap, "rate_limit_per_minute": 60,
	}, nil)
	if code != 201 {
		t.Fatalf("create partner %d %v", code, created)
	}
	pid = created["id"].(string)
	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "sandbox key", "scopes": scopes,
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	return pid, issued["secret"].(string)
}

func fundPartner(t *testing.T, c client, pid string, receipt string, amount int64) {
	t.Helper()
	code, proposed := c.do("POST", "/v1/admin/partners/"+pid+"/float/topups", "operator:ada", map[string]any{
		"reason": "bank receipt " + receipt, "receipt_ref": receipt, "amount_minor": amount, "currency": "NGN",
	}, nil)
	if code != 201 {
		t.Fatalf("propose %d %v", code, proposed)
	}
	if code, body := c.do("POST", "/v1/admin/partners/"+pid+"/float/topups/"+proposed["id"].(string)+"/approve", "operator:ada", map[string]any{
		"reason": "cannot self-approve",
	}, nil); code != 409 {
		t.Fatalf("same approver %d %v", code, body)
	}
	code, approved := c.do("POST", "/v1/admin/partners/"+pid+"/float/topups/"+proposed["id"].(string)+"/approve", "operator:bola", map[string]any{
		"reason": "cleared against " + receipt,
	}, nil)
	if code != 200 || approved["status"] != partners.TopupApproved {
		t.Fatalf("approve %d %v", code, approved)
	}
}

var purchaseScopes = []string{
	partners.ScopeMetersValidate, partners.ScopeQuotesCreate,
	partners.ScopePurchasesCreate, partners.ScopePurchasesRead, partners.ScopeFloatRead,
}

func TestPartnerPurchaseAPIBothStores(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		s, c, logs := partnerAPI(t, st, false)
		pid, secret := issuePartner(t, c, "Acme Co", 0, purchaseScopes)
		fundPartner(t, c, pid, "BANK-ACME-001", 2_000_000)

		code, val := c.do("POST", "/v1/partner/meters/validate", secret, map[string]any{
			"meter_number": "45012345678", "disco_code": "IKEDC",
		}, nil)
		if code != 200 || val["valid"] != true {
			t.Fatalf("validate %d %v", code, val)
		}

		code, q := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
			"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
		}, nil)
		if code != 201 {
			t.Fatalf("quote %d %v", code, q)
		}

		idem := map[string]string{"Idempotency-Key": "partner-http-00000001"}
		body := map[string]any{"quote_id": q["quote_id"], "partner_ref": "http-order-0001"}
		code, purchase := c.do("POST", "/v1/partner/purchases", secret, body, idem)
		if code != 201 {
			t.Fatalf("purchase %d %v", code, purchase)
		}
		if purchase["funding_source"] != "PARTNER_FLOAT" {
			t.Fatalf("funding %v", purchase)
		}
		id := purchase["transaction_id"].(string)
		tok, _ := purchase["token"].(string)
		if tok == "" {
			t.Fatalf("expected token in create response %v", purchase)
		}

		code, replay := c.do("POST", "/v1/partner/purchases", secret, body, idem)
		if code != 200 || replay["transaction_id"] != id {
			t.Fatalf("replay %d %v", code, replay)
		}

		code, got := c.do("GET", "/v1/partner/purchases/"+id, secret, nil, nil)
		if code != 200 || got["transaction_id"] != id {
			t.Fatalf("get %d %v", code, got)
		}
		code, byRef := c.do("GET", "/v1/partner/purchases?partner_ref=http-order-0001", secret, nil, nil)
		if code != 200 || byRef["transaction_id"] != id {
			t.Fatalf("by ref %d %v", code, byRef)
		}

		code, fl := c.do("GET", "/v1/partner/float", secret, nil, nil)
		if code != 200 {
			t.Fatalf("float %d %v", code, fl)
		}
		if _, ok := fl["available_minor"].(float64); !ok {
			t.Fatalf("float body %v", fl)
		}

		if strings.Contains(logs.String(), tok) {
			t.Fatalf("token logged: %s", logs.String())
		}
		evs, err := s.O.Store().Repos().Outbox().ForAggregate(t.Context(), uuid.MustParse(id))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(evs)
		if strings.Contains(string(raw), tok) {
			t.Fatalf("token in outbox: %s", raw)
		}

		_, bSecret := issuePartner(t, c, "Beta Co", 0, purchaseScopes)
		if code, body := c.do("GET", "/v1/partner/purchases/"+id, bSecret, nil, nil); code != 404 {
			t.Fatalf("IDOR get %d %v", code, body)
		}
		if code, body := c.do("GET", "/v1/partner/purchases?partner_ref=http-order-0001", bSecret, nil, nil); code != 404 {
			t.Fatalf("IDOR list %d %v", code, body)
		}
		if code, body := c.do("POST", "/v1/partner/purchases", bSecret, map[string]any{
			"quote_id": q["quote_id"], "partner_ref": "http-order-0001",
		}, map[string]string{"Idempotency-Key": "partner-http-beta00001"}); code == 201 {
			t.Fatalf("IDOR consume quote %d %v", code, body)
		}

		tb, err := s.O.Store().Repos().Ledger().TrialBalance(t.Context())
		if err != nil || tb != 0 {
			t.Fatalf("trial balance %d (%v)", tb, err)
		}
	})
}

func TestPartnerPurchaseScopeIsolationAndFloatFlag(t *testing.T) {
	st := memstore.New()
	_, c, _ := partnerAPI(t, st, false)
	pid, secret := issuePartner(t, c, "Scoped Co", 0, []string{partners.ScopeMetersValidate})
	if code, body := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
		"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
	}, nil); code != 403 {
		t.Fatalf("quotes without scope %d %v", code, body)
	}
	if code, body := c.do("GET", "/v1/partner/float", secret, nil, nil); code != 403 {
		t.Fatalf("float without scope %d %v", code, body)
	}
	_ = pid

	_, c2, _ := partnerAPI(t, memstore.New(), true)
	_, secret2 := issuePartner(t, c2, "NoFloat Co", 0, purchaseScopes)
	if code, body := c2.do("POST", "/v1/partner/meters/validate", secret2, map[string]any{
		"meter_number": "45012345678", "disco_code": "IKEDC",
	}, nil); code != 200 {
		t.Fatalf("validate still works %d %v", code, body)
	}
	code, q := c2.do("POST", "/v1/partner/quotes", secret2, map[string]any{
		"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
	}, nil)
	if code != 201 {
		t.Fatalf("quote without float flag %d %v", code, q)
	}
	if code, body := c2.do("POST", "/v1/partner/purchases", secret2, map[string]any{
		"quote_id": q["quote_id"], "partner_ref": "no-float-0001",
	}, map[string]string{"Idempotency-Key": "partner-http-nofloat01"}); code != 404 {
		t.Fatalf("purchase with float off %d %v", code, body)
	}
}

func TestPartnerPurchaseInsufficientFloatHTTP(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		_, c, _ := partnerAPI(t, st, false)
		_, secret := issuePartner(t, c, "Poor Co", 0, purchaseScopes)
		code, q := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
			"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
		}, nil)
		if code != 201 {
			t.Fatalf("quote %d %v", code, q)
		}
		code, body := c.do("POST", "/v1/partner/purchases", secret, map[string]any{
			"quote_id": q["quote_id"], "partner_ref": "poor-order-0001",
		}, map[string]string{"Idempotency-Key": "partner-http-poor00001"})
		if code != 409 {
			t.Fatalf("want 409 insufficient, got %d %v", code, body)
		}
	})
}
