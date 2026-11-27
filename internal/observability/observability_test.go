package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/providers"
)

func TestPrometheusExpositionAndLabelSafety(t *testing.T) {
	reg := observability.NewRegistry()
	reg.ObserveHTTPRequest("GET /v1/meters/{meter_id}", 200, 12*time.Millisecond)
	reg.ObserveHTTPRequest("", 404, 1*time.Millisecond) // unmatched path collapses to UNMATCHED
	reg.ObserveProviderCall("paystack", "verify_payment", observability.OutcomeOK, 40*time.Millisecond)
	reg.ObserveProviderCall("mockvend", "vend", observability.OutcomeAmbiguous, 5*time.Second)
	reg.ObserveProviderCall("resend", "send_notification", observability.OutcomeDefinitive, 20*time.Millisecond)
	reg.SetTransactionGauges(map[string]int64{"COMPLETED": 7, "MANUAL_REVIEW": 2}, 2, 1.5)
	reg.IncResumeEscalations()
	reg.IncNotificationDelivery("SMS", "SENT")
	reg.TouchWorkerHeartbeat(time.Unix(1_700_000_000, 0).UTC())

	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()

	// Non-/metrics path returns 404; POST /metrics returns 405.
	if res, err := http.Get(srv.URL + "/other"); err != nil || res.StatusCode != 404 {
		t.Fatalf("want 404 on /other, got %v %+v", err, res)
	}
	if res, err := http.Post(srv.URL+"/metrics", "text/plain", nil); err != nil || res.StatusCode != 405 {
		t.Fatalf("want 405 on POST /metrics, got %v %+v", err, res)
	}

	res, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	bodyBytes, _ := io.ReadAll(res.Body)
	body := string(bodyBytes)

	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("unexpected metrics response: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	for _, want := range []string{
		`http_requests_total{route="GET /v1/meters/{meter_id}",code="200"} 1`,
		`http_requests_total{route="UNMATCHED",code="404"} 1`,
		`http_request_duration_seconds_bucket{route="GET /v1/meters/{meter_id}",le="0.025"} 1`,
		`provider_calls_total{provider="paystack",op="verify_payment",outcome="ok"} 1`,
		`provider_calls_total{provider="mockvend",op="vend",outcome="ambiguous"} 1`,
		`provider_calls_total{provider="resend",op="send_notification",outcome="definitive"} 1`,
		`provider_call_duration_seconds_count{provider="paystack",op="verify_payment"} 1`,
		`transactions_by_status{status="COMPLETED"} 7`,
		`transactions_by_status{status="MANUAL_REVIEW"} 2`,
		`manual_review_open 2`,
		`resume_escalations_total 1`,
		`outbox_lag_seconds 1.5`,
		`notification_deliveries_total{channel="SMS",status="SENT"} 1`,
		`worker_heartbeat_unix 1700000000`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing metric line %q in:\n%s", want, body)
		}
	}

	if observability.OutcomeOf(nil) != observability.OutcomeOK ||
		observability.OutcomeOf(providers.ErrAmbiguous) != observability.OutcomeAmbiguous ||
		observability.OutcomeOf(errors.New("bad")) != observability.OutcomeDefinitive {
		t.Fatal("OutcomeOf classification mismatch")
	}
}

func TestOpenTelemetryNoopAndOTLPExport(t *testing.T) {
	// 1. Empty endpoint -> strict no-op.
	noop := observability.NewTracer(observability.TracerConfig{ServiceName: "meterly-test"})
	if noop.Enabled() {
		t.Fatal("empty endpoint must disable tracer")
	}
	ctx, sp := noop.StartSpan(context.Background(), "http.request", observability.SpanKindServer)
	sp.End(false)
	if err := noop.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. Configured endpoint -> exports parent/child spans with correlation ID over OTLP/HTTP JSON.
	var (
		mu       sync.Mutex
		received []map[string]any
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer collector.Close()

	tr := observability.NewTracer(observability.TracerConfig{
		ServiceName:  "meterly-api",
		OTLPEndpoint: collector.URL,
	})
	cid := uuid.New()
	reqCtx := audit.WithCorrelation(context.Background(), cid)
	inHdr := http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}
	reqCtx = tr.ExtractHTTP(reqCtx, inHdr)

	reqCtx, rootSpan := tr.StartSpan(reqCtx, "GET /v1/meters", observability.SpanKindServer,
		observability.StringAttr("http.route", "GET /v1/meters"))
	childCtx, childSpan := tr.StartSpan(reqCtx, "provider.mockvend.validate_meter", observability.SpanKindClient,
		observability.StringAttr("meterly.provider", "mockvend"))

	outHdr := http.Header{}
	observability.InjectHTTP(childCtx, outHdr)
	if !strings.HasPrefix(outHdr.Get("Traceparent"), "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("traceparent did not propagate trace ID: %q", outHdr.Get("Traceparent"))
	}

	childSpan.End(false)
	rootSpan.End(false)

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tr.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("expected OTLP spans exported to collector")
	}
	rawJSON, _ := json.Marshal(received)
	s := string(rawJSON)
	for _, want := range []string{
		"4bf92f3577b34da6a3ce929d0e0e4736",
		cid.String(),
		"meterly.correlation_id",
		"provider.mockvend.validate_meter",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("OTLP payload missing %q: %s", want, s)
		}
	}
}
