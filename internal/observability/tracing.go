package observability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teamthy/meterly/internal/audit"
)

type SpanKind int

const (
	SpanKindInternal SpanKind = 1
	SpanKindServer   SpanKind = 2
	SpanKindClient   SpanKind = 3
)

const (
	statusCodeOK    = 1
	statusCodeError = 2
)

type Attr struct {
	Key   string
	Value string
}

func StringAttr(k, v string) Attr { return Attr{Key: k, Value: v} }
func IntAttr(k string, v int) Attr {
	return Attr{Key: k, Value: strconv.Itoa(v)}
}

type spanCtxKey struct{}

type SpanContext struct {
	TraceID string // 32 lowercase hex chars (16 bytes)
	SpanID  string // 16 lowercase hex chars (8 bytes)
}

func SpanContextFrom(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(spanCtxKey{}).(SpanContext)
	return sc, ok && sc.TraceID != "" && sc.SpanID != ""
}

type TracerConfig struct {
	ServiceName  string
	OTLPEndpoint string // empty = no-op tracer
	HTTPClient   *http.Client
	Log          *slog.Logger
}

// Tracer creates W3C TraceContext spans and exports them over OTLP/HTTP JSON
// when OTEL_EXPORTER_OTLP_ENDPOINT is configured. When unconfigured it is a
// zero-overhead no-op.
type Tracer struct {
	enabled     bool
	serviceName string
	tracesURL   string
	http        *http.Client
	log         *slog.Logger

	ch       chan otlpSpan
	flushReq chan chan struct{}
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewTracer(cfg TracerConfig) *Tracer {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	svc := strings.TrimSpace(cfg.ServiceName)
	if svc == "" {
		svc = "meterly"
	}
	ep := strings.TrimSpace(cfg.OTLPEndpoint)
	if ep == "" {
		return &Tracer{serviceName: svc, log: log}
	}
	tracesURL := normalizeTracesURL(ep)
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	t := &Tracer{
		enabled:     true,
		serviceName: svc,
		tracesURL:   tracesURL,
		http:        hc,
		log:         log,
		ch:          make(chan otlpSpan, 2048),
		flushReq:    make(chan chan struct{}),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	go t.runExporter()
	return t
}

func normalizeTracesURL(ep string) string {
	trimmed := strings.TrimRight(ep, "/")
	if strings.HasSuffix(trimmed, "/v1/traces") {
		return trimmed
	}
	return trimmed + "/v1/traces"
}

func (t *Tracer) Enabled() bool { return t != nil && t.enabled }

// ExtractHTTP parses an inbound W3C traceparent header if valid and attaches
// it as the parent SpanContext on ctx.
func (t *Tracer) ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	if !t.Enabled() {
		return ctx
	}
	tp := strings.TrimSpace(h.Get("Traceparent"))
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return ctx
	}
	if !isNonZeroHex(parts[1]) || !isNonZeroHex(parts[2]) {
		return ctx
	}
	return context.WithValue(ctx, spanCtxKey{}, SpanContext{
		TraceID: strings.ToLower(parts[1]),
		SpanID:  strings.ToLower(parts[2]),
	})
}

// InjectHTTP writes the active SpanContext into h as a W3C traceparent header.
func InjectHTTP(ctx context.Context, h http.Header) {
	sc, ok := SpanContextFrom(ctx)
	if !ok || h == nil {
		return
	}
	h.Set("Traceparent", "00-"+sc.TraceID+"-"+sc.SpanID+"-01")
}

func isNonZeroHex(s string) bool {
	b, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

type Span struct {
	tracer    *Tracer
	traceID   string
	spanID    string
	parentID  string
	name      string
	kind      SpanKind
	startNano int64
	mu        sync.Mutex
	attrs     []Attr
	ended     bool
}

func (t *Tracer) StartSpan(ctx context.Context, name string, kind SpanKind, attrs ...Attr) (context.Context, *Span) {
	if !t.Enabled() {
		return ctx, nil
	}
	parent, hasParent := SpanContextFrom(ctx)
	traceID := parent.TraceID
	if !hasParent {
		traceID = randomHex(16)
	}
	spanID := randomHex(8)

	var allAttrs []Attr
	if cid := audit.CorrelationFrom(ctx); cid != nil {
		allAttrs = append(allAttrs, StringAttr("meterly.correlation_id", cid.String()))
	}
	allAttrs = append(allAttrs, attrs...)

	sp := &Span{
		tracer:    t,
		traceID:   traceID,
		spanID:    spanID,
		parentID:  parent.SpanID,
		name:      name,
		kind:      kind,
		startNano: time.Now().UTC().UnixNano(),
		attrs:     allAttrs,
	}
	nextCtx := context.WithValue(ctx, spanCtxKey{}, SpanContext{TraceID: traceID, SpanID: spanID})
	return nextCtx, sp
}

func (s *Span) SetName(name string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.name = name
	}
}

