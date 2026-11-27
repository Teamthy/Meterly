// Package observability provides Prometheus metrics exposition and
// OpenTelemetry W3C TraceContext + OTLP/HTTP JSON span export using only the
// Go standard library.
//
// Label discipline: all metric helpers accept strictly typed low-cardinality
// dimensions. Meter numbers, tokens, phone numbers, email addresses, user IDs
// and transaction IDs are never accepted as metric labels or span attributes.
package observability

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Outcome string

const (
	OutcomeOK         Outcome = "ok"
	OutcomeAmbiguous  Outcome = "ambiguous"
	OutcomeDefinitive Outcome = "definitive"
)

var (
	httpDurationBuckets     = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	providerDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20}
	knownTxnStatuses        = []string{
		"PAYMENT_PENDING",
		"FAILED",
		"VENDING_PENDING",
		"DELIVERY_PENDING",
		"RECONCILIATION_PENDING",
		"MANUAL_REVIEW",
		"REFUND_PENDING",
		"REFUNDED",
		"COMPLETED",
	}
)

type histSeries struct {
	buckets []uint64 // non-cumulative counts per upper bound; cumulative computed at render
	count   uint64
	sum     float64
}

func newHistSeries(n int) *histSeries {
	return &histSeries{buckets: make([]uint64, n)}
}

func (h *histSeries) observe(bounds []float64, v float64) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		v = 0
	}
	for i, b := range bounds {
		if v <= b {
			h.buckets[i]++
			break
		}
	}
	h.count++
	h.sum += v
}

type httpReqKey struct {
	route string
	code  string
}

type provCallKey struct {
	provider string
	op       string
	outcome  string
}

type provDurKey struct {
	provider string
	op       string
}

type notifDelivKey struct {
	channel string
	status  string
}

// Registry collects Meterly's operational metrics and renders them in
// Prometheus text exposition format 0.0.4.
type Registry struct {
	mu sync.Mutex

	httpRequests map[httpReqKey]uint64
	httpDuration map[string]*histSeries

	providerCalls    map[provCallKey]uint64
	providerDuration map[provDurKey]*histSeries

	txnsByStatus     map[string]int64
	manualReviewOpen int64
	outboxLagSeconds float64

	notifDeliveries map[notifDelivKey]uint64

	resumeEscalations atomic.Uint64
}

func NewRegistry() *Registry {
	byStatus := make(map[string]int64, len(knownTxnStatuses))
	for _, s := range knownTxnStatuses {
		byStatus[s] = 0
	}
	return &Registry{
		httpRequests:     map[httpReqKey]uint64{},
		httpDuration:     map[string]*histSeries{},
		providerCalls:    map[provCallKey]uint64{},
		providerDuration: map[provDurKey]*histSeries{},
		txnsByStatus:     byStatus,
		notifDeliveries:  map[notifDelivKey]uint64{},
	}
}

// SafeRoute normalizes a Go 1.22 ServeMux pattern into a bounded route label.
// Unmatched routes collapse to "UNMATCHED" so attacker-chosen paths never
// enter Prometheus labels.
func SafeRoute(pattern string) string {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "UNMATCHED"
	}
	return p
}

func (r *Registry) ObserveHTTPRequest(routePattern string, statusCode int, d time.Duration) {
	if r == nil {
		return
	}
	route := SafeRoute(routePattern)
	code := strconv.Itoa(statusCode)
	sec := d.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpRequests[httpReqKey{route: route, code: code}]++
	hs, ok := r.httpDuration[route]
	if !ok {
		hs = newHistSeries(len(httpDurationBuckets))
		r.httpDuration[route] = hs
	}
	hs.observe(httpDurationBuckets, sec)
}

