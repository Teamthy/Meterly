// Package httpapi exposes the /v1 REST API (spec §7). Handlers only parse,
// authorise and map errors; all business rules live in the orchestrator.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/mock"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Authenticator resolves a bearer token to a user ID. Production must use
// an OIDC-compatible implementation.
type Authenticator interface {
	Authenticate(r *http.Request) (userID string, err error)
}

// DevAuth treats the bearer token itself as the user ID. Development only.
type DevAuth struct{}

func (DevAuth) Authenticate(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", errors.New("missing bearer token")
	}
	return tok, nil
}

type Server struct {
	O        *orchestrator.Orchestrator
	Auth     Authenticator
	Idem     *idempotency.Store
	Payments providers.PaymentProvider
	Log      *slog.Logger
	// Dev enables /v1/dev/* simulation endpoints. Never enable in production.
	Dev       bool
	DevPay    *mock.Payments
	DevRemote *mock.RemoteCredit
}

type ctxKey struct{}

func userID(r *http.Request) string { return r.Context().Value(ctxKey{}).(string) }

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	mux.Handle("POST /v1/meters/validate", s.authed(s.validateMeter))
	mux.Handle("POST /v1/meters", s.authed(s.registerMeter))
	mux.Handle("GET /v1/meters", s.authed(s.listMeters))
	mux.Handle("GET /v1/meters/{meter_id}", s.authed(s.getMeter))
	mux.Handle("GET /v1/meters/{meter_id}/capabilities", s.authed(s.getCapabilities))
	mux.Handle("POST /v1/beneficiaries", s.authed(s.addBeneficiary))

	mux.Handle("POST /v1/electricity/quotes", s.authed(s.createQuote))
	mux.Handle("POST /v1/electricity/transactions", s.authed(s.createTransaction))
	mux.Handle("GET /v1/electricity/transactions", s.authed(s.listTransactions))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}", s.authed(s.getTransaction))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/status", s.authed(s.getTransaction))
	mux.Handle("POST /v1/electricity/transactions/{transaction_id}/payment-check", s.authed(s.paymentCheck))
	mux.Handle("GET /v1/electricity/transactions/{transaction_id}/token", s.authed(s.getToken))

	mux.HandleFunc("POST /v1/payments/webhooks/{provider}", s.paymentWebhook)

	if s.Dev {
		mux.HandleFunc("POST /v1/dev/payments/{ref}/complete", s.devCompletePayment)
		mux.HandleFunc("POST /v1/dev/remote/settle", s.devSettle)
		mux.HandleFunc("POST /v1/dev/reconcile", s.devReconcile)
		mux.HandleFunc("POST /v1/dev/transactions/{transaction_id}/fallback-to-token", s.devFallback)
	}
	return s.withCorrelation(mux)
}

// ---- middleware ------------------------------------------------------------

func (s *Server) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get("X-Correlation-ID")
		if _, err := uuid.Parse(cid); err != nil {
			cid = uuid.NewString()
		}
		w.Header().Set("X-Correlation-ID", cid)
		start := time.Now()
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
		// Never log bodies: they can contain meter numbers and tokens.
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "cid", cid, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := s.Auth.Authenticate(r)
		if err != nil {
			writeErr(w, r, 401, "UNAUTHENTICATED", "authentication required")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, uid)))
	})
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
	if !decode(w, r, &req) || !req.valid() {
		writeErr(w, r, 400, "INVALID_REQUEST", "meter_number and disco_code are required")
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
	if !decode(w, r, &req) || !req.valid() {
		writeErr(w, r, 400, "INVALID_REQUEST", "meter_number and disco_code are required")
		return
	}
	m, err := s.O.Meters.Register(r.Context(), userID(r), req.MeterNumber, req.DiscoCode, req.Label, meters.PermManage)
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
	if !decode(w, r, &req) || !req.valid() || req.DisplayName == "" {
		writeErr(w, r, 400, "INVALID_REQUEST", "meter_number, disco_code and display_name are required")
		return
	}
	m, err := s.O.Meters.Register(r.Context(), userID(r), req.MeterNumber, req.DiscoCode, req.DisplayName, meters.PermPurchase)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"meter_id": m.ID, "display_name": req.DisplayName, "masked_meter_number": m.Masked, "customer_name": m.CustomerName})
}

