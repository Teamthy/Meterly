package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/app"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/platform/config"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type otlpCapture struct {
	mu    sync.Mutex
	spans []capturedSpan
}

type capturedSpan struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Kind         int
	Attrs        map[string]string
}

func newOTLPCollector(t *testing.T) (*httptest.Server, *otlpCapture) {
	t.Helper()
	cap := &otlpCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		var payload struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID      string `json:"traceId"`
						SpanID       string `json:"spanId"`
						ParentSpanID string `json:"parentSpanId"`
						Name         string `json:"name"`
						Kind         int    `json:"kind"`
						Attributes   []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(raw, &payload); err == nil {
			cap.mu.Lock()
			for _, rs := range payload.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						attrs := map[string]string{}
						for _, a := range sp.Attributes {
							attrs[a.Key] = a.Value.StringValue
						}
						cap.spans = append(cap.spans, capturedSpan{
							TraceID:      sp.TraceID,
							SpanID:       sp.SpanID,
							ParentSpanID: sp.ParentSpanID,
							Name:         sp.Name,
							Kind:         sp.Kind,
							Attrs:        attrs,
						})
					}
				}
			}
			cap.mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func (c *otlpCapture) snapshot() []capturedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedSpan, len(c.spans))
	copy(out, c.spans)
	return out
}

