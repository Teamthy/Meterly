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
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestDevConsoleFromServer(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	readings := mock.NewMeterReadings()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	orch.SetMeterReadingProvider(readings)

	prod := &httpapi.Server{O: orch, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: false}
	prodH := prod.Routes()
	rr := httptest.NewRecorder()
	prodH.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/dev/console", nil))
	if rr.Code != 404 {
		t.Fatalf("production must not expose /v1/dev/console: %d %s", rr.Code, rr.Body.String())
	}

	s := &httpapi.Server{
		O: orch, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true,
		DevPay: pay, DevRemote: remote, DevReadings: readings,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()
	do := func(method, path, bearer string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rdr = strings.NewReader(string(raw))
		}
		req := httptest.NewRequest(method, path, rdr)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rr = do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "70011112222", "disco_code": "IKEDC"})
	if rr.Code != 201 {
		t.Fatalf("register %d %s", rr.Code, rr.Body.String())
	}
	var meter struct {
		MeterID string `json:"meter_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &meter)

	rr = do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": meter.MeterID, "amount_minor": 500000, "currency": "NGN"})
	if rr.Code != 200 && rr.Code != 201 {
		t.Fatalf("quote %d %s", rr.Code, rr.Body.String())
	}
	var quote struct {
		QuoteID string `json:"quote_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &quote)

	req := httptest.NewRequest("POST", "/v1/electricity/transactions", strings.NewReader(`{"quote_id":"`+quote.QuoteID+`","email":"ada@example.com"}`))
	req.Header.Set("Authorization", "Bearer ada")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "dev-console-test-key-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("txn %d %s", rec.Code, rec.Body.String())
	}
	var txn map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &txn)
	url, _ := txn["checkout_url"].(string)
	ref := url[strings.LastIndex(url, "/")+1:]

	rr = do("GET", "/v1/dev/console", "", nil)
	if rr.Code != 200 {
		t.Fatalf("console %d %s", rr.Code, rr.Body.String())
	}
	var cons httpapi.DevConsole
	if err := json.Unmarshal(rr.Body.Bytes(), &cons); err != nil {
		t.Fatal(err)
	}
	if !cons.Enabled || !strings.Contains(cons.Copy, "Prefix-7") || !strings.Contains(cons.Copy, "MODE=dev") {
		t.Fatalf("copy %+v", cons)
	}
	if len(cons.Meters) != 1 || cons.Meters[0].MeterID.String() != meter.MeterID || !cons.Meters[0].RemoteUsable {
		t.Fatalf("meters %+v", cons.Meters)
	}
	if cons.Meters[0].Masked == "70011112222" {
		t.Fatal("console must not return the full meter number")
	}
	if len(cons.Payments) != 1 || cons.Payments[0].Reference != ref || !cons.Payments[0].Complete.Enabled {
		t.Fatalf("payments %+v", cons.Payments)
	}
	if !cons.Remote.Settle.Enabled {
		t.Fatalf("remote %+v", cons.Remote)
	}

	rr = do("POST", "/v1/dev/payments/"+ref+"/complete", "", map[string]any{"success": true})
	if rr.Code != 200 {
		t.Fatalf("complete %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/v1/dev/console", "", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &cons)
	if len(cons.Payments) != 1 || cons.Payments[0].Complete.Enabled {
		t.Fatalf("paid payment should disable complete %+v", cons.Payments)
	}

	rr = do("POST", "/v1/dev/meters/"+meter.MeterID+"/reading", "", map[string]any{"kwh": 82})
	if rr.Code != 200 {
		t.Fatalf("reading %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/v1/dev/console", "", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &cons)
	if !strings.Contains(cons.Meters[0].ReadingLabel, "82") {
		t.Fatalf("reading label %q", cons.Meters[0].ReadingLabel)
	}
}
