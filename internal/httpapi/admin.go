package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Admin API (spec §11). Operators investigate and nudge transactions;
// admins can also refund from review and switch providers. Every mutating
// action needs a reason and is audited.
func (s *Server) adminRoutes(mux *patternMux) {
	op, admin := s.authed(auth.RoleOperator, false), s.authed(auth.RoleAdmin, false)
	mux.Handle("GET /v1/admin/console", op(s.adminConsole))
	mux.Handle("GET /v1/admin/transactions", op(s.adminSearch))
	mux.Handle("GET /v1/admin/transactions/{transaction_id}", op(s.adminDetail))
	mux.Handle("POST /v1/admin/transactions/{transaction_id}/reconcile", op(s.adminAction(s.O.Reconcile)))
	mux.Handle("POST /v1/admin/transactions/{transaction_id}/fallback-to-token", op(s.adminAction(s.O.FallbackToToken)))
	mux.Handle("POST /v1/admin/transactions/{transaction_id}/token-reveal", op(s.adminRevealToken))
	mux.Handle("POST /v1/admin/transactions/{transaction_id}/requeue", admin(s.adminAction(s.O.Requeue)))
	mux.Handle("POST /v1/admin/transactions/{transaction_id}/refund", admin(s.adminAction(s.O.OperatorRefund)))
	mux.Handle("GET /v1/admin/providers", op(s.adminProviders))
	mux.Handle("POST /v1/admin/providers/{name}/enable", admin(s.adminSetProvider(true)))
	mux.Handle("POST /v1/admin/providers/{name}/disable", admin(s.adminSetProvider(false)))
	mux.Handle("POST /v1/admin/unit-meters/{id}/verify", op(s.adminVerifyUnitMeter))
	mux.Handle("POST /v1/admin/meters/{meter_id}/ownership", op(s.adminAttestOwnership))
	mux.Handle("POST /v1/admin/remote-credit/cohorts", admin(s.adminCreateRemoteCohort))
	mux.Handle("GET /v1/admin/remote-credit/cohorts", op(s.adminListRemoteCohorts))
	mux.Handle("DELETE /v1/admin/remote-credit/cohorts/{id}", admin(s.adminRevokeRemoteCohort))
	s.adminSupportRoutes(mux)
	s.adminReconcileRoutes(mux)
}

// actor builds the audit actor from the caller and a {"reason": "..."} body.
func (s *Server) actor(w http.ResponseWriter, r *http.Request) (audit.Actor, bool) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return audit.Actor{}, false
	}
	return audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}, true
}

func txnID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("transaction_id"))
	if err != nil {
		writeErr(w, 404, "TRANSACTION_NOT_FOUND", "transaction not found")
		return uuid.Nil, false
	}
	return id, true
}

func adminView(t *tx.Transaction) map[string]any {
	v := view(t)
	v["customer_id"] = t.CustomerID
	v["check_attempts"] = t.CheckAttempts
	v["next_check_at"] = t.NextCheckAt
	v["payment_provider"] = t.PaymentProviderName
	v["payment_reference"] = t.PaymentProviderRef
	v["vending_provider"] = t.VendProviderName
	v["vending_reference"] = t.VendProviderRef
	v["delivery_provider"] = t.DeliveryProviderName
	v["delivery_reference"] = t.DeliveryProviderRef
	v["refund_reference"] = t.RefundProviderRef
	v["refund_amount_minor"] = t.RefundAmount.Minor
	// Never the token itself; revealing it is a separate audited action.
	v["token_available"] = len(t.TokenCiphertext) > 0
	return v
}

func (s *Server) adminConsole(w http.ResponseWriter, r *http.Request) {
	c, err := s.O.AdminConsole(r.Context(), callerOf(r).Principal.Has(auth.RoleAdmin))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) adminSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TxnFilter{
		ReviewOnly: q.Get("review") == "true",
		Payment:    tx.PaymentState(q.Get("payment_status")),
		Vending:    tx.VendingState(q.Get("vending_status")),
		Delivery:   tx.DeliveryState(q.Get("delivery_status")),
		CustomerID: q.Get("customer_id"),
	}
	if v := q.Get("customer_id"); v != "" {
		if _, err := uuid.Parse(v); err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "customer_id must be a UUID")
			return
		}
	}
	if v := q.Get("meter_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "meter_id must be a UUID")
			return
		}
		f.MeterID = id
	}
	if v := q.Get("before"); v != "" {
		ts, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "before must be an RFC 3339 timestamp")
			return
		}
		f.Before = ts
	}
	f.Limit = 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, 400, "INVALID_REQUEST", "limit must be 1–200")
			return
		}
		f.Limit = n
	}
	list, err := s.O.Search(r.Context(), f)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, adminView(t))
	}
	resp := map[string]any{"data": out}
	if len(list) == f.Limit {
		resp["next_before"] = list[len(list)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, 200, resp)
}

func (s *Server) adminDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := txnID(w, r)
	if !ok {
		return
	}
	actor := audit.Actor{
		UserID:        userID(r),
		Type:          audit.ActorOperator,
		CorrelationID: audit.CorrelationFrom(r.Context()),
	}
	d, err := s.O.DetailForOperator(r.Context(), id, actor)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"transaction":   adminView(d.Transaction),
		"ledger":        d.Ledger,
		"events":        d.Events,
		"audit":         d.Audit,
		"notifications": d.Notifications,
		"investigation": orchestrator.InvestigationFrom(d, callerOf(r).Principal.Has(auth.RoleAdmin)),
	})
}

func (s *Server) adminAction(fn func(ctx context.Context, id uuid.UUID, a audit.Actor) (*tx.Transaction, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := txnID(w, r)
		if !ok {
			return
		}
		a, ok := s.actor(w, r)
		if !ok {
			return
		}
		t, err := fn(r.Context(), id, a)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, 200, adminView(t))
	}
}

func (s *Server) adminRevealToken(w http.ResponseWriter, r *http.Request) {
	id, ok := txnID(w, r)
	if !ok {
		return
	}
	a, ok := s.actor(w, r)
	if !ok {
		return
	}
	tok, t, err := s.O.RevealToken(r.Context(), id, a)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"transaction_id": t.ID, "token": tok, "units_kwh": t.UnitsKWh})
}

func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	st, err := s.O.ProviderStatus(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for name, enabled := range st {
		out = append(out, map[string]any{"name": name, "enabled": enabled})
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminSetProvider(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.actor(w, r)
		if !ok {
			return
		}
		name := r.PathValue("name")
		if err := s.O.SetProviderEnabled(r.Context(), name, enabled, a); err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"name": name, "enabled": enabled})
	}
}

func (s *Server) adminAttestOwnership(w http.ResponseWriter, r *http.Request) {
	if s.HouseholdsDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "households are not enabled")
		return
	}
	mid, ok := parsePathUUID(w, r, "meter_id")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
		UserID string `json:"user_id"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 || body.UserID == "" {
		writeErr(w, 400, "INVALID_REQUEST", "user_id and a reason (5–500 characters) are required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	own, err := s.O.AttestOwnership(r.Context(), actor, mid, body.UserID)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, own)
}