func (s *Server) listMeters(w http.ResponseWriter, r *http.Request) {
	list := s.O.Meters.ListForUser(userID(r))
	if list == nil {
		list = []meters.Listed{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) meterFromPath(w http.ResponseWriter, r *http.Request) (*meters.Meter, bool) {
	id, err := uuid.Parse(r.PathValue("meter_id"))
	if err != nil {
		writeErr(w, r, 404, "METER_NOT_FOUND", "meter not found")
		return nil, false
	}
	m, err := s.O.Meters.Authorize(userID(r), id, meters.PermView)
	if err != nil {
		s.mapErr(w, r, err)
		return nil, false
	}
	return m, true
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
		MeterID            uuid.UUID                       `json:"meter_id"`
		AmountMinor        int64                           `json:"amount_minor"`
		Currency           string                          `json:"currency"`
		DeliveryPreference orchestrator.DeliveryPreference `json:"delivery_preference"`
	}
	if !decode(w, r, &req) || req.MeterID == uuid.Nil || req.Currency != "NGN" {
		writeErr(w, r, 400, "INVALID_REQUEST", "meter_id, amount_minor and currency=NGN are required")
		return
	}
	q, err := s.O.CreateQuote(userID(r), req.MeterID, req.AmountMinor, req.DeliveryPreference)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, q)
}

func (s *Server) createTransaction(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		writeErr(w, r, 400, "INVALID_REQUEST", "Idempotency-Key header (16–128 chars) is required")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, r, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	var req struct {
		QuoteID       uuid.UUID `json:"quote_id"`
		Email         string    `json:"email"`
		IsGift        bool      `json:"is_gift"`
		RecipientName string    `json:"recipient_name"`
	}
	if json.Unmarshal(body, &req) != nil || req.QuoteID == uuid.Nil {
		writeErr(w, r, 400, "INVALID_REQUEST", "quote_id is required")
		return
	}
	uid := userID(r)
	prev, fresh, err := s.Idem.Begin(uid, "create_transaction", key, idempotency.HashRequest(body))
	switch {
	case errors.Is(err, idempotency.ErrConflict):
		writeErr(w, r, 409, "IDEMPOTENCY_CONFLICT", "Idempotency-Key was used with a different request")
		return
	case errors.Is(err, idempotency.ErrInProgress):
		writeErr(w, r, 409, "IDEMPOTENCY_CONFLICT", "a request with this Idempotency-Key is in progress")
		return
	case !fresh:
		t, err := s.O.Txns.Get(uuid.MustParse(prev))
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, 202, view(t))
		return
	}
	t, err := s.O.CreateTransaction(r.Context(), orchestrator.CreateRequest{CustomerID: uid, QuoteID: req.QuoteID, Email: req.Email, IsGift: req.IsGift, RecipientName: req.RecipientName})
	if err != nil {
		s.Idem.Abort(uid, "create_transaction", key)
		s.mapErr(w, r, err)
		return
	}
	s.Idem.Complete(uid, "create_transaction", key, t.ID.String())
	writeJSON(w, 202, view(t))
}

func view(t *tx.Transaction) map[string]any {
	return map[string]any{
		"transaction_id": t.ID, "status": t.Status(),
		"payment_status": t.Payment, "vending_status": t.Vending, "delivery_status": t.Delivery,
		"delivery_method": t.DeliveryMethod, "meter_id": t.MeterID, "is_gift": t.IsGift, "recipient_name": t.RecipientName,
		"amount_minor": t.Amount.Minor, "fee_minor": t.Fee.Minor, "total_minor": t.Total.Minor, "currency": t.Total.Currency,
		"units_kwh": t.UnitsKWh, "failure_code": t.FailureCode, "checkout_url": t.CheckoutURL,
		"history": t.History, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
}

func (s *Server) txnFromPath(w http.ResponseWriter, r *http.Request) (*tx.Transaction, bool) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, r, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return nil, false
	}
	t, err := s.O.Txns.Get(id)
	if err != nil || t.CustomerID != userID(r) {
		writeErr(w, r, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return nil, false
	}
	return t, true
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.txnFromPath(w, r); ok {
		writeJSON(w, 200, view(t))
	}
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, t := range s.O.Txns.ListForCustomer(userID(r)) {
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
		writeErr(w, r, 422, "PAYMENT_UNCONFIRMED", "payment has not been initialised")
		return
	}
	t, err := s.O.HandlePaymentNotification(r.Context(), t.PaymentProviderRef)
	if err != nil && t == nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, view(t))
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, r, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	tok, t, err := s.O.Token(userID(r), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	m, _ := s.O.Meters.Get(t.MeterID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"transaction_id": t.ID, "token": tok, "units_kwh": t.UnitsKWh, "masked_meter_number": m.Masked,
		"note": "This token works only on the meter it was bought for. Sharing it does not move credit to another meter.",
	})
}

