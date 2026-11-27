// Package httpapi exposes the /v1 REST API (spec §7). Handlers only parse,
// authorise and map errors; all business rules live in the orchestrator.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/offline"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/ratelimit"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Limits configures rate limiting. Zero values use the defaults.
type Limits struct {
	WritesPerMinute       int // per user: meter validation/registration, quotes, purchases
	AuthFailuresPerMinute int // per client IP: failed authentications
	WebhooksPerMinute     int // per client IP
	PublicPayPerMinute    int // per client IP: public Pay-for-Me views, checkouts and status polls
	PublicLinkPerMinute   int // per link code: public Pay-for-Me checkout attempts
	// ClientIPHeader, when set (e.g. "X-Forwarded-For"), takes the client IP
	// from the right-most value of that header, which the trusted proxy
	// appended. Leave empty when not behind a proxy.
	ClientIPHeader string
}

type Server struct {
	O             *orchestrator.Orchestrator
	Reconcile     *reconcile.Engine
	Auth          auth.Authenticator
	Identity      *identity.Engine
	Security      *security.Engine
	Payments      providers.PaymentProvider
	Remote        providers.RemoteCreditProvider
	Notifications *notificationservice.Service
	Offline       *offline.Service
	Metrics       *observability.Registry
	Tracer        *observability.Tracer
	Log           *slog.Logger
	Ready         func(context.Context) error
	Limits        Limits
	// NewLimiter, when set, builds shared (e.g. Redis) buckets. Nil = in-process.
	NewLimiter         func(name string, perMinute, burst int) ratelimit.Limiter
	PayForMeDisabled   bool
	SplitMeterDisabled bool
	AutoPowerDisabled  bool
	PowerDeskDisabled  bool
	USSDDisabled       bool
	WhatsAppDisabled   bool
	SupportDisabled    bool
	HouseholdsDisabled bool
	PublicBaseURL      string
	// Dev enables /v1/dev/* simulation endpoints. Never enable in production.
	Dev         bool
	DevPay      *mock.Payments
	DevRemote   *mock.RemoteCredit
	DevReadings *mock.MeterReadings

	writes, authFails, hooks, publicPay, publicLink ratelimit.Limiter
	patterns                                        []string
}

type patternMux struct {
	*http.ServeMux
	patterns []string
}

func (m *patternMux) Handle(pattern string, handler http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, handler)
}

func (m *patternMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, handler)
}

// RegisteredPatterns returns a copy of the exact patterns registered on the
// underlying ServeMux during the most recent Routes() call.
func (s *Server) RegisteredPatterns() []string {
	return append([]string(nil), s.patterns...)
}

type caller struct {
	UserID    string // internal user UUID
	Principal auth.Principal
}

type ctxKey struct{}

func callerOf(r *http.Request) caller { return r.Context().Value(ctxKey{}).(caller) }

