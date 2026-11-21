package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/paystack/paystacktest"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

const psSecret = "sk_test_e2e_secret_value"

type psEnv struct {
	c     client
	o     *orchestrator.Orchestrator
	fake  *paystacktest.Fake
	logs  *syncBuf
	clock time.Time
}

func newPaystackEnv(t *testing.T, st store.Store) *psEnv {
	e := &psEnv{logs: &syncBuf{}, clock: time.Now().UTC()}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.fake = paystacktest.New(psSecret)
	t.Cleanup(e.fake.Close)
	ps, err := paystack.New(paystack.Config{SecretKey: psSecret, BaseURL: e.fake.URL})
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	vend, remote := mock.NewVending(), mock.NewRemoteCredit()
	e.o = orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, ps, vend, remote, log)
	e.o.Now = func() time.Time { return e.clock }
	s := &httpapi.Server{O: e.o, Auth: auth.DevAuth{}, Payments: ps, Log: log, Dev: true}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	e.c = client{t, srv}
	return e
}

func (e *psEnv) webhook(t *testing.T, event, ref, signWith string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"event": event, "data": map[string]any{"reference": ref, "amount": 510000, "status": "success"}})
	req, _ := http.NewRequest("POST", e.c.srv.URL+"/v1/payments/webhooks/paystack", bytes.NewReader(body))
	req.Header.Set("X-Paystack-Signature", paystack.Sign(signWith, body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// purchase registers a meter, quotes ₦5,000 and creates a transaction.
func (e *psEnv) purchase(t *testing.T, meter, idemKey string) (code int, txn map[string]any) {
	t.Helper()
	_, m := e.c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": meter, "disco_code": "IKEDC"}, nil)
	_, q := e.c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN", "delivery_preference": "TOKEN"}, nil)
	return e.c.do("POST", "/v1/electricity/transactions", "ada",
		map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"}, map[string]string{"Idempotency-Key": idemKey})
}

func (e *psEnv) status(t *testing.T, id string) string {
	t.Helper()
	_, got := e.c.do("GET", "/v1/electricity/transactions/"+id, "ada", nil, nil)
	return got["status"].(string)
}

func eachStore(t *testing.T, fn func(t *testing.T, st store.Store)) {
	t.Run("memory", func(t *testing.T) { fn(t, memstore.New()) })
	t.Run("postgres", func(t *testing.T) { fn(t, pgstore.New(pgtest.NewPool(t), "SANDBOX")) })
}

// ref returns the Paystack reference we committed for a transaction.
func (e *psEnv) ref(t *testing.T, id string) string {
	t.Helper()
	for _, r := range e.fake.References() {
		if strings.HasPrefix(r, id+".") {
			tx, _ := e.o.Store().Repos().Transactions().ByPaymentRef(t.Context(), r)
			if tx != nil {
				return r
			}
		}
	}
	t.Fatalf("no committed Paystack reference for %s", id)
	return ""
}

func TestPaystackEndToEnd(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPaystackEnv(t, st)
		code, txn := e.purchase(t, "45012345678", "ps-e2e-0123456789ab")
		if code != 202 || !strings.HasPrefix(txn["checkout_url"].(string), "https://checkout.paystack.com/") {
			t.Fatalf("create %d %v", code, txn)
		}
		id := txn["transaction_id"].(string)
		ref := e.ref(t, id)

		// A webhook before payment changes nothing: verification says unpaid.
		if c := e.webhook(t, "charge.success", ref, psSecret); c != 200 || e.status(t, id) != "PAYMENT_PENDING" {
			t.Fatalf("premature webhook: %d %s", c, e.status(t, id))
		}
		e.fake.Pay(ref, 0)
		if c := e.webhook(t, "charge.success", ref, "sk_test_attacker"); c != 401 {
			t.Fatalf("forged webhook: %d", c)
		}
		if c := e.webhook(t, "refund.processed", ref, psSecret); c != 200 {
			t.Fatalf("ignored event: %d", c)
		}
		if c := e.webhook(t, "charge.success", ref, psSecret); c != 200 {
			t.Fatalf("webhook: %d", c)
		}
		if s := e.status(t, id); s != "COMPLETED" {
			t.Fatalf("got %s", s)
		}
		if strings.Contains(e.logs.String(), psSecret) {
			t.Fatal("secret key in logs")
		}
	})
}

// Paystack created the checkout but the response was lost. The client
// retries with the same Idempotency-Key and gets a working checkout.
func TestPaystackLostInitializeResponse(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPaystackEnv(t, st)
		e.fake.FailAfterApply("POST /transaction/initialize", 1)
		code, _ := e.purchase(t, "45012345678", "ps-lost-0123456789ab")
		if code != 502 {
			t.Fatalf("want 502, got %d", code)
		}
		_, txns := e.c.do("GET", "/v1/electricity/transactions", "ada", nil, nil)
		list := txns["data"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["payment_status"] != "CREATED" {
			t.Fatalf("want one CREATED transaction, got %v", list)
		}
		id := list[0].(map[string]any)["transaction_id"].(string)
		// Retry: same key and same body => same transaction, now with a checkout.
		code, again := e.c.do("POST", "/v1/electricity/transactions", "ada",
			map[string]any{"quote_id": e.quoteOf(t, id), "email": "ada@example.com"}, map[string]string{"Idempotency-Key": "ps-lost-0123456789ab"})
		if code != 202 || again["transaction_id"] != id || again["checkout_url"] == "" {
			t.Fatalf("retry %d %v", code, again)
		}
		if n := len(e.fake.References()); n != 2 {
			t.Fatalf("want an orphaned first attempt plus the committed one, got %d", n)
		}
		e.fake.Pay(e.ref(t, id), 0)
		e.webhook(t, "charge.success", e.ref(t, id), psSecret)
		if s := e.status(t, id); s != "COMPLETED" {
			t.Fatalf("got %s", s)
		}
	})
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *psEnv) quoteOf(t *testing.T, id string) string {
	t.Helper()
	tx, err := e.o.Get(t.Context(), mustUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return tx.QuoteID.String()
}

// Vending fails, the refund reaches Paystack but its response is lost; the
// worker retries and Paystack ends up with exactly one refund.
func TestPaystackRefundWithLostResponse(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPaystackEnv(t, st)
		_, txn := e.purchase(t, "45012340000", "ps-refund-0123456789")
		id := txn["transaction_id"].(string)
		ref := e.ref(t, id)
		e.fake.Pay(ref, 0)
		e.fake.FailAfterApply("POST /refund", 1)
		e.webhook(t, "charge.success", ref, psSecret)
		if s := e.status(t, id); s != "REFUND_PENDING" {
			t.Fatalf("got %s", s)
		}
		e.clock = e.clock.Add(10 * time.Minute)
		if _, _, err := e.o.ResumeDue(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := e.status(t, id); s != "REFUNDED" || e.fake.Refunds() != 1 {
			t.Fatalf("got %s with %d refunds", s, e.fake.Refunds())
		}
	})
}

// No webhook ever arrives; the worker verifies with Paystack and completes.
func TestPaystackMissedWebhook(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPaystackEnv(t, st)
		_, txn := e.purchase(t, "45012345678", "ps-missed-0123456789")
		id := txn["transaction_id"].(string)
		e.fake.Pay(e.ref(t, id), 0)
		e.clock = e.clock.Add(10 * time.Minute)
		if _, progressed, err := e.o.ResumeDue(t.Context()); err != nil || progressed != 1 {
			t.Fatalf("progressed %d err %v", progressed, err)
		}
		if s := e.status(t, id); s != "COMPLETED" {
			t.Fatalf("got %s", s)
		}
	})
}

// Underpayment reported by Paystack goes to review and is never vended.
func TestPaystackUnderpayment(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		e := newPaystackEnv(t, st)
		_, txn := e.purchase(t, "45012345678", "ps-under-0123456789a")
		id := txn["transaction_id"].(string)
		ref := e.ref(t, id)
		e.fake.Pay(ref, 100)
		e.webhook(t, "charge.success", ref, psSecret)
		if s := e.status(t, id); s != "MANUAL_REVIEW" {
			t.Fatalf("got %s", s)
		}
	})
}