// ---- webhooks -------------------------------------------------------------------

func (s *Server) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("provider") != s.Payments.Name() {
		writeErr(w, r, 404, "INVALID_REQUEST", "unknown provider")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, r, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	headers := map[string]string{}
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}
	if err := s.Payments.VerifyWebhook(headers, body); err != nil {
		writeErr(w, r, 401, "UNAUTHENTICATED", "invalid webhook signature")
		return
	}
	var evt struct {
		Reference string `json:"reference"`
	}
	if json.Unmarshal(body, &evt) != nil || evt.Reference == "" {
		writeErr(w, r, 400, "INVALID_REQUEST", "reference missing")
		return
	}
	// Acknowledge regardless of downstream outcome; verification and
	// reconciliation own the result.
	if _, err := s.O.HandlePaymentNotification(r.Context(), evt.Reference); err != nil {
		s.Log.Warn("webhook processing", "err", err)
	}
	writeJSON(w, 200, map[string]string{"status": "received"})
}

// ---- dev simulation -------------------------------------------------------------

func (s *Server) devCompletePayment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Success *bool `json:"success"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ok := req.Success == nil || *req.Success
	if err := s.DevPay.Complete(r.PathValue("ref"), ok, 0); err != nil {
		writeErr(w, r, 404, "INVALID_REQUEST", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"completed": true, "success": ok})
}

func (s *Server) devSettle(w http.ResponseWriter, _ *http.Request) {
	s.DevRemote.Settle()
	writeJSON(w, 200, map[string]any{"settled": true})
}

func (s *Server) devReconcile(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"resolved": s.O.ReconcileAll(r.Context())})
}

func (s *Server) devFallback(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, r, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}
	t, err := s.O.FallbackToToken(id, "dev-operator")
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, view(t))
}

// ---- helpers ----------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, r *http.Request, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"code": errCode, "message": msg, "request_id": w.Header().Get("X-Correlation-ID"), "details": map[string]any{},
	}})
}

func (s *Server) mapErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, meters.ErrInvalidMeter):
		writeErr(w, r, 422, "METER_VALIDATION_FAILED", "meter could not be validated with the provider")
	case errors.Is(err, meters.ErrNotFound):
		writeErr(w, r, 404, "METER_NOT_FOUND", "meter not found")
	case errors.Is(err, meters.ErrForbidden):
		writeErr(w, r, 403, "FORBIDDEN", "you do not have access to this meter")
	case errors.Is(err, orchestrator.ErrNotEligible):
		writeErr(w, r, 422, "METER_NOT_ELIGIBLE", err.Error())
	case errors.Is(err, orchestrator.ErrAmountOutOfRange):
		writeErr(w, r, 422, "INVALID_REQUEST", err.Error())
	case errors.Is(err, orchestrator.ErrQuoteExpired), errors.Is(err, orchestrator.ErrQuoteNotFound):
		writeErr(w, r, 422, "QUOTE_EXPIRED", err.Error())
	case errors.Is(err, orchestrator.ErrTokenUnavailable):
		writeErr(w, r, 409, "DELIVERY_FAILED", "no token is available for this transaction")
	case errors.Is(err, orchestrator.ErrPaymentMismatch):
		writeErr(w, r, 422, "PAYMENT_UNCONFIRMED", err.Error())
	case errors.Is(err, tx.ErrNotFound):
		writeErr(w, r, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
	case errors.Is(err, tx.ErrInvalidTransition):
		writeErr(w, r, 409, "INVALID_REQUEST", err.Error())
	case providers.IsAmbiguous(err):
		writeErr(w, r, 502, "PROVIDER_UNAVAILABLE", "provider did not respond in time")
	default:
		s.Log.Error("internal error", "err", err, "path", r.URL.Path)
		writeErr(w, r, 500, "INTERNAL_ERROR", "internal error")
	}
}
