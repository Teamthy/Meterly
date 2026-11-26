package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/heshttp"
	"github.com/teamthy/meterly/internal/providers/heshttp/hestest"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestRemoteCreditOnMeterWebhook(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
		pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
		o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
		s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Remote: remote, Log: log, Dev: true, DevPay: pay, DevRemote: remote}
		srv := httptest.NewServer(s.Routes())
		t.Cleanup(srv.Close)
		c := client{t, srv}

		code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "70011112222", "disco_code": "IKEDC", "label": "Smart"}, nil)
		if code != 201 {
			t.Fatalf("register %d %v", code, m)
		}
		caps := m["capabilities"].(map[string]any)
		rc := caps["remote_credit"].(map[string]any)
		if rc["authorised"] != true {
			t.Fatalf("expected authorised remote credit: %v", rc)
		}

		code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{
			"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN", "delivery_preference": "AUTO",
		}, nil)
		if code != 201 || q["delivery_method"] != "REMOTE" {
			t.Fatalf("quote %d %v", code, q)
		}

		code, txn := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
			map[string]string{"Idempotency-Key": "remote-e2e-01234567"})
		if code != 202 {
			t.Fatalf("create %d %v", code, txn)
		}
		id := txn["transaction_id"].(string)
		ref := txn["checkout_url"].(string)[strings.LastIndex(txn["checkout_url"].(string), "/")+1:]
		c.do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{}, nil)
		if code, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"}); code != 200 {
			t.Fatalf("pay webhook %d", code)
		}

		code, got := c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
		if code != 200 || got["delivery_status"] != "ACCEPTED" || got["delivery_method"] != "REMOTE" || got["credit_on_meter"] != false {
			t.Fatalf("accepted %d %v", code, got)
		}
		if code, tok := c.do("GET", "/v1/electricity/transactions/"+id+"/token", "ada", nil, nil); code != 409 || tok["error"].(map[string]any)["code"] != "CREDIT_ON_METER" {
			t.Fatalf("token while remote in flight: %d %v", code, tok)
		}

		if code, _ := c.do("POST", "/v1/webhooks/remote-credit/mockhes", "", map[string]any{"idempotency_key": "credit:" + id}, nil); code != 401 {
			t.Fatalf("unsigned remote webhook: %d", code)
		}

		remote.Settle()
		if code, _ := c.do("POST", "/v1/webhooks/remote-credit/mockhes", "", map[string]any{"idempotency_key": "credit:" + id}, map[string]string{"X-Mock-Signature": "dev-signature"}); code != 200 {
			t.Fatalf("remote webhook %d", code)
		}
		code, got = c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
		if code != 200 || got["delivery_status"] != "CONFIRMED" || got["credit_on_meter"] != true || got["token_available"] != false {
			t.Fatalf("confirmed %d %v", code, got)
		}
		if code, _ := c.do("GET", "/v1/electricity/transactions/"+id+"/token", "ada", nil, nil); code != 409 {
			t.Fatalf("still no keypad token after confirm: %d", code)
		}
	})
}

func TestHESHTTPAdapterEndToEnd(t *testing.T) {
	const secret = "e2e-hes-hmac-secret"
	fake := hestest.New(secret)
	t.Cleanup(fake.Close)
	clientHES, err := heshttp.New(heshttp.Config{BaseURL: fake.URL, HMACSecret: secret})
	if err != nil {
		t.Fatal(err)
	}

	st := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend := mock.NewPayments(), mock.NewVending()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, clientHES), box, pay, vend, clientHES, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Remote: clientHES, Log: log, Dev: true, DevPay: pay}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t, srv}

	code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "70011112222", "disco_code": "IKEDC"}, nil)
	if code != 201 {
		t.Fatalf("register %d %v", code, m)
	}
	code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{
		"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN",
	}, nil)
	if code != 201 || q["delivery_method"] != "REMOTE" {
		t.Fatalf("quote %d %v", code, q)
	}
	code, txn := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
		map[string]string{"Idempotency-Key": "heshttp-e2e-0123456"})
	if code != 202 {
		t.Fatalf("create %d %v", code, txn)
	}
	id := txn["transaction_id"].(string)
	ref := txn["checkout_url"].(string)[strings.LastIndex(txn["checkout_url"].(string), "/")+1:]
	c.do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{}, nil)
	c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"})

	code, got := c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
	if code != 200 || got["delivery_status"] != "ACCEPTED" {
		t.Fatalf("accepted via heshttp: %d %v", code, got)
	}

	fake.Settle()
	body := map[string]any{"idempotency_key": "credit:" + id}
	raw, _ := json.Marshal(body)
	sig := heshttp.Sign(secret, raw)
	if code, _ := c.do("POST", "/v1/webhooks/remote-credit/heshttp", "", body, map[string]string{"X-Meterly-Signature": sig}); code != 200 {
		t.Fatalf("heshttp webhook %d", code)
	}
	code, got = c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
	if code != 200 || got["delivery_status"] != "CONFIRMED" || got["credit_on_meter"] != true {
		t.Fatalf("confirmed via heshttp: %d %v", code, got)
	}
}

func TestAdminRemoteCohorts(t *testing.T) {
	st := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Remote: remote, Log: log}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	c := client{t, srv}

	body := map[string]any{"disco": "IKEDC", "number_prefix": "62", "manufacturer": "hexing", "evidence": "MAP letter March 2026 Hexing AMI"}
	if code, _ := c.do("POST", "/v1/admin/remote-credit/cohorts", "ada", body, nil); code != 403 {
		t.Fatalf("customer create cohort: %d", code)
	}
	if code, _ := c.do("POST", "/v1/admin/remote-credit/cohorts", "operator:ola", body, nil); code != 403 {
		t.Fatalf("operator must not create cohort: %d", code)
	}
	code, created := c.do("POST", "/v1/admin/remote-credit/cohorts", "admin:ada", body, nil)
	if code != 201 || created["disco"] != "IKEDC" || created["number_prefix"] != "62" {
		t.Fatalf("create %d %v", code, created)
	}
	if code, _ := c.do("POST", "/v1/admin/remote-credit/cohorts", "admin:ada", body, nil); code != 409 {
		t.Fatalf("duplicate cohort: %d", code)
	}
	code, list := c.do("GET", "/v1/admin/remote-credit/cohorts", "operator:ola", nil, nil)
	if code != 200 || len(list["data"].([]any)) != 1 {
		t.Fatalf("list %d %v", code, list)
	}
	id := created["id"].(string)
	if code, _ := c.do("DELETE", "/v1/admin/remote-credit/cohorts/"+id, "admin:ada", map[string]any{"reason": "model retired from IKEDC AMI"}, nil); code != 200 {
		t.Fatalf("revoke %d", code)
	}
}