func TestObservabilityMetricsAndOTLPTracing(t *testing.T) {
	pool := pgtest.NewPool(t)
	dburl, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	dburl.Path = "/" + pool.Config().ConnConfig.Database

	collector, spans := newOTLPCollector(t)

	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			"MODE":                        "dev",
			"DATABASE_URL":                dburl.String(),
			"TOKEN_ENC_KEY":               "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
			"METER_HMAC_KEY":              "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
			"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL,
			"OUTBOX_INTERVAL":             "15ms",
			"NOTIFICATION_INTERVAL":       "15ms",
			"RESUME_INTERVAL":             "1h",
			"METRICS_REFRESH_INTERVAL":    "25ms",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}

	pubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubURL := "http://" + pubLn.Addr().String()
	metURL := "http://" + metLn.Addr().String()

	a, err := app.Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- a.ServeListeners(ctx, pubLn, metLn) }()

	// Public API port MUST NOT serve /metrics.
	res, err := http.Get(pubURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("public /metrics must return 404, got %d", res.StatusCode)
	}

	const (
		secretMeter = "44123456789"
		secretSub   = "cust-secret-subject-9911"
		secretPhone = "+2348011112222"
	)

	cid := uuid.New().String()
	doJSON := func(method, path, idemKey string, body any, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, pubURL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secretSub)
		req.Header.Set("X-Correlation-ID", cid)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if out != nil && resp.StatusCode < 300 {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, string(raw))
			}
		}
		return resp.StatusCode
	}

	// 1. Register meter (triggers provider.mockvend.validate_meter).
	var regResp struct {
		ID string `json:"meter_id"`
	}
	if code := doJSON("POST", "/v1/meters", "idem-meter-1", map[string]string{
		"meter_number": secretMeter,
		"disco_code":   "IKEDC",
		"label":        "Home",
	}, &regResp); code != 201 {
		t.Fatalf("register meter status=%d", code)
	}

	// Attach verified SMS contact on internal user so notification delivery runs.
	uid, err := a.Store.Repos().Users().EnsureByExternalID(context.Background(), secretSub)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secure.NewBox(cfg.TokenEncKey, cfg.MeterHMACKey, cfg.KeyVersion)
	if err != nil {
		t.Fatal(err)
	}
	phoneCt, err := box.EncryptBound(secretPhone, []byte("user-phone:"+uid))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Repos().Users().UpsertContacts(context.Background(), notifications.UserContacts{
		UserID:          uid,
		PhoneCiphertext: phoneCt,
		PhoneHMAC:       notifications.DestinationHMAC(box, secretPhone),
		PhoneKeyVersion: cfg.KeyVersion,
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Create quote + transaction + complete payment + payment-check -> COMPLETED.
	var quoteResp struct {
		ID string `json:"quote_id"`
	}
	if code := doJSON("POST", "/v1/electricity/quotes", "", map[string]any{
		"meter_id":            regResp.ID,
		"amount_minor":        200000,
		"currency":            "NGN",
		"delivery_preference": "TOKEN",
	}, &quoteResp); code != 201 {
		t.Fatalf("quote status=%d", code)
	}

	var txnResp struct {
		ID          string `json:"transaction_id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if code := doJSON("POST", "/v1/electricity/transactions", "idem-observability-txn-0001", map[string]any{
		"quote_id": quoteResp.ID,
		"email":    "buyer@example.com",
	}, &txnResp); code != 202 {
		t.Fatalf("create txn status=%d", code)
	}

	payRef := strings.TrimPrefix(txnResp.CheckoutURL, "https://checkout.mock/")
	if code := doJSON("POST", "/v1/dev/payments/"+payRef+"/complete", "", map[string]any{}, nil); code != 200 {
		t.Fatalf("dev complete payment status=%d", code)
	}
	if code := doJSON("POST", "/v1/electricity/transactions/"+txnResp.ID+"/payment-check", "", nil, nil); code != 200 {
		t.Fatalf("payment-check status=%d", code)
	}

	// Wait briefly for outbox + notification delivery + gauge refresh to observe COMPLETED & SMS SENT.
	deadline := time.Now().Add(3 * time.Second)
	var metricsText string
	for time.Now().Before(deadline) {
		a.RefreshMetrics(context.Background())
		mres, err := http.Get(metURL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(mres.Body)
		mres.Body.Close()
		metricsText = string(b)
		if strings.Contains(metricsText, `notification_deliveries_total{channel="SMS",status="SENT"} 2`) &&
			strings.Contains(metricsText, `transactions_by_status{status="COMPLETED"} 1`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Shutdown cleanly so tracer flushes all spans.
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeListeners error: %v", err)
	}

	for _, want := range []string{
		`http_requests_total{route="UNMATCHED",code="404"} 1`,
		`http_requests_total{route="POST /v1/meters",code="201"} 1`,
		`http_requests_total{route="POST /v1/electricity/transactions",code="202"} 1`,
		`http_requests_total{route="POST /v1/electricity/transactions/{transaction_id}/payment-check",code="200"} 1`,
		`http_request_duration_seconds_count{route="POST /v1/electricity/transactions"} 1`,
		`provider_calls_total{provider="mockvend",op="validate_meter",outcome="ok"} 1`,
		`provider_calls_total{provider="mockpay",op="initialize_payment",outcome="ok"} 1`,
		`provider_calls_total{provider="mockpay",op="verify_payment",outcome="ok"} 1`,
		`provider_calls_total{provider="mockvend",op="vend",outcome="ok"} 1`,
		`provider_calls_total{provider="mocksms",op="send_notification",outcome="ok"} 2`,
		`provider_call_duration_seconds_count{provider="mockvend",op="vend"} 1`,
		`transactions_by_status{status="COMPLETED"} 1`,
		`manual_review_open 0`,
		`resume_escalations_total 0`,
		`outbox_lag_seconds `,
		`notification_deliveries_total{channel="SMS",status="SENT"} 2`,
	} {
		if !strings.Contains(metricsText, want) {
			t.Fatalf("missing expected metric %q in:\n%s", want, metricsText)
		}
	}

	// Strict cardinality & PII discipline: never expose meter number, user ID,
	// auth subject, phone number or transaction UUID in Prometheus output.
	for _, forbidden := range []string{secretMeter, secretSub, secretPhone, uid, txnResp.ID, regResp.ID} {
		if forbidden != "" && strings.Contains(metricsText, forbidden) {
			t.Fatalf("sensitive/high-cardinality value %q leaked into /metrics:\n%s", forbidden, metricsText)
		}
	}

	// Verify OTLP spans: HTTP server span + child provider client span with correlation ID.
	captured := spans.snapshot()
	var foundHTTP, foundProvider bool
	for _, sp := range captured {
		for _, val := range sp.Attrs {
			for _, forbidden := range []string{secretMeter, secretPhone} {
				if strings.Contains(val, forbidden) {
					t.Fatalf("PII %q leaked into span attribute: %+v", forbidden, sp)
				}
			}
		}
		if sp.Name == "POST /v1/meters" && sp.Kind == 2 && sp.Attrs["meterly.correlation_id"] == cid {
			foundHTTP = true
			for _, child := range captured {
				if child.Name == "provider.mockvend.validate_meter" &&
					child.Kind == 3 &&
					child.TraceID == sp.TraceID &&
					child.ParentSpanID == sp.SpanID &&
					child.Attrs["meterly.correlation_id"] == cid {
					foundProvider = true
				}
			}
		}
	}
	if !foundHTTP || !foundProvider {
		t.Fatalf("expected linked HTTP and provider OTLP spans with correlation ID %s; got %+v", cid, captured)
	}
}

type closingAwareStore struct {
	store.Store
	closed          atomic.Bool
	opsAfterClose   atomic.Int64
	beforeCloseHook func(store.Store)
}

func (s *closingAwareStore) InTx(ctx context.Context, fn func(store.Repos) error) error {
	if s.closed.Load() {
		s.opsAfterClose.Add(1)
	}
	return s.Store.InTx(ctx, fn)
}

func (s *closingAwareStore) Repos() store.Repos {
	if s.closed.Load() {
		s.opsAfterClose.Add(1)
	}
	return s.Store.Repos()
}

func (s *closingAwareStore) Close() {
	if s.beforeCloseHook != nil {
		s.beforeCloseHook(s.Store)
	}
	s.closed.Store(true)
	s.Store.Close()
}

type blockingPayProvider struct {
	enteredVerify chan struct{}
	enterOnce     sync.Once
	allowFinish   chan struct{}
	paidAmount    money.Amount
}

func (b *blockingPayProvider) Name() string { return "blockpay" }

func (b *blockingPayProvider) InitializePayment(_ context.Context, req providers.PaymentInit) (providers.PaymentInitResult, error) {
	return providers.PaymentInitResult{ProviderRef: "ref-" + req.Reference, CheckoutURL: "https://pay.test/checkout"}, nil
}

func (b *blockingPayProvider) VerifyPayment(ctx context.Context, providerRef string) (providers.PaymentVerification, error) {
	b.enterOnce.Do(func() { close(b.enteredVerify) })
	select {
	case <-b.allowFinish:
	case <-ctx.Done():
		return providers.PaymentVerification{}, ctx.Err()
	}
	now := time.Now().UTC()
	return providers.PaymentVerification{
		ProviderRef: providerRef,
		Status:      providers.PaymentSucceeded,
		Amount:      b.paidAmount,
		PaidAt:      &now,
	}, nil
}

func (b *blockingPayProvider) RefundPayment(_ context.Context, req providers.RefundRequest) (string, error) {
	return "rf-" + req.Reference, nil
}

func (b *blockingPayProvider) ParseWebhook(_ http.Header, _ []byte) (providers.WebhookEvent, error) {
	return providers.WebhookEvent{}, nil
}

func TestGracefulShutdownDrainsHTTPAndInFlightClaims(t *testing.T) {
	pool := pgtest.NewPool(t)
	collector, spans := newOTLPCollector(t)

	cfg, err := config.Load(func(k string) string {
		return map[string]string{
			"MODE":                        "dev",
			"TOKEN_ENC_KEY":               "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
			"METER_HMAC_KEY":              "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
			"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL,
			"RESUME_INTERVAL":             "20ms",
			"OUTBOX_INTERVAL":             "20ms",
			"NOTIFICATION_INTERVAL":       "20ms",
			"PROVIDER_TIMEOUT":            "5s",
			"RESUME_STALE_AFTER":          "6s",
			"SHUTDOWN_TIMEOUT":            "10s",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}

	var txnID uuid.UUID
	var statusAtClose tx.Overall

	pgSt := pgstore.New(pool, "SANDBOX")
	trackedStore := &closingAwareStore{
		Store: pgSt,
		beforeCloseHook: func(inner store.Store) {
			if txnID != uuid.Nil {
				if got, err := inner.Repos().Transactions().Get(context.Background(), txnID); err == nil {
					statusAtClose = got.Status()
				}
			}
		},
	}

	blockPay := &blockingPayProvider{
		enteredVerify: make(chan struct{}),
		allowFinish:   make(chan struct{}),
	}

	httpEntered := make(chan struct{})
	allowHTTPFinish := make(chan struct{})
	var slowOnce sync.Once

	a, err := app.BuildWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.BuildOptions{
		Store:           trackedStore,
		PaymentProvider: blockPay,
		WrapPublicHTTP: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Slow-Request") == "1" {
					slowOnce.Do(func() { close(httpEntered) })
					<-allowHTTPFinish
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a pending transaction whose UpdatedAt is older than ResumeStaleAfter
	// so the resume worker claims it immediately on the next tick.
	uid, err := a.Store.Repos().Users().EnsureByExternalID(context.Background(), "shutdown-cust")
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.Orch.Meters.Register(context.Background(), uid, "44987654321", "IKEDC", "Office")
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.Orch.CreateQuote(context.Background(), uid, m.ID, 150000, orchestrator.PrefToken)
	if err != nil {
		t.Fatal(err)
	}
	blockPay.paidAmount = q.Total
	created, _, err := a.Orch.CreateTransaction(context.Background(), orchestrator.CreateRequest{
		CustomerID:     uid,
		QuoteID:        q.ID,
		IdempotencyKey: "idem-shutdown-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	txnID = created.ID

	staleTime := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := pool.Exec(context.Background(),
		`UPDATE electricity_transactions SET next_check_at = $2, updated_at = $2 WHERE id = $1`,
		txnID, staleTime); err != nil {
		t.Fatal(err)
	}

	pubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pubURL := "http://" + pubLn.Addr().String()

	runCtx, cancelRun := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- a.ServeListeners(runCtx, pubLn, metLn) }()

	// Start an in-flight HTTP request.
	type httpResult struct {
		code int
		err  error
	}
	httpResCh := make(chan httpResult, 1)
	go func() {
		req, _ := http.NewRequest("GET", pubURL+"/healthz", nil)
		req.Header.Set("X-Slow-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			httpResCh <- httpResult{err: err}
			return
		}
		resp.Body.Close()
		httpResCh <- httpResult{code: resp.StatusCode}
	}()

	// Wait until BOTH the HTTP request and the worker's transaction claim are mid-flight.
	select {
	case <-httpEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for in-flight HTTP request")
	}
	select {
	case <-blockPay.enteredVerify:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker to claim due transaction and enter VerifyPayment")
	}

	// Trigger graceful shutdown while both HTTP and worker claim are in flight!
	cancelRun()

	// Wait briefly until public listener stops accepting brand-new TCP connections.
	deadline := time.Now().Add(2 * time.Second)
	rejectedNewConn := false
	for time.Now().Before(deadline) {
		freshClient := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 200 * time.Millisecond}
		if resp, err := freshClient.Get(pubURL + "/healthz"); err != nil {
			rejectedNewConn = true
			break
		} else {
			resp.Body.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !rejectedNewConn {
		t.Fatal("public HTTP server kept accepting new connections after shutdown started")
	}

	// Store MUST NOT be closed while HTTP or worker claim are still draining.
	if trackedStore.closed.Load() {
		t.Fatal("store closed before in-flight HTTP request and worker claim finished")
	}

	// Allow the in-flight HTTP request to finish; it must return 200 OK.
	close(allowHTTPFinish)
	select {
	case hr := <-httpResCh:
		if hr.err != nil || hr.code != 200 {
			t.Fatalf("in-flight HTTP request failed during drain: code=%d err=%v", hr.code, hr.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight HTTP request did not complete")
	}

	// Worker claim is STILL blocked in VerifyPayment, so Store MUST STILL be open!
	time.Sleep(30 * time.Millisecond)
	if trackedStore.closed.Load() {
		t.Fatal("store closed while in-flight worker claim was still running")
	}

	// Release the in-flight provider VerifyPayment call so the worker finishes
	// payment confirmation -> vending -> token delivery -> DB commit -> shutdown!
	close(blockPay.allowFinish)

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeListeners returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListeners did not complete graceful shutdown")
	}

	if !trackedStore.closed.Load() {
		t.Fatal("expected store to be closed at the end of graceful shutdown")
	}
	if n := trackedStore.opsAfterClose.Load(); n != 0 {
		t.Fatalf("detected %d store operation(s) after Store.Close()", n)
	}
	if statusAtClose != tx.OverallCompleted {
		t.Fatalf("in-flight worker claim did not finish committing before Store.Close(): status=%s, want COMPLETED", statusAtClose)
	}

	// Verify spans from both the drained HTTP request and the drained worker
	// claim were flushed to OTLP before Tracer.Shutdown returned.
	captured := spans.snapshot()
	var sawHTTPSpan, sawWorkerVerifySpan, sawWorkerVendSpan bool
	for _, sp := range captured {
		if sp.Name == "GET /healthz" {
			sawHTTPSpan = true
		}
		if sp.Name == "provider.blockpay.verify_payment" && sp.Attrs["meterly.correlation_id"] != "" {
			sawWorkerVerifySpan = true
		}
		if sp.Name == "provider.mockvend.vend" && sp.Attrs["meterly.correlation_id"] != "" {
			sawWorkerVendSpan = true
		}
	}
	if !sawHTTPSpan || !sawWorkerVerifySpan || !sawWorkerVendSpan {
		t.Fatalf("missing flushed spans after graceful shutdown (http=%v verify=%v vend=%v): %+v",
			sawHTTPSpan, sawWorkerVerifySpan, sawWorkerVendSpan, captured)
	}
}