// userID is the internal user ID (UUID) of the authenticated caller.
func userID(r *http.Request) string { return callerOf(r).UserID }

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func (s *Server) Routes() http.Handler {
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	newLim := s.NewLimiter
	if newLim == nil {
		newLim = func(_ string, per, burst int) ratelimit.Limiter { return ratelimit.New(per, burst) }
	}
	s.writes = newLim("writes", orDefault(s.Limits.WritesPerMinute, 30), 10)
	s.authFails = newLim("auth", orDefault(s.Limits.AuthFailuresPerMinute, 20), 10)
	s.hooks = newLim("webhooks", orDefault(s.Limits.WebhooksPerMinute, 600), 100)
	pubPayRate := orDefault(s.Limits.PublicPayPerMinute, 60)
	pubLinkRate := orDefault(s.Limits.PublicLinkPerMinute, 10)
	s.publicPay = newLim("public-pay", pubPayRate, min(pubPayRate, max(20, pubPayRate/3)))
	s.publicLink = newLim("public-link", pubLinkRate, min(pubLinkRate, max(5, pubLinkRate/2)))

	mux := &patternMux{ServeMux: http.NewServeMux()}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.readyz)

	user, write := s.authed(auth.RoleCustomer, false), s.authed(auth.RoleCustomer, true)
	s.identityRoutes(mux, user, write)
	s.securityRoutes(mux, user, write)
	mux.Handle("POST /v1/meters/validate", write(s.validateMeter))
	mux.Handle("POST /v1/meters", write(s.registerMeter))
	mux.Handle("GET /v1/meters", user(s.listMeters))
	mux.Handle("GET /v1/meters/{meter_id}", user(s.getMeter))
	mux.Handle("DELETE /v1/meters/{meter_id}", write(s.removeSavedMeter))
	mux.Handle("GET /v1/meters/{meter_id}/capabilities", user(s.getCapabilities))
	mux.Handle("GET /v1/meters/{meter_id}/reading", user(s.getMeterReading))
	mux.Handle("GET /v1/meters/{meter_id}/usage", user(s.getMeterUsage))
	mux.Handle("GET /v1/meters/usage", user(s.listMeterUsage))
	mux.Handle("GET /v1/meters/{meter_id}/buy-options", user(s.getBuyOptions))
	mux.Handle("GET /v1/electricity/catalog", user(s.getCatalog))
	mux.Handle("POST /v1/beneficiaries", write(s.addBeneficiary))
	mux.Handle("GET /v1/beneficiaries", user(s.listBeneficiaries))
	s.householdRoutes(mux, user, write)

	mux.Handle("POST /v1/electricity/quotes", write(s.createQuote))
	mux.Handle("POST /v1/electricity/transactions", write(s.createTransaction))
	mux.Handle("GET /v1/electricity/transactions", user(s.listTransactions))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}", user(s.getTransaction))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/status", user(s.getTransaction))
	mux.Handle("POST /v1/electricity/transactions/{transaction_id}/payment-check", write(s.paymentCheck))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/token", user(s.getToken))
	mux.Handle("POST /v1/electricity/transactions/{transaction_id}/token/shares", write(s.recordTokenCopy))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/token/shares", user(s.listTokenCopies))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/receipt", user(s.receipt))

	mux.Handle("GET /v1/me/notification-preferences", user(s.getNotificationPreferences))
	mux.Handle("PUT /v1/me/notification-preferences", write(s.putNotificationPreferences))
	mux.Handle("GET /v1/me/consents", user(s.getConsents))
	mux.Handle("POST /v1/me/consents/withdraw", write(s.withdrawConsent))

	mux.Handle("POST /v1/payment-requests", write(s.createPaymentRequest))
	mux.Handle("GET /v1/payment-requests", user(s.listPaymentRequests))
	mux.Handle("DELETE /v1/payment-requests/{id}", write(s.cancelPaymentRequest))

	mux.HandleFunc("GET /v1/public/pay/{code}", s.publicPayLimited(s.getPublicPayLink))
	mux.HandleFunc("POST /v1/public/pay/{code}/checkout", s.publicPayLimited(s.checkoutPublicPayLink))
	mux.HandleFunc("GET /v1/public/pay/status/{status_code}", s.publicPayLimited(s.getPublicPayStatus))

	mux.Handle("POST /v1/pots", write(s.createPot))
	mux.Handle("GET /v1/pots/{id}", user(s.getPot))
	mux.Handle("POST /v1/pots/{id}/cancel", write(s.cancelPot))
	mux.HandleFunc("GET /v1/public/pots/{code}", s.publicPotLimited(s.getPublicPot))
	mux.HandleFunc("POST /v1/public/pots/{code}/contribute", s.publicPotLimited(s.contributeToPot))

	mux.Handle("GET /v1/me/payment-methods", user(s.listPaymentMethods))
	mux.Handle("DELETE /v1/me/payment-methods/{id}", write(s.deletePaymentMethod))
	mux.Handle("POST /v1/me/payment-methods/{transaction_id}/save", write(s.savePaymentMethod))
	mux.Handle("POST /v1/autopower/rules", write(s.createAutoPowerRule))
	mux.Handle("GET /v1/autopower/rules", user(s.listAutoPowerRules))
	mux.Handle("PATCH /v1/autopower/rules/{id}", write(s.updateAutoPowerRule))
	mux.Handle("DELETE /v1/autopower/rules/{id}", write(s.deleteAutoPowerRule))
	mux.Handle("POST /v1/autopower/runs/{id}/skip", write(s.skipAutoPowerRun))

	s.powerDeskRoutes(mux, user, write)
	s.offlineRoutes(mux, write)
	s.supportRoutes(mux, user, write)

	mux.HandleFunc("POST /v1/payments/webhooks/{provider}", s.limited(s.hooks, s.paymentWebhook))
	mux.HandleFunc("POST /v1/webhooks/remote-credit/{provider}", s.limited(s.hooks, s.remoteCreditWebhook))
	mux.HandleFunc("GET /v1/notifications/inbound/{provider}", s.limited(s.hooks, s.notificationInboundChallenge))
	mux.HandleFunc("POST /v1/notifications/inbound/{provider}", s.limited(s.hooks, s.notificationInbound))

	s.adminRoutes(mux)

	if s.Dev {
		mux.HandleFunc("GET /v1/dev/console", s.devConsole)
		mux.HandleFunc("POST /v1/dev/payments/{ref}/complete", s.devCompletePayment)
		mux.HandleFunc("POST /v1/dev/remote/settle", s.devSettle)
		mux.HandleFunc("POST /v1/dev/meters/{meter_id}/reading", s.devSetReading)
		mux.HandleFunc("POST /v1/dev/reconcile", s.devReconcile)
		mux.HandleFunc("POST /v1/dev/transactions/{transaction_id}/resume", s.devResume)
	}
	s.patterns = append([]string(nil), mux.patterns...)
	return s.withCorrelation(mux.ServeMux)
}

