// Command partner-demo drives a sandbox partner through validate → quote →
// purchase, receives signed webhooks, and reconciles float.
//
//	MODE=dev go run ./cmd/api
//	go run ./scripts/partner-demo
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/teamthy/meterly/internal/partners"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "partner-demo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	api := getenv("API_URL", "http://127.0.0.1:8080")
	opA := getenv("OPERATOR_A", "operator:ada")
	opB := getenv("OPERATOR_B", "operator:bola")

	type rec struct {
		body []byte
		sig  string
	}
	var mu sync.Mutex
	received := map[string]rec{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	hookURL := "http://" + ln.Addr().String() + "/hooks"
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received[r.Header.Get("Meterly-Event-Type")] = rec{b, r.Header.Get("Meterly-Signature")}
		mu.Unlock()
		w.WriteHeader(200)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	created, err := do(api, "POST", "/v1/admin/partners", opA, map[string]any{
		"reason": "partner-demo sandbox", "organisation": "Demo Partner",
		"environment": "SANDBOX", "contact_ref": "demo",
		"daily_value_cap_minor": 0, "rate_limit_per_minute": 60,
	})
	if err != nil {
		return err
	}
	pid := str(created["id"])
	issued, err := do(api, "POST", "/v1/admin/partners/"+pid+"/keys", opA, map[string]any{
		"reason": "demo sandbox key",
		"scopes": []string{
			partners.ScopeMetersValidate, partners.ScopeQuotesCreate,
			partners.ScopePurchasesCreate, partners.ScopePurchasesRead,
			partners.ScopeFloatRead, partners.ScopeWebhooksManage,
		},
	})
	if err != nil {
		return err
	}
	secret := str(issued["secret"])

	proposed, err := do(api, "POST", "/v1/admin/partners/"+pid+"/float/topups", opA, map[string]any{
		"reason": "demo bank receipt", "receipt_ref": fmt.Sprintf("DEMO-%d", time.Now().UnixNano()),
		"amount_minor": 2_000_000, "currency": "NGN",
	})
	if err != nil {
		return err
	}
	if _, err := do(api, "POST", "/v1/admin/partners/"+pid+"/float/topups/"+str(proposed["id"])+"/approve", opB, map[string]any{
		"reason": "cleared for demo",
	}); err != nil {
		return err
	}

	wh, err := do(api, "POST", "/v1/partner/webhooks", secret, map[string]any{
		"url": hookURL, "description": "partner-demo",
		"events": []string{
			partners.EventPaymentConfirmed, partners.EventVended, partners.EventDelivered,
			partners.EventFailed, partners.EventRefunded, partners.EventUnknown,
		},
		"include_token": true,
	})
	if err != nil {
		return fmt.Errorf("register webhook (API must allow loopback in MODE=dev): %w", err)
	}
	whSecret := str(wh["secret"])

	if _, err := do(api, "POST", "/v1/partner/meters/validate", secret, map[string]any{
		"meter_number": "45012345678", "disco_code": "IKEDC",
	}); err != nil {
		return err
	}
	quote, err := do(api, "POST", "/v1/partner/quotes", secret, map[string]any{
		"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
	})
	if err != nil {
		return err
	}
	purchase, err := doHdr(api, "POST", "/v1/partner/purchases", secret, map[string]any{
		"quote_id": quote["quote_id"], "partner_ref": fmt.Sprintf("demo-%d", time.Now().UnixNano()),
	}, map[string]string{"Idempotency-Key": fmt.Sprintf("demo-idem-%d", time.Now().UnixNano())})
	if err != nil {
		return err
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, pay := received[partners.EventPaymentConfirmed]
		_, vend := received[partners.EventVended]
		_, del := received[partners.EventDelivered]
		mu.Unlock()
		if pay && vend && del {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	got := received
	mu.Unlock()
	for _, typ := range []string{partners.EventPaymentConfirmed, partners.EventVended, partners.EventDelivered} {
		ev, ok := got[typ]
		if !ok {
			return fmt.Errorf("missing webhook %s (is RUN_WORKER on?)", typ)
		}
		if err := partners.VerifySignature([]string{whSecret}, ev.sig, ev.body, time.Now().UTC(), partners.WebhookReplayWindow); err != nil {
			return fmt.Errorf("verify %s: %w", typ, err)
		}
	}

	gotP, err := do(api, "GET", "/v1/partner/purchases/"+str(purchase["transaction_id"]), secret, nil)
	if err != nil {
		return err
	}
	fl, err := do(api, "GET", "/v1/partner/float", secret, nil)
	if err != nil {
		return err
	}
	total := asInt(purchase["total_minor"])
	if total <= 0 {
		total = asInt(gotP["total_minor"])
	}
	avail := asInt(fl["available_minor"])
	want := 2_000_000 - total
	if avail != want {
		return fmt.Errorf("float not reconciled: available_minor=%d want %d (funded 2000000 - total %d)", avail, want, total)
	}
	if str(gotP["status"]) == "" {
		return fmt.Errorf("purchase missing status: %v", gotP)
	}
	fmt.Printf("ok partner=%s purchase=%s status=%s float_available=%d events=%d\n",
		pid, str(purchase["transaction_id"]), str(gotP["status"]), avail, len(got))
	return nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func do(api, method, path, bearer string, body any) (map[string]any, error) {
	return doHdr(api, method, path, bearer, body, nil)
}

func doHdr(api, method, path, bearer string, body any, hdr map[string]string) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(api, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if res.StatusCode >= 300 {
		return out, fmt.Errorf("%s %s -> %d %s", method, path, res.StatusCode, raw)
	}
	return out, nil
}
