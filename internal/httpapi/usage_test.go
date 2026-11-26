package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestMeterUsageFromBackend(t *testing.T) {
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

	rr := do("POST", "/v1/meters", "ada", map[string]any{
		"meter_number": "70011112222", "disco_code": "IKEDC", "label": "Home",
	})
	if rr.Code != 201 {
		t.Fatalf("register smart %d %s", rr.Code, rr.Body.String())
	}
	var smart struct {
		MeterID string `json:"meter_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &smart)
	id, err := uuid.Parse(smart.MeterID)
	if err != nil {
		t.Fatal(err)
	}

	rr = do("POST", "/v1/dev/meters/"+smart.MeterID+"/reading", "", map[string]any{"kwh": 82})
	if rr.Code != 200 {
		t.Fatalf("set reading %d %s", rr.Code, rr.Body.String())
	}
	if err := st.Repos().Readings().InsertSample(t.Context(), meters.ReadingSample{
		MeterID: id, MilliKWh: 100000, ReadAt: time.Now().UTC().Add(-6 * time.Hour), Provider: "mockreadings",
	}); err != nil {
		t.Fatal(err)
	}

	rr = do("GET", "/v1/meters/"+smart.MeterID+"/usage", "ada", nil)
	if rr.Code != 200 {
		t.Fatalf("usage %d %s", rr.Code, rr.Body.String())
	}
	var usage orchestrator.MeterUsage
	if err := json.Unmarshal(rr.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.RemainingKWh == nil || *usage.RemainingKWh != 82 {
		t.Fatalf("remaining %+v", usage.RemainingKWh)
	}
	if usage.RemainingSource != "remote_reading" {
		t.Fatalf("source %s", usage.RemainingSource)
	}
	if usage.Consumption == nil || usage.Consumption.KWhPerHour < 2.9 || usage.Consumption.KWhPerHour > 3.1 {
		t.Fatalf("consumption %+v", usage.Consumption)
	}
	if usage.EstimatedHoursRemaining == nil {
		t.Fatal("expected eta from backend")
	}

	rr = do("POST", "/v1/meters", "ada", map[string]any{
		"meter_number": "45011112222", "disco_code": "EKEDC", "label": "Keypad",
	})
	if rr.Code != 201 {
		t.Fatalf("register keypad %d %s", rr.Code, rr.Body.String())
	}
	var keypad struct {
		MeterID string `json:"meter_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &keypad)
	rr = do("GET", "/v1/meters/"+keypad.MeterID+"/usage", "ada", nil)
	if rr.Code != 200 {
		t.Fatalf("keypad usage %d %s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &usage)
	if usage.RemainingKWh != nil || usage.UnavailableReason != "REMOTE_READING_NOT_SUPPORTED" {
		t.Fatalf("keypad should not invent remaining: %+v", usage)
	}

	rr = do("GET", "/v1/meters/usage", "ada", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), smart.MeterID) {
		t.Fatalf("list usage %d %s", rr.Code, rr.Body.String())
	}
}