// ---- middleware ------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid, err := uuid.Parse(r.Header.Get("X-Correlation-ID"))
		if err != nil {
			cid = uuid.New()
		}
		w.Header().Set("X-Correlation-ID", cid.String())
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		ctx := audit.WithCorrelation(r.Context(), cid)
		ctx = s.Tracer.ExtractHTTP(ctx, r.Header)
		ctx, sp := s.Tracer.StartSpan(ctx, r.Method+" UNMATCHED", observability.SpanKindServer,
			observability.StringAttr("http.request.method", r.Method),
		)

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r.Body = http.MaxBytesReader(rec, r.Body, 1<<20)
		r2 := r.WithContext(ctx)
		next.ServeHTTP(rec, r2)

		dur := time.Since(start)
		route := observability.SafeRoute(r2.Pattern)
		s.Metrics.ObserveHTTPRequest(route, rec.status, dur)
		if sp != nil {
			if route != "UNMATCHED" {
				sp.SetName(route)
			}
			sp.SetAttr(observability.StringAttr("http.route", route))
			sp.SetAttr(observability.IntAttr("http.response.status_code", rec.status))
			sp.End(rec.status >= 500)
		}
		// Never log bodies: they can contain meter numbers and tokens.
		s.Log.Info("request", "method", r.Method, "path", route, "status", rec.status, "cid", cid, "ms", dur.Milliseconds())
	})
}

// clientIP is the caller's address, from the trusted proxy header if set.
func (s *Server) clientIP(r *http.Request) string {
	if h := s.Limits.ClientIPHeader; h != "" {
		if v := r.Header.Values(h); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeErr(w, 429, "RATE_LIMITED", "too many requests; retry later")
}

func (s *Server) limited(l ratelimit.Limiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.Allow(s.clientIP(r)); !ok {
			tooMany(w, wait)
			return
		}
		h(w, r)
	}
}

// authed authenticates the caller, maps them to an internal user, checks
// the role, and (for writes) applies the per-user rate limit.
func (s *Server) authed(role auth.Role, isWrite bool) func(http.HandlerFunc) http.Handler {
	return func(h http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := s.Auth.Authenticate(r)
			if err != nil {
				if ok, wait := s.authFails.Allow(s.clientIP(r)); !ok {
					tooMany(w, wait)
					return
				}
				writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
				return
			}
			if !p.Has(role) {
				writeErr(w, 403, "FORBIDDEN", "insufficient role")
				return
			}
			uid, err := s.O.Store().Repos().Users().EnsureByExternalID(r.Context(), p.Subject)
			if err != nil {
				s.mapErr(w, r, err)
				return
			}
			if err := s.captureVerifiedContacts(r.Context(), uid, p); err != nil {
				s.mapErr(w, r, err)
				return
			}
			if isWrite {
				if ok, wait := s.writes.Allow(uid); !ok {
					tooMany(w, wait)
					return
				}
			}
			if s.Security != nil && s.Identity != nil {
				if tok := s.bearerToken(r); strings.HasPrefix(tok, identity.SessionPrefix) {
					if sess, err := s.Identity.LookupSession(r.Context(), tok); err == nil {
						_ = s.Security.Store.TouchSession(r.Context(), sess.ID,
							s.Security.HashIP(s.clientIP(r)), s.Security.HashUA(r.UserAgent()), time.Now().UTC())
					}
				}
			}
			h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller{UserID: uid, Principal: p})))
		})
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.Ready(ctx); err != nil {
			s.Log.Warn("not ready", "err", err)
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
	}
	body := map[string]any{"status": "ready", "vending": "ok", "service": "ok"}
	if s.O != nil {
		if s.O.ServicePaused() {
			body["status"] = "degraded"
			body["service"] = "paused"
		}
		h := s.O.VendingHealth(r.Context())
		if h.Paused {
			body["status"] = "degraded"
			body["vending"] = "paused"
		}
	}
	writeJSON(w, 200, body)
}

