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

func TestCatalogAndBuyOptionsAndCheckoutFromServer(t *testing.T) {
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
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := do("GET", "/v1/electricity/catalog", "ada", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"IKEDC"`) || !strings.Contains(rr.Body.String(), `"ngn-500"`) {
		t.Fatalf("catalog %d %s", rr.Code, rr.Body.String())
	}

	rr = do("POST", "/v1/meters", "ada", map[string]any{
		"meter_number": "70011112222", "disco_code": "IKEDC", "label": "Home",
	})
	if rr.Code != 201 {
		t.Fatalf("register %d %s", rr.Code, rr.Body.String())
	}
	var meter struct {
		MeterID string `json:"meter_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &meter)

	rr = do("GET", "/v1/meters/"+meter.MeterID+"/buy-options", "ada", nil)
	if rr.Code != 200 {
		t.Fatalf("buy-options %d %s", rr.Code, rr.Body.String())
	}
	var opts orchestrator.BuyOptions
	if err := json.Unmarshal(rr.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.Tariff.KoboPerKWh <= 0 || len(opts.Packs) == 0 || opts.RecommendedPackID == "" {
		t.Fatalf("buy-options %+v", opts)
	}
	for _, p := range opts.Packs {
		if p.EstimatedUnitsKWh <= 0 {
			t.Fatalf("pack units must come from server: %+v", p)
		}
	}

	rr = do("POST", "/v1/electricity/quotes", "ada", map[string]any{
		"meter_id": meter.MeterID, "pack_id": "ngn-500", "currency": "NGN", "delivery_preference": "AUTO",
		"confirm_customer_name": true,
	})
	if rr.Code != 201 {
		t.Fatalf("quote %d %s", rr.Code, rr.Body.String())
	}
	var quote map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &quote)
	if quote["total_minor"] != float64(48000) {
		t.Fatalf("pay amount %+v", quote["total_minor"])
	}
	conf, _ := quote["confirmation"].(map[string]any)
	if conf == nil || conf["cta_label"] == "" {
		t.Fatalf("confirmation missing: %s", rr.Body.String())
	}
	if _, ok := quote["estimated_units_kwh"].(float64); !ok {
		t.Fatalf("units missing: %s", rr.Body.String())
	}

	txnBody := map[string]any{"quote_id": quote["quote_id"], "email": "ada@example.com"}
	postTxn := func() *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(txnBody)
		req := httptest.NewRequest("POST", "/v1/electricity/transactions", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer ada")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "buy-options-test-key-01")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rr = postTxn()
	if rr.Code != 202 {
		t.Fatalf("txn %d %s", rr.Code, rr.Body.String())
	}
	var txn map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &txn)
	checkout, _ := txn["checkout"].(map[string]any)
	if checkout == nil || checkout["cta_label"] == "" || checkout["total_minor"] != float64(48000) {
		t.Fatalf("checkout missing: %s", rr.Body.String())
	}
	if txn["checkout_url"] == "" {
		t.Fatal("checkout_url")
	}

	rr = postTxn()
	if rr.Code != 202 {
		t.Fatalf("idempotent replay %d %s", rr.Code, rr.Body.String())
	}
}