func (r *Registry) ObserveProviderCall(provider, op string, outcome Outcome, d time.Duration) {
	if r == nil {
		return
	}
	switch outcome {
	case OutcomeOK, OutcomeAmbiguous, OutcomeDefinitive:
	default:
		outcome = OutcomeDefinitive
	}
	sec := d.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerCalls[provCallKey{provider: provider, op: op, outcome: string(outcome)}]++
	dk := provDurKey{provider: provider, op: op}
	hs, ok := r.providerDuration[dk]
	if !ok {
		hs = newHistSeries(len(providerDurationBuckets))
		r.providerDuration[dk] = hs
	}
	hs.observe(providerDurationBuckets, sec)
}

func (r *Registry) SetTransactionGauges(byStatus map[string]int64, manualReviewOpen int64, outboxLagSeconds float64) {
	if r == nil {
		return
	}
	if outboxLagSeconds < 0 || math.IsNaN(outboxLagSeconds) || math.IsInf(outboxLagSeconds, 0) {
		outboxLagSeconds = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]int64, len(knownTxnStatuses))
	for _, s := range knownTxnStatuses {
		next[s] = 0
	}
	for k, v := range byStatus {
		next[k] = v
	}
	r.txnsByStatus = next
	r.manualReviewOpen = manualReviewOpen
	r.outboxLagSeconds = outboxLagSeconds
}

func (r *Registry) IncResumeEscalations() {
	if r == nil {
		return
	}
	r.resumeEscalations.Add(1)
}

func (r *Registry) IncNotificationDelivery(channel, status string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifDeliveries[notifDelivKey{channel: channel, status: status}]++
}

// Handler serves Prometheus metrics at GET /metrics only.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/metrics" {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodHead {
			return
		}
		_, _ = io.WriteString(w, r.Render())
	})
}