// ---- meters -------------------------------------------------------------------

type meterReq struct {
	MeterNumber string `json:"meter_number"`
	DiscoCode   string `json:"disco_code"`
	Label       string `json:"label"`
	DisplayName string `json:"display_name"`
}

func (m meterReq) valid() bool { return m.MeterNumber != "" && m.DiscoCode != "" }

func (s *Server) validateMeter(w http.ResponseWriter, r *http.Request) {
	var req meterReq
	if !decode(r, &req) || !req.valid() {
		writeErr(w, 400, "INVALID_REQUEST", "meter_number and disco_code are required")
		return
	}
	m, err := s.O.Meters.Validate(r.Context(), req.MeterNumber, req.DiscoCode)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	// Validation alone grants no access, so return only what's needed to
	// confirm the meter identity.
	writeJSON(w, 200, map[string]any{
		"valid": true, "validation_status": "VERIFIED", "masked_meter_number": m.Masked,
		"disco": m.DiscoCode, "meter_type": m.MeterType, "customer_name": m.CustomerName,
		"capabilities": m.Capabilities,
	})
}

func (s *Server) registerMeter(w http.ResponseWriter, r *http.Request) {
	var req meterReq
	if !decode(r, &req) || !req.valid() {
		writeErr(w, 400, "INVALID_REQUEST", "meter_number and disco_code are required")
		return
	}
	m, err := s.O.Meters.Register(r.Context(), userID(r), req.MeterNumber, req.DiscoCode, req.Label)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, m)
}

// addBeneficiary saves someone else's meter so the user can buy for it.
// It grants PURCHASE only: the purchaser sees their own transactions and
// tokens for that meter, never the owner's.
func (s *Server) addBeneficiary(w http.ResponseWriter, r *http.Request) {
	var req meterReq
	if !decode(r, &req) || !req.valid() || req.DisplayName == "" {
		writeErr(w, 400, "INVALID_REQUEST", "meter_number, disco_code and display_name are required")
		return
	}
	m, err := s.O.Meters.Register(r.Context(), userID(r), req.MeterNumber, req.DiscoCode, req.DisplayName)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"meter_id": m.ID, "display_name": req.DisplayName, "masked_meter_number": m.Masked, "customer_name": m.CustomerName})
}

func (s *Server) listBeneficiaries(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.Meters.ListForUser(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []meters.Listed{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) getMeterReading(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return
	}
	reading, err := s.O.LatestMeterReading(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, reading)
}

func (s *Server) getMeterUsage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return
	}
	usage, err := s.O.MeterUsage(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, usage)
}

