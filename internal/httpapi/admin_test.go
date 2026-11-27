package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

type adminEnv struct {
	c   client
	pay *mock.Payments
}

func newAdminEnv(t *testing.T, st store.Store, limits httpapi.Limits) *adminEnv {
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay, Limits: limits}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return &adminEnv{c: client{t, srv}, pay: pay}
}

// underpaid creates a purchase whose payment is short, so it lands in review.
func (e *adminEnv) underpaid(t *testing.T) string {
	t.Helper()
	_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
	_, q := e.c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
	_, txn := e.c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
		map[string]string{"Idempotency-Key": "admin-0123456789abc"})
	url := txn["checkout_url"].(string)
	ref := url[strings.LastIndex(url, "/")+1:]
	_ = e.pay.Complete(ref, true, 100)
	e.c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"})
	return txn["transaction_id"].(string)
}

func TestAdminAPI(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newAdminEnv(t, st, httpapi.Limits{})
		id := e.underpaid(t)
		reason := map[string]any{"reason": "customer underpaid, ticket 77"}

		// Customers can't reach the admin API at all.
		for _, r := range []struct{ method, path string }{
			{"GET", "/v1/admin/console"}, {"GET", "/v1/admin/transactions"}, {"GET", "/v1/admin/transactions/" + id},
			{"POST", "/v1/admin/transactions/" + id + "/token-reveal"}, {"POST", "/v1/admin/providers/mockvend/disable"},
		} {
			if code, _ := e.c.do(r.method, r.path, "ada", reason, nil); code != 403 {
				t.Fatalf("customer %s %s: %d", r.method, r.path, code)
			}
		}
		if code, _ := e.c.do("GET", "/v1/admin/transactions", "", nil, nil); code != 401 {
			t.Fatalf("anonymous: %d", code)
		}

		// Operators can investigate.
		code, list := e.c.do("GET", "/v1/admin/transactions?review=true", "operator:ola", nil, nil)
		if code != 200 || len(list["data"].([]any)) != 1 {
			t.Fatalf("review queue %d %v", code, list)
		}
		code, d := e.c.do("GET", "/v1/admin/transactions/"+id, "operator:ola", nil, nil)
		tr := d["transaction"].(map[string]any)
		if code != 200 || tr["status"] != "MANUAL_REVIEW" || tr["review_reason"] == "" {
			t.Fatalf("detail %d %v", code, tr)
		}
		if _, has := tr["token"]; has {
			t.Fatal("detail must never include the token")
		}
		if code, _ := e.c.do("GET", "/v1/admin/transactions?limit=0", "operator:ola", nil, nil); code != 400 {
			t.Fatalf("bad limit: %d", code)
		}

		// ...but not refund, and every action needs a reason.
		if code, _ := e.c.do("POST", "/v1/admin/transactions/"+id+"/refund", "operator:ola", reason, nil); code != 403 {
			t.Fatalf("operator refund: %d", code)
		}
		if code, _ := e.c.do("POST", "/v1/admin/transactions/"+id+"/refund", "admin:bo", map[string]any{}, nil); code != 400 {
			t.Fatalf("refund without reason: %d", code)
		}
		code, got := e.c.do("POST", "/v1/admin/transactions/"+id+"/refund", "admin:bo", reason, nil)
		if code != 200 || got["status"] != "REFUNDED" || got["refund_amount_minor"].(float64) != 100 {
			t.Fatalf("refund %d %v", code, got)
		}
		if code, _ := e.c.do("POST", "/v1/admin/transactions/"+id+"/refund", "admin:bo", reason, nil); code != 409 {
			t.Fatalf("second refund: %d", code)
		}
		// The refund shows up in the transaction's audit trail.
		_, d = e.c.do("GET", "/v1/admin/transactions/"+id, "operator:ola", nil, nil)
		auditTrail := d["audit"].([]any)
		if len(auditTrail) != 1 || auditTrail[0].(map[string]any)["action"] != "transaction.refund" {
			t.Fatalf("audit %v", auditTrail)
		}

		// Provider switches: admin only.
		if code, _ := e.c.do("POST", "/v1/admin/providers/mockvend/disable", "operator:ola", reason, nil); code != 403 {
			t.Fatalf("operator disable: %d", code)
		}
		if code, _ := e.c.do("POST", "/v1/admin/providers/mockvend/disable", "admin:bo", reason, nil); code != 200 {
			t.Fatalf("disable: %d", code)
		}
		if code, _ := e.c.do("POST", "/v1/admin/providers/acme/disable", "admin:bo", reason, nil); code != 404 {
			t.Fatalf("unknown provider: %d", code)
		}
		_, ps := e.c.do("GET", "/v1/admin/providers", "operator:ola", nil, nil)
		for _, p := range ps["data"].([]any) {
			pm := p.(map[string]any)
			if pm["name"] == "mockvend" && pm["enabled"] != false {
				t.Fatalf("providers %v", ps)
			}
		}
	})
}