func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder

	// 1. http_requests_total
	b.WriteString("# HELP http_requests_total Total HTTP requests handled by route pattern and status code.\n")
	b.WriteString("# TYPE http_requests_total counter\n")
	httpKeys := make([]httpReqKey, 0, len(r.httpRequests))
	for k := range r.httpRequests {
		httpKeys = append(httpKeys, k)
	}
	sort.Slice(httpKeys, func(i, j int) bool {
		if httpKeys[i].route != httpKeys[j].route {
			return httpKeys[i].route < httpKeys[j].route
		}
		return httpKeys[i].code < httpKeys[j].code
	})
	for _, k := range httpKeys {
		fmt.Fprintf(&b, "http_requests_total{route=%q,code=%q} %d\n", k.route, k.code, r.httpRequests[k])
	}

	// 2. http_request_duration_seconds
	b.WriteString("# HELP http_request_duration_seconds HTTP request latency in seconds by route pattern.\n")
	b.WriteString("# TYPE http_request_duration_seconds histogram\n")
	httpRoutes := make([]string, 0, len(r.httpDuration))
	for route := range r.httpDuration {
		httpRoutes = append(httpRoutes, route)
	}
	sort.Strings(httpRoutes)
	for _, route := range httpRoutes {
		writeHistogram(&b, "http_request_duration_seconds", fmt.Sprintf("route=%q", route), httpDurationBuckets, r.httpDuration[route])
	}

	// 3. provider_calls_total
	b.WriteString("# HELP provider_calls_total External provider calls by provider, operation and outcome.\n")
	b.WriteString("# TYPE provider_calls_total counter\n")
	pKeys := make([]provCallKey, 0, len(r.providerCalls))
	for k := range r.providerCalls {
		pKeys = append(pKeys, k)
	}
	sort.Slice(pKeys, func(i, j int) bool {
		if pKeys[i].provider != pKeys[j].provider {
			return pKeys[i].provider < pKeys[j].provider
		}
		if pKeys[i].op != pKeys[j].op {
			return pKeys[i].op < pKeys[j].op
		}
		return pKeys[i].outcome < pKeys[j].outcome
	})
	for _, k := range pKeys {
		fmt.Fprintf(&b, "provider_calls_total{provider=%q,op=%q,outcome=%q} %d\n", k.provider, k.op, k.outcome, r.providerCalls[k])
	}

	// 4. provider_call_duration_seconds
	b.WriteString("# HELP provider_call_duration_seconds External provider call latency in seconds.\n")
	b.WriteString("# TYPE provider_call_duration_seconds histogram\n")
	dKeys := make([]provDurKey, 0, len(r.providerDuration))
	for k := range r.providerDuration {
		dKeys = append(dKeys, k)
	}
	sort.Slice(dKeys, func(i, j int) bool {
		if dKeys[i].provider != dKeys[j].provider {
			return dKeys[i].provider < dKeys[j].provider
		}
		return dKeys[i].op < dKeys[j].op
	})
	for _, k := range dKeys {
		writeHistogram(&b, "provider_call_duration_seconds", fmt.Sprintf("provider=%q,op=%q", k.provider, k.op), providerDurationBuckets, r.providerDuration[k])
	}

	// 5. transactions_by_status
	b.WriteString("# HELP transactions_by_status Current electricity transactions grouped by derived status.\n")
	b.WriteString("# TYPE transactions_by_status gauge\n")
	statuses := make([]string, 0, len(r.txnsByStatus))
	for s := range r.txnsByStatus {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		fmt.Fprintf(&b, "transactions_by_status{status=%q} %d\n", s, r.txnsByStatus[s])
	}

	// 6. manual_review_open
	b.WriteString("# HELP manual_review_open Number of electricity transactions currently awaiting manual review.\n")
	b.WriteString("# TYPE manual_review_open gauge\n")
	fmt.Fprintf(&b, "manual_review_open %d\n", r.manualReviewOpen)

	// 7. resume_escalations_total
	b.WriteString("# HELP resume_escalations_total Total transactions escalated to manual review.\n")
	b.WriteString("# TYPE resume_escalations_total counter\n")
	fmt.Fprintf(&b, "resume_escalations_total %d\n", r.resumeEscalations.Load())

	// 8. outbox_lag_seconds
	b.WriteString("# HELP outbox_lag_seconds Age in seconds of the oldest unpublished outbox event.\n")
	b.WriteString("# TYPE outbox_lag_seconds gauge\n")
	fmt.Fprintf(&b, "outbox_lag_seconds %s\n", strconv.FormatFloat(r.outboxLagSeconds, 'f', -1, 64))

	// 9. notification_deliveries_total
	b.WriteString("# HELP notification_deliveries_total Notification delivery transitions by channel and status.\n")
	b.WriteString("# TYPE notification_deliveries_total counter\n")
	nKeys := make([]notifDelivKey, 0, len(r.notifDeliveries))
	for k := range r.notifDeliveries {
		nKeys = append(nKeys, k)
	}
	sort.Slice(nKeys, func(i, j int) bool {
		if nKeys[i].channel != nKeys[j].channel {
			return nKeys[i].channel < nKeys[j].channel
		}
		return nKeys[i].status < nKeys[j].status
	})
	for _, k := range nKeys {
		fmt.Fprintf(&b, "notification_deliveries_total{channel=%q,status=%q} %d\n", k.channel, k.status, r.notifDeliveries[k])
	}

	return b.String()
}

func writeHistogram(b *strings.Builder, name, baseLabels string, bounds []float64, hs *histSeries) {
	var cum uint64
	for i, ub := range bounds {
		cum += hs.buckets[i]
		fmt.Fprintf(b, "%s_bucket{%s,le=%q} %d\n", name, baseLabels, strconv.FormatFloat(ub, 'f', -1, 64), cum)
	}
	fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, baseLabels, hs.count)
	fmt.Fprintf(b, "%s_sum{%s} %s\n", name, baseLabels, strconv.FormatFloat(hs.sum, 'f', -1, 64))
	fmt.Fprintf(b, "%s_count{%s} %d\n", name, baseLabels, hs.count)
}