func (s *Server) listMeterUsage(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.ListMeterUsage(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []orchestrator.MeterUsage{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) listMeters(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.Meters.ListForUser(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []meters.Listed{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) meterFromPath(w http.ResponseWriter, r *http.Request) (*meters.Meter, bool) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return nil, false
	}
	m, err := s.O.Meters.Authorize(r.Context(), userID(r), id, meters.PermView)
	if err != nil {
		s.mapErr(w, r, err)
		return nil, false
	}
	return m, true
}

// removeSavedMeter removes only the authenticated caller's saved access.
func (s *Server) removeSavedMeter(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return
	}
	if err := s.O.Meters.RemoveSavedMeter(r.Context(), userID(r), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getMeter(w http.ResponseWriter, r *http.Request) {
	if m, ok := s.meterFromPath(w, r); ok {
		writeJSON(w, 200, m)
	}
}

func (s *Server) getCapabilities(w http.ResponseWriter, r *http.Request) {
	if m, ok := s.meterFromPath(w, r); ok {
		writeJSON(w, 200, m.Capabilities)
	}
}

// ---- quotes & transactions ----------------------------------------------------

func (s *Server) createQuote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MeterID             uuid.UUID                       `json:"meter_id"`
		AmountMinor         int64                           `json:"amount_minor"`
		PackID              string                          `json:"pack_id"`
		Currency            string                          `json:"currency"`
		DeliveryPreference  orchestrator.DeliveryPreference `json:"delivery_preference"`
		ConfirmCustomerName bool                            `json:"confirm_customer_name"`
	}
	if !decode(r, &req) || req.MeterID == uuid.Nil || req.Currency != "NGN" {
		writeErr(w, 400, "INVALID_REQUEST", "meter_id, currency=NGN and pack_id or amount_minor are required")
		return
	}
	if req.PackID == "" && req.AmountMinor <= 0 {
		writeErr(w, 400, "INVALID_REQUEST", "pack_id or amount_minor is required")
		return
	}
	q, err := s.O.IssueQuote(r.Context(), orchestrator.QuoteSpec{
		CustomerID: userID(r), MeterID: req.MeterID, AmountMinor: req.AmountMinor, PackID: req.PackID, Pref: req.DeliveryPreference,
		ConfirmCustomerName: req.ConfirmCustomerName,
		RequireNameConfirm:  req.ConfirmCustomerName,
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	m, _ := s.O.Meters.Get(r.Context(), q.MeterID)
	writeJSON(w, 201, quoteView(q, orchestrator.QuoteConfirmation(q, m)))
}

func (s *Server) createTransaction(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, 400, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	var req struct {
		QuoteID         uuid.UUID `json:"quote_id"`
		Email           string    `json:"email"`
		IsGift          bool      `json:"is_gift"`
		RecipientName   string    `json:"recipient_name"`
		RecipientPhone  string    `json:"recipient_phone"`
		NotifyRecipient bool      `json:"notify_recipient"`
	}
	if json.Unmarshal(body, &req) != nil || req.QuoteID == uuid.Nil {
		writeErr(w, 400, "INVALID_REQUEST", "quote_id is required")
		return
	}
	if !validEmail(req.Email) {
		// Payment providers need it for the receipt; ask before creating anything.
		writeErr(w, 400, "INVALID_REQUEST", "a valid email is required")
		return
	}
	requestHash := idempotency.HashRequest(body)
	if req.RecipientPhone != "" {
		requestHash = s.O.HashGiftRequest(body)
	}
	t, _, err := s.O.CreateTransaction(r.Context(), orchestrator.CreateRequest{
		CustomerID: userID(r), QuoteID: req.QuoteID, Email: req.Email, IsGift: req.IsGift, RecipientName: req.RecipientName, RecipientPhone: req.RecipientPhone, NotifyRecipient: req.NotifyRecipient,
		IdempotencyKey: key, RequestHash: requestHash, IP: s.clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil && t == nil {
		s.mapErr(w, r, err)
		return
	}
	if err != nil {
		// Created, but checkout could not be opened yet: the client retries
		// with the same Idempotency-Key.
		s.Log.Warn("payment initialisation failed", "txn", t.ID, "err", err)
		writeErr(w, 502, "PROVIDER_UNAVAILABLE", "payment provider unavailable; retry with the same Idempotency-Key")
		return
	}
	writeJSON(w, 202, s.viewTxn(r.Context(), t))
}

func view(t *tx.Transaction) map[string]any {
	tokenAvailable := t.DeliveryMethod == tx.MethodToken && t.Vending == tx.VendConfirmed && len(t.TokenCiphertext) > 0
	creditOnMeter := t.DeliveryMethod == tx.MethodRemote && t.Delivery == tx.DelConfirmed
	return map[string]any{
		"transaction_id": t.ID, "status": t.Status(),
		"payment_status": t.Payment, "vending_status": t.Vending, "delivery_status": t.Delivery,
		"delivery_method": t.DeliveryMethod, "meter_id": t.MeterID, "is_gift": t.IsGift, "recipient_name": t.RecipientName,
		"amount_minor": t.Amount.Minor, "fee_minor": t.Fee.Minor, "total_minor": t.Total.Minor, "currency": t.Total.Currency,
		"units_kwh": t.UnitsKWh, "failure_code": t.FailureCode, "review_reason": t.ReviewReason, "checkout_url": t.CheckoutURL,
		"token_available": tokenAvailable, "credit_on_meter": creditOnMeter,
		"history": t.History, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
}

func (s *Server) txnFromPath(w http.ResponseWriter, r *http.Request) (*tx.Transaction, bool) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return nil, false
	}
	t, err := s.O.Get(r.Context(), id)
	if err != nil || t.CustomerID != userID(r) {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return nil, false
	}
	return t, true
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.txnFromPath(w, r); ok {
		writeJSON(w, 200, s.viewTxn(r.Context(), t))
	}
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.ListForCustomer(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, t := range list {
		out = append(out, view(t))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

// paymentCheck lets the app ask "I've paid" after returning from checkout.
// It triggers server-side verification; it never trusts the client.
func (s *Server) paymentCheck(w http.ResponseWriter, r *http.Request) {
	t, ok := s.txnFromPath(w, r)
	if !ok {
		return
	}
	if t.PaymentProviderRef == "" {
		writeErr(w, 422, "PAYMENT_UNCONFIRMED", "payment has not been initialised")
		return
	}
	t2, err := s.O.HandlePaymentNotification(r.Context(), t.PaymentProviderRef)
	if t2 != nil {
		t = t2
	}
	if err != nil {
		s.Log.Warn("payment check", "txn", t.ID, "err", err)
	}
	writeJSON(w, 200, view(t))
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAction(w, r, security.ActionTokenRead) {
		return
	}
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	tok, t, err := s.O.Token(r.Context(), userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	m, err := s.O.Meters.Get(r.Context(), t.MeterID)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"transaction_id": t.ID, "token": tok, "units_kwh": t.UnitsKWh, "masked_meter_number": m.Masked,
		"note": "This token works only on the meter it was bought for. Sharing it does not move credit to another meter.",
	})
}

// ---- webhooks -------------------------------------------------------------------

func (s *Server) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("provider") != s.Payments.Name() {
		writeErr(w, 404, "INVALID_REQUEST", "unknown provider")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	evt, err := s.Payments.ParseWebhook(r.Header, body)
	if errors.Is(err, providers.ErrBadSignature) {
		writeErr(w, 401, "UNAUTHENTICATED", "invalid webhook signature")
		return
	}
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "malformed webhook")
		return
	}
	if strings.HasPrefix(evt.Type, "charge.dispute") {
		if err := s.O.HandlePaymentDispute(r.Context(), disputeNotice(evt)); err != nil {
			s.Log.Warn("dispute webhook", "err", err)
		}
		writeJSON(w, 200, map[string]string{"status": "received"})
		return
	}
	if !evt.Relevant {
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	// Acknowledge regardless of the downstream outcome; verification and
	// the resume worker own the result.
	if _, err := s.O.HandlePaymentNotification(r.Context(), evt.Reference); err != nil {
		s.Log.Warn("webhook processing", "err", err)
	}
	writeJSON(w, 200, map[string]string{"status": "received"})
}

// ---- dev simulation -------------------------------------------------------------

func (s *Server) devCompletePayment(w http.ResponseWriter, r *http.Request) {
	if s.DevPay == nil {
		writeErr(w, 404, "INVALID_REQUEST", "mock payments not in use")
		return
	}
	var req struct {
		Success *bool `json:"success"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ok := req.Success == nil || *req.Success
	ref := r.PathValue("ref")
	if err := s.DevPay.Complete(ref, ok, 0); err != nil {
		writeErr(w, 404, "INVALID_REQUEST", err.Error())
		return
	}
	if _, err := s.O.HandlePaymentNotification(r.Context(), ref); err != nil {
		s.Log.Warn("dev payment complete", "err", err)
	}
	writeJSON(w, 200, map[string]any{"completed": true, "success": ok})
}

func (s *Server) devSettle(w http.ResponseWriter, r *http.Request) {
	if s.DevRemote == nil {
		writeErr(w, 404, "INVALID_REQUEST", "mock remote credit not in use")
		return
	}
	s.DevRemote.Settle()
	processed, progressed, err := s.O.ResumeDue(r.Context())
	if err != nil {
		s.Log.Warn("dev remote settle", "err", err)
	}
	writeJSON(w, 200, map[string]any{"settled": true, "processed": processed, "progressed": progressed})
}

func (s *Server) devSetReading(w http.ResponseWriter, r *http.Request) {
	if s.DevReadings == nil {
		writeErr(w, 404, "INVALID_REQUEST", "mock meter readings not in use")
		return
	}
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
		return
	}
	var body struct {
		KWh float64 `json:"kwh"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	m, err := s.O.Store().Repos().Meters().Get(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	number, err := s.O.Box().Decrypt(m.NumberEncrypted)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	s.DevReadings.SetReading(number, body.KWh, time.Now().UTC())
	writeJSON(w, 200, map[string]any{"set": true, "kwh": body.KWh})
}

func (s *Server) devReconcile(w http.ResponseWriter, r *http.Request) {
	processed, progressed, err := s.O.ResumeDue(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"processed": processed, "progressed": progressed})
}

func (s *Server) devResume(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	t, err := s.O.Resume(r.Context(), id)
	if err != nil && t == nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, view(t))
}

// ---- helpers ----------------------------------------------------------------------

func validEmail(e string) bool {
	if len(e) < 3 || len(e) > 254 || strings.ContainsAny(e, " \t\r\n") {
		return false
	}
	at := strings.LastIndex(e, "@")
	return at > 0 && at < len(e)-1 && strings.Contains(e[at+1:], ".")
}

func decode(r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"code": errCode, "message": msg, "request_id": w.Header().Get("X-Correlation-ID"), "details": map[string]any{},
	}})
}

func (s *Server) mapErr(w http.ResponseWriter, r *http.Request, err error) {
	if code, c, msg, ok := mapHouseholdErr(err); ok {
		writeErr(w, code, c, msg)
		return
	}
	switch {
	case errors.Is(err, meters.ErrInvalidMeter):
		writeErr(w, 422, "METER_VALIDATION_FAILED", "meter could not be validated with the provider")
	case errors.Is(err, meters.ErrCannotVend):
		writeErr(w, 422, "METER_CANNOT_VEND", "this meter cannot be vended right now")
	case errors.Is(err, meters.ErrSandboxMeter):
		writeErr(w, 422, "SANDBOX_METER", "sandbox meter numbers are not allowed live")
	case errors.Is(err, orchestrator.ErrInFlight):
		writeErr(w, 409, "PURCHASE_IN_FLIGHT", "finish or wait for the open purchase on this meter")
	case errors.Is(err, orchestrator.ErrNameNotConfirmed):
		writeErr(w, 422, "NAME_NOT_CONFIRMED", "confirm the disco account name before paying")
	case errors.Is(err, orchestrator.ErrAccountSuspended):
		writeErr(w, 403, "ACCOUNT_SUSPENDED", "purchases are paused on this account")
	case errors.Is(err, orchestrator.ErrRiskDenied):
		writeErr(w, 403, "FORBIDDEN", "this payment was declined")
	case errors.Is(err, orchestrator.ErrVendingPaused):
		writeErr(w, 503, "VENDING_PAUSED", "electricity vending is paused")
	case errors.Is(err, orchestrator.ErrServicePaused):
		writeErr(w, 503, "SERVICE_PAUSED", "service is paused")
	case errors.Is(err, orchestrator.ErrPilotCap):
		writeErr(w, 429, "PILOT_CAP", "pilot daily cap reached")
	case errors.Is(err, orchestrator.ErrInvalidPilot):
		writeErr(w, 400, "INVALID_REQUEST", "invalid pilot allow-list entry")
	case errors.Is(err, meters.ErrNotFound):
		writeErr(w, 404, "METER_NOT_FOUND", "meter not found")
	case errors.Is(err, meters.ErrForbidden):
		writeErr(w, 403, "FORBIDDEN", "you do not have access to this meter")
	case errors.Is(err, orchestrator.ErrNotEligible):
		writeErr(w, 422, "METER_NOT_ELIGIBLE", err.Error())
	case errors.Is(err, orchestrator.ErrReadingUnavailable):
		writeErr(w, 404, "READING_UNAVAILABLE", "no confirmed meter reading is available")
	case errors.Is(err, identity.ErrNoPasskeys):
		writeErr(w, 404, "NOT_FOUND", "passkeys are not configured")
	case errors.Is(err, identity.ErrOrigin):
		writeErr(w, 400, "INVALID_REQUEST", "origin is not allowed")
	case errors.Is(err, identity.ErrBadAttestation), errors.Is(err, identity.ErrBadAssertion),
		errors.Is(err, identity.ErrChallenge):
		writeErr(w, 400, "INVALID_REQUEST", "passkey verification failed")
	case errors.Is(err, identity.ErrUnknownCredential), errors.Is(err, identity.ErrClonedKey),
		errors.Is(err, identity.ErrSession):
		writeErr(w, 401, "UNAUTHENTICATED", "passkey or session was rejected")
	case errors.Is(err, identity.ErrStepOrder), errors.Is(err, identity.ErrInvalidStep):
		writeErr(w, 422, "INVALID_REQUEST", err.Error())
	case errors.Is(err, security.ErrStepUp):
		writeErr(w, 403, "STEP_UP_REQUIRED", "confirm this device to continue")
	case errors.Is(err, security.ErrLastPasskey), errors.Is(err, identity.ErrLastPasskey):
		writeErr(w, 403, "FORBIDDEN", "cannot delete the last passkey")
	case errors.Is(err, security.ErrDenied):
		writeErr(w, 403, "FORBIDDEN", "action denied")
	case errors.Is(err, errInvalidJSON):
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
	case errors.Is(err, gifts.ErrInvalidContact), errors.Is(err, notifications.ErrInvalidPhone), errors.Is(err, notifications.ErrInvalidEmail), errors.Is(err, tokenshares.ErrUnsupportedChannel):
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
	case errors.Is(err, orchestrator.ErrTokenShareContact):
		writeErr(w, 422, "INVALID_REQUEST", "no verified contact for this channel")
	case errors.Is(err, orchestrator.ErrTokenShareDisabled):
		writeErr(w, 403, "FEATURE_DISABLED", "sending a token to another destination is not enabled")
	case errors.Is(err, orchestrator.ErrTokenShareOptedOut):
		writeErr(w, 422, "INVALID_REQUEST", "destination has opted out of this channel")
	case errors.Is(err, orchestrator.ErrTokenShareRateLimited):
		writeErr(w, 429, "RATE_LIMITED", "daily token send limit reached")
	case errors.Is(err, orchestrator.ErrAmountOutOfRange), errors.Is(err, orchestrator.ErrInvalidPreference),
		errors.Is(err, orchestrator.ErrInvalidPack):
		writeErr(w, 422, "INVALID_REQUEST", err.Error())
	case errors.Is(err, orchestrator.ErrQuoteExpired), errors.Is(err, orchestrator.ErrQuoteNotFound):
		writeErr(w, 422, "QUOTE_EXPIRED", err.Error())
	case errors.Is(err, orchestrator.ErrCreditOnMeter):
		writeErr(w, 409, "CREDIT_ON_METER", "credit is delivered to the meter; there is no keypad token until an operator falls back")
	case errors.Is(err, orchestrator.ErrTokenUnavailable):
		writeErr(w, 409, "DELIVERY_FAILED", "no token is available for this transaction")
	case errors.Is(err, meters.ErrInvalidCohort):
		writeErr(w, 422, "INVALID_REQUEST", "invalid remote-credit cohort")
	case errors.Is(err, reconcile.ErrIdempotencyKey):
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
	case errors.Is(err, reconcile.ErrIdempotencyConflict):
		writeErr(w, 409, "IDEMPOTENCY_CONFLICT", err.Error())
	case errors.Is(err, reconcile.ErrSameApprover):
		writeErr(w, 409, "INVALID_REQUEST", "proposer cannot approve")
	case errors.Is(err, reconcile.ErrNotProposed):
		writeErr(w, 409, "INVALID_REQUEST", err.Error())
	case errors.Is(err, reconcile.ErrUnknownAccount), errors.Is(err, reconcile.ErrBadStatus):
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
	case errors.Is(err, reconcile.ErrNotFound):
		writeErr(w, 404, "NOT_FOUND", "not found")
	case errors.Is(err, reconcile.ErrNoSource):
		writeErr(w, 503, "PROVIDER_UNAVAILABLE", "reconciliation source is not configured")
	case errors.Is(err, store.ErrDuplicate):
		writeErr(w, 409, "DUPLICATE_RESOURCE", "resource already exists")
	case errors.Is(err, orchestrator.ErrPaymentMismatch):
		writeErr(w, 422, "PAYMENT_UNCONFIRMED", err.Error())
	case errors.Is(err, idempotency.ErrConflict):
		writeErr(w, 409, "IDEMPOTENCY_CONFLICT", "Idempotency-Key was used with a different request")
	case errors.Is(err, idempotency.ErrInProgress):
		writeErr(w, 409, "IDEMPOTENCY_CONFLICT", "a request with this Idempotency-Key is in progress")
	case errors.Is(err, orchestrator.ErrReasonRequired):
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
	case errors.Is(err, orchestrator.ErrNotInReview), errors.Is(err, orchestrator.ErrRefundNotSafe), errors.Is(err, orchestrator.ErrNothingToRefund):
		writeErr(w, 409, "INVALID_REQUEST", err.Error())
	case errors.Is(err, orchestrator.ErrProviderDisabled):
		writeErr(w, 503, "PROVIDER_UNAVAILABLE", "provider temporarily disabled")
	case errors.Is(err, risk.ErrNotFound), errors.Is(err, disputes.ErrNotFound):
		writeErr(w, 404, "NOT_FOUND", "not found")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "NOT_FOUND", "not found")
	case errors.Is(err, tx.ErrNotFound):
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
	case errors.Is(err, tx.ErrInvalidTransition), errors.Is(err, tx.ErrConflict):
		writeErr(w, 409, "INVALID_REQUEST", err.Error())
	case providers.IsAmbiguous(err):
		writeErr(w, 502, "PROVIDER_UNAVAILABLE", "provider did not respond in time")
	default:
		s.Log.Error("internal error", "err", err, "path", r.Pattern)
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
	}
}