func TestAdminConsoleFromServer(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newAdminEnv(t, st, httpapi.Limits{})
		id := e.underpaid(t)

		if code, _ := e.c.do("GET", "/v1/admin/console", "ada", nil, nil); code != 403 {
			t.Fatalf("customer: %d", code)
		}
		if code, _ := e.c.do("GET", "/v1/admin/console", "", nil, nil); code != 401 {
			t.Fatalf("anonymous: %d", code)
		}

		code, cons := e.c.do("GET", "/v1/admin/console", "operator:ola", nil, nil)
		if code != 200 || cons["role"] != "OPERATOR" || cons["can_admin"] != false {
			t.Fatalf("operator console %d %v", code, cons)
		}
		review, _ := cons["review"].([]any)
		if len(review) != 1 {
			t.Fatalf("review queue %v", cons["review"])
		}
		code, cons = e.c.do("GET", "/v1/admin/console", "admin:bo", nil, nil)
		if code != 200 || cons["role"] != "ADMIN" || cons["can_admin"] != true {
			t.Fatalf("admin console %d %v", code, cons)
		}

		code, d := e.c.do("GET", "/v1/admin/transactions/"+id, "operator:ola", nil, nil)
		if code != 200 {
			t.Fatalf("detail %d %v", code, d)
		}
		inv, _ := d["investigation"].(map[string]any)
		if inv == nil {
			t.Fatalf("missing investigation %v", d)
		}
		if _, has := inv["token"]; has {
			t.Fatal("investigation must never include the token")
		}
		if inv["status"] != "MANUAL_REVIEW" {
			t.Fatalf("investigation status %v", inv["status"])
		}
		refundEnabled := true
		for _, a := range inv["actions"].([]any) {
			am := a.(map[string]any)
			if am["id"] == "refund" {
				refundEnabled = am["enabled"] == true
			}
		}
		if refundEnabled {
			t.Fatalf("operator refund action should be disabled %v", inv["actions"])
		}

		_, d = e.c.do("GET", "/v1/admin/transactions/"+id, "admin:bo", nil, nil)
		inv = d["investigation"].(map[string]any)
		found := false
		for _, a := range inv["actions"].([]any) {
			am := a.(map[string]any)
			if am["id"] == "refund" && am["enabled"] == true {
				found = true
			}
		}
		if !found {
			t.Fatalf("admin refund should be enabled %v", inv["actions"])
		}
	})
}

func TestAdminTokenReveal(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newAdminEnv(t, st, httpapi.Limits{})
		_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil)
		_, q := e.c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
		_, txn := e.c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
			map[string]string{"Idempotency-Key": "reveal-0123456789ab"})
		url := txn["checkout_url"].(string)
		ref := url[strings.LastIndex(url, "/")+1:]
		_ = e.pay.Complete(ref, true, 0)
		e.c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"})
		id := txn["transaction_id"].(string)

		if code, _ := e.c.do("POST", "/v1/admin/transactions/"+id+"/token-reveal", "operator:ola", map[string]any{"reason": "x"}, nil); code != 400 {
			t.Fatalf("short reason: %d", code)
		}
		code, tok := e.c.do("POST", "/v1/admin/transactions/"+id+"/token-reveal", "operator:ola", map[string]any{"reason": "customer on phone, ticket 9"}, nil)
		if code != 200 || len(tok["token"].(string)) != 24 {
			t.Fatalf("reveal %d %v", code, tok)
		}
		_, d := e.c.do("GET", "/v1/admin/transactions/"+id, "operator:ola", nil, nil)
		trail := d["audit"].([]any)
		if len(trail) != 1 || trail[0].(map[string]any)["action"] != "token.reveal" {
			t.Fatalf("audit %v", trail)
		}
	})
}

func TestRateLimits(t *testing.T) {
	e := newAdminEnv(t, newMem(), httpapi.Limits{WritesPerMinute: 1, AuthFailuresPerMinute: 1})
	// Writes: burst of 10 per user, then 429 with Retry-After.
	var last *http.Response
	for i := 0; i < 12; i++ {
		req, _ := http.NewRequest("POST", e.c.srv.URL+"/v1/meters/validate", strings.NewReader(`{"meter_number":"45012345678","disco_code":"IKEDC"}`))
		req.Header.Set("Authorization", "Bearer spammer")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		last = res
	}
	if last.StatusCode != 429 || last.Header.Get("Retry-After") == "" {
		t.Fatalf("want 429 with Retry-After, got %d", last.StatusCode)
	}
	// Another user is unaffected.
	if code, _ := e.c.do("POST", "/v1/meters/validate", "someone-else", map[string]any{"meter_number": "45012345678", "disco_code": "IKEDC"}, nil); code != 200 {
		t.Fatalf("other user: %d", code)
	}
	// Repeated failed authentication from one IP is throttled.
	code := 0
	for i := 0; i < 12; i++ {
		code, _ = e.c.do("GET", "/v1/meters", "", nil, nil)
	}
	if code != 429 {
		t.Fatalf("auth failures: %d", code)
	}
}

func newMem() store.Store { return memstore.New() }