func (s *Span) SetAttr(a Attr) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.attrs = append(s.attrs, a)
	}
}

func (s *Span) End(isError bool) {
	if s == nil || s.tracer == nil || !s.tracer.Enabled() {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	attrs := make([]otlpKeyValue, 0, len(s.attrs))
	for _, a := range s.attrs {
		attrs = append(attrs, otlpKeyValue{
			Key:   a.Key,
			Value: otlpAnyValue{StringValue: a.Value},
		})
	}
	s.mu.Unlock()

	code := statusCodeOK
	if isError {
		code = statusCodeError
	}
	endNano := time.Now().UTC().UnixNano()
	if endNano < s.startNano {
		endNano = s.startNano
	}
	out := otlpSpan{
		TraceID:           s.traceID,
		SpanID:            s.spanID,
		ParentSpanID:      s.parentID,
		Name:              s.name,
		Kind:              int(s.kind),
		StartTimeUnixNano: strconv.FormatInt(s.startNano, 10),
		EndTimeUnixNano:   strconv.FormatInt(endNano, 10),
		Attributes:        attrs,
		Status:            otlpStatus{Code: code},
	}
	select {
	case s.tracer.ch <- out:
	default:
		// Drop rather than blocking payment/vending request handling.
	}
}

func (t *Tracer) Flush(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	ack := make(chan struct{})
	select {
	case t.flushReq <- ack:
	case <-t.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-t.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Tracer) Shutdown(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	t.stopOnce.Do(func() { close(t.stopCh) })
	select {
	case <-t.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type otlpExportRequest struct {
	ResourceSpans []otlpResourceSpan `json:"resourceSpans"`
}

type otlpResourceSpan struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpan struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue string `json:"stringValue"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

func (t *Tracer) runExporter() {
	defer close(t.doneCh)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	batch := make([]otlpSpan, 0, 64)
	sendBatch := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		spans := make([]otlpSpan, len(batch))
		copy(spans, batch)
		batch = batch[:0]
		t.postSpans(ctx, spans)
	}
	drainChan := func() {
		for {
			select {
			case sp := <-t.ch:
				batch = append(batch, sp)
				if len(batch) >= 64 {
					sendBatch(context.Background())
				}
			default:
				return
			}
		}
	}

	for {
		select {
		case sp := <-t.ch:
			batch = append(batch, sp)
			if len(batch) >= 64 {
				sendBatch(context.Background())
			}
		case <-ticker.C:
			sendBatch(context.Background())
		case ack := <-t.flushReq:
			drainChan()
			sendBatch(context.Background())
			close(ack)
		case <-t.stopCh:
			drainChan()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			sendBatch(ctx)
			cancel()
			return
		}
	}
}

func (t *Tracer) postSpans(ctx context.Context, spans []otlpSpan) {
	payload := otlpExportRequest{
		ResourceSpans: []otlpResourceSpan{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{{
					Key:   "service.name",
					Value: otlpAnyValue{StringValue: t.serviceName},
				}},
			},
			ScopeSpans: []otlpScopeSpan{{
				Scope: otlpScope{Name: "github.com/teamthy/meterly"},
				Spans: spans,
			}},
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tracesURL, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := t.http.Do(req)
	if err != nil {
		t.log.Warn("otlp trace export failed", "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
}

func randomHex(numBytes int) string {
	buf := make([]byte, numBytes)
	_, _ = rand.Read(buf)
	allZero := true
	for _, b := range buf {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		buf[0] = 1
	}
	return hex.EncodeToString(buf)
}
