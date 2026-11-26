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

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
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

func TestReadyzDegradesWhenVendingPaused(t *testing.T) {
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, slog.Default())
	vend.SetWalletMinor(0)
	o.SetWallet(vend, 50_000)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: slog.Default()}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}
	code, body := c.do("GET", "/readyz", "", nil, nil)
	if code != 200 || body["status"] != "degraded" || body["vending"] != "paused" {
		t.Fatalf("readyz %d %v", code, body)
	}
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
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay, DevRemote: remote}
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
	if code, _ := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"]}, map[string]string{"Idempotency-Key": "no-email-0123456789"}); code != 400 {
		t.Fatalf("missing email must be rejected before anything is created: %d", code)
	}
	body := map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"}
	code, txn := c.do("POST", "/v1/electricity/transactions", "ada", body, idem)
	if code != 202 || txn["status"] != "PAYMENT_PENDING" {
		t.Fatalf("create %d %v", code, txn)
	}
	id := txn["transaction_id"].(string)
	if code, again := c.do("POST", "/v1/electricity/transactions", "ada", body, idem); code != 202 || again["transaction_id"] != id {
		t.Fatalf("replay %d %v", code, again)
	}
	if code, _ := c.do("POST", "/v1/electricity/transactions", "ada", map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com", "is_gift": true}, idem); code != 409 {
		t.Fatalf("idempotency conflict: %d", code)
	}

	if code, _ := c.do("POST", "/v1/electricity/transactions/"+id+"/token/shares", "ada", map[string]any{"channel": "COPY"}, map[string]string{"Idempotency-Key": "pending-copy-123456"}); code != 409 {
		t.Fatalf("pending token copy accepted: %d", code)
	}
	// A pending record is not presented as successful.
	if code, receipt := c.do("GET", "/v1/electricity/transactions/"+id+"/receipt", "ada", nil, nil); code != 200 || receipt["payment_status"] != "PENDING" || receipt["overall_status"] != "PAYMENT_PENDING" {
		t.Fatalf("pending receipt %d %v", code, receipt)
	}
	if code, _ := c.do("GET", "/v1/electricity/transactions/"+id+"/receipt", "eve", nil, nil); code != 404 {
		t.Fatalf("other user read receipt: %d", code)
	}
	if code, _ := c.do("GET", "/v1/electricity/transactions/"+id+"/receipt?format=pdf", "ada", nil, nil); code != 400 {
		t.Fatalf("unsupported format: %d", code)
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
	// The HTML version is private and contains neither raw meter number nor token.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/electricity/transactions/"+id+"/receipt?format=html", nil)
	req.Header.Set("Authorization", "Bearer ada")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	html, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(html), "CONFIRMED") || strings.Contains(string(html), meter) || strings.Contains(string(html), tok["token"].(string)) {
		t.Fatalf("invalid or sensitive HTML receipt, status=%d", resp.StatusCode)
	}
	path := "/v1/electricity/transactions/" + id + "/token/shares"
	key := map[string]string{"Idempotency-Key": "copy-action-123456789"}
	if code, _ := c.do("POST", path, "eve", map[string]any{"channel": "COPY"}, key); code != 404 {
		t.Fatalf("other user recorded copy: %d", code)
	}
	if code, _ := c.do("POST", path, "ada", map[string]any{"channel": "SMS"}, key); code != 422 {
		t.Fatalf("SMS send without a verified phone must be 422, got %d", code)
	}
	code, share := c.do("POST", path, "ada", map[string]any{"channel": "COPY"}, key)
	if code != 201 {
		t.Fatalf("copy record: %d %v", code, share)
	}
	code, replay := c.do("POST", path, "ada", map[string]any{"channel": "COPY"}, key)
	if code != 201 || share["share"].(map[string]any)["share_id"] != replay["share"].(map[string]any)["share_id"] {
		t.Fatalf("copy replay: %d %v", code, replay)
	}
	if code, list := c.do("GET", path, "ada", nil, nil); code != 200 || len(list["data"].([]any)) != 1 {
		t.Fatalf("copy history: %d %v", code, list)
	}
	if code, _ := c.do("GET", path, "eve", nil, nil); code != 404 {
		t.Fatalf("other user read copy history: %d", code)
	}
	transactionID := uuid.MustParse(id)
	transaction, err := o.Get(context.Background(), transactionID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	ids := make(chan uuid.UUID, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := o.RecordTokenCopy(context.Background(), transaction.CustomerID, transactionID, "parallel-copy-123456")
			if e != nil {
				errs <- e
				return
			}
			ids <- v.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for e := range errs {
		t.Errorf("concurrent copy: %v", e)
	}
	var first uuid.UUID
	for got := range ids {
		if first == uuid.Nil {
			first = got
		}
		if got != first {
			t.Fatal("duplicate copy records")
		}
	}
	copies, err := o.ListTokenCopies(context.Background(), transaction.CustomerID, transactionID)
	if err != nil || len(copies) != 2 {
		t.Fatalf("copy count: %d %v", len(copies), err)
	}
	auditRows, err := st.Repos().Audit().ForResource(context.Background(), "electricity_transaction", transactionID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range auditRows {
		if a.Action == "token.copy_recorded" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("copy audit count: %d", count)
	}
	if b, _ := json.Marshal(share); strings.Contains(string(b), tok["token"].(string)) || strings.Contains(string(b), meter) {
		t.Fatal("copy metadata leaked sensitive data")
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

	// Use a fresh server/rate limiter for a separate gift flow.
	giftSrv := httptest.NewServer(s.Routes())
	defer giftSrv.Close()
	c = client{t, giftSrv}
	// Gift contact is optional, encrypted, bound to this transaction, and not sent.
	code, giftQuote := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
	if code != 201 {
		t.Fatalf("gift quote: %d", code)
	}
	phone := "+2348000000123" // synthetic only
	giftKey := map[string]string{"Idempotency-Key": "gift-contact-123456789"}
	giftBody := map[string]any{"quote_id": giftQuote["quote_id"], "email": "ada@example.com", "is_gift": true, "recipient_name": "Family", "recipient_phone": phone, "notify_recipient": true}
	code, giftResponse := c.do("POST", "/v1/electricity/transactions", "ada", giftBody, giftKey)
	if code != 202 {
		t.Fatalf("gift create: %d %v", code, giftResponse)
	}
	giftID := uuid.MustParse(giftResponse["transaction_id"].(string))
	giftRecord, err := st.Repos().Gifts().Get(context.Background(), giftID)
	if err != nil {
		t.Fatal(err)
	}
	if !giftRecord.NotificationRequested || giftRecord.ContactKeyVersion != "t1" || strings.Contains(string(giftRecord.ContactCiphertext), phone) {
		t.Fatal("gift contact not protected")
	}
	contact, err := box.DecryptBound(giftRecord.ContactCiphertext, []byte("gift-contact:"+giftID.String()))
	if err != nil || contact != phone {
		t.Fatal("gift contact could not be recovered")
	}
	if _, err := box.DecryptBound(giftRecord.ContactCiphertext, []byte("gift-contact:"+uuid.NewString())); err == nil {
		t.Fatal("gift ciphertext not bound to transaction")
	}
	code, againGift := c.do("POST", "/v1/electricity/transactions", "ada", giftBody, giftKey)
	if code != 202 || againGift["transaction_id"] != giftID.String() {
		t.Fatal("gift idempotent replay failed")
	}
	changedBody := map[string]any{}
	for k, v := range giftBody {
		changedBody[k] = v
	}
	changedBody["recipient_phone"] = "+2348000000456"
	if code, _ := c.do("POST", "/v1/electricity/transactions", "ada", changedBody, giftKey); code != 409 {
		t.Fatalf("changed recipient phone not rejected: %d", code)
	}
	if b, _ := json.Marshal(giftResponse); strings.Contains(string(b), phone) {
		t.Fatal("gift phone leaked in response")
	}
	events, err := st.Repos().Outbox().ForAggregate(context.Background(), giftID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(events); strings.Contains(string(b), phone) {
		t.Fatal("gift phone leaked into outbox")
	}
	if strings.Contains(logs.String(), phone) {
		t.Fatal("gift phone leaked in logs")
	}
	code, invalidQuote := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
	if code != 201 {
		t.Fatal("invalid gift quote")
	}
	invalid := map[string]any{"quote_id": invalidQuote["quote_id"], "email": "ada@example.com", "is_gift": true, "recipient_phone": phone}
	if code, _ := c.do("POST", "/v1/electricity/transactions", "ada", invalid, map[string]string{"Idempotency-Key": "gift-invalid-1234567"}); code != 400 {
		t.Fatalf("unsolicited contact accepted: %d", code)
	}
	// Registration is not evidence of ownership and cannot create MANAGE.
	accessSrv := httptest.NewServer(s.Routes())
	defer accessSrv.Close()
	access := client{t, accessSrv}
	meterPath := "/v1/meters/" + m["meter_id"].(string)
	if code, _ := access.do("DELETE", meterPath, "mallory", nil, nil); code != 404 {
		t.Fatalf("unrelated user removed access: %d", code)
	}
	code, registered := access.do("POST", "/v1/meters", "eve", map[string]any{"meter_number": meter, "disco_code": "IKEDC", "label": "Shared"}, nil)
	if code != 201 || registered["meter_id"] != m["meter_id"] {
		t.Fatalf("registration: %d %v", code, registered)
	}
	code, eveList := access.do("GET", "/v1/meters", "eve", nil, nil)
	if code != 200 {
		t.Fatal("list failed")
	}
	entries := eveList["data"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["permission"] != "PURCHASE" {
		t.Fatalf("registration created elevated access: %v", eveList)
	}
	if code, _ := access.do("GET", "/v1/electricity/transactions/"+id+"/token", "eve", nil, nil); code != 404 {
		t.Fatalf("other purchaser's token exposed: %d", code)
	}
	code, preQuote := access.do("POST", "/v1/electricity/quotes", "eve", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
	if code != 201 {
		t.Fatalf("quote before removal %d", code)
	}
	if code, _ := access.do("DELETE", meterPath, "eve", nil, nil); code != 204 {
		t.Fatalf("self removal failed: %d", code)
	}
	if code, _ := access.do("DELETE", meterPath, "eve", nil, nil); code != 404 {
		t.Fatalf("removed user still has access: %d", code)
	}
	if code, _ := access.do("GET", meterPath, "ada", nil, nil); code != 200 {
		t.Fatalf("another user's access affected: %d", code)
	}
	if code, _ := access.do("GET", meterPath, "eve", nil, nil); code != 403 {
		t.Fatalf("removed access still usable: %d", code)
	}
	if code, _ := access.do("POST", "/v1/electricity/transactions", "eve", map[string]any{"quote_id": preQuote["quote_id"], "email": "eve@example.com"}, map[string]string{"Idempotency-Key": "removed-access-123456"}); code != 403 {
		t.Fatalf("revoked quote used: %d", code)
	}
	meterID := uuid.MustParse(m["meter_id"].(string))
	audits, err := st.Repos().Audit().ForResource(context.Background(), "meter", meterID)
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, a := range audits {
		if a.Action == "meter.self_access_removed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("self removal audit count %d", count)
	}
	if code, _ := access.do("POST", "/v1/meters", "eve", map[string]any{"meter_number": meter, "disco_code": "IKEDC"}, nil); code != 201 {
		t.Fatalf("self-registration after removal failed: %d", code)
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
