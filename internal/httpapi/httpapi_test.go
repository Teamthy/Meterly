package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func (c client) do(method, path, user string, body any, hdr map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestEndToEndPurchase(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }},
		{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) { endToEnd(t, b.open(t)) })
	}
}

func endToEnd(t *testing.T, st store.Store) {
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: httpapi.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay, DevRemote: remote}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}

	const meter = "45012345678"
	code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": meter, "disco_code": "IKEDC", "label": "Home"}, nil)
	if code != 201 {
		t.Fatalf("register %d %v", code, m)
	}
	code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
	if code != 201 {
		t.Fatalf("quote %d %v", code, q)
	}
	idem := map[string]string{"Idempotency-Key": "e2e-0123456789abcdef"}
	body := map[string]any{"quote_id": q["quote_id"]}
	code, txn := c.do("POST", "/v1/electricity/transactions", "ada", body, idem)
	if code != 202 || txn["status"] != "PAYMENT_PENDING" {
		t.Fatalf("create %d %v", code, txn)
	}
	id := txn["transaction_id"].(string)
	if code, again := c.do("POST", "/v1/electricity/transactions", "ada", body, idem); code != 202 || again["transaction_id"] != id {
		t.Fatalf("replay %d %v", code, again)
	}
	if code, _ := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "is_gift": true}, idem); code != 409 {
		t.Fatalf("idempotency conflict: %d", code)
	}

	ref := txn["checkout_url"].(string)[strings.LastIndex(txn["checkout_url"].(string), "/")+1:]
	c.do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{}, nil)
	if code, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, nil); code != 401 {
		t.Fatalf("unsigned webhook accepted: %d", code)
	}
	if code, _ := c.do("POST", "/v1/payments/webhooks/mockpay", "", map[string]any{"reference": ref}, map[string]string{"X-Mock-Signature": "dev-signature"}); code != 200 {
		t.Fatalf("webhook %d", code)
	}
	code, got := c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
	if code != 200 || got["status"] != "COMPLETED" {
		t.Fatalf("status %d %v", code, got["status"])
	}
	if code, _ := c.do("GET", "/v1/electricity/transactions/"+id, "eve", nil, nil); code != 404 {
		t.Fatalf("other user read transaction: %d", code)
	}
	code, tok := c.do("GET", "/v1/electricity/transactions/"+id+"/token", "ada", nil, nil)
	if code != 200 {
		t.Fatalf("token %d %v", code, tok)
	}
	token := tok["token"].(string)
	if code, _ := c.do("GET", "/v1/electricity/transactions/"+id+"/token", "eve", nil, nil); code != 404 {
		t.Fatalf("other user read token: %d", code)
	}
	if code, list := c.do("GET", "/v1/electricity/transactions", "ada", nil, nil); code != 200 || len(list["data"].([]any)) != 1 {
		t.Fatalf("list %d %v", code, list)
	}
	if code, _ := c.do("GET", "/readyz", "", nil, nil); code != 200 {
		t.Fatalf("readyz %d", code)
	}

	// Sensitive values must never reach the logs.
	out := logs.String()
	digits := strings.ReplaceAll(token, "-", "")
	for _, secret := range []string{token, digits, meter} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output contains sensitive value %q", secret)
		}
	}
	_ = context.Background()
}
