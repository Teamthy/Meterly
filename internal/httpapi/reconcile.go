package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/reconcile"
)

func (s *Server) adminReconcileRoutes(mux *patternMux) {
	op, admin := s.authed(auth.RoleOperator, false), s.authed(auth.RoleAdmin, false)
	mux.Handle("GET /v1/admin/reconciliation/exceptions", op(s.adminReconList))
	mux.Handle("GET /v1/admin/reconciliation/exceptions/{id}", op(s.adminReconDetail))
	mux.Handle("POST /v1/admin/reconciliation/exceptions/{id}/assign", op(s.adminReconAssign))
	mux.Handle("POST /v1/admin/reconciliation/exceptions/{id}/resolve", op(s.adminReconResolve))
	mux.Handle("POST /v1/admin/reconciliation/exceptions/{id}/adjustments", op(s.adminReconPropose))
	mux.Handle("POST /v1/admin/reconciliation/adjustments/{id}/approve", admin(s.adminReconApprove))
	mux.Handle("GET /v1/admin/reconciliation/summary", op(s.adminReconSummary))
	mux.Handle("POST /v1/admin/reconciliation/wallet-topup", admin(s.adminReconWalletTopup))
}

func (s *Server) reconEngine(w http.ResponseWriter) *reconcile.Engine {
	if s.Reconcile == nil {
		writeErr(w, 503, "PROVIDER_UNAVAILABLE", "reconciliation is not configured")
		return nil
	}
	return s.Reconcile
}

func exceptionView(e reconcile.Exception) map[string]any {
	return map[string]any{
		"id": e.ID, "run_id": e.RunID, "provider": e.Provider,
		"business_date": reconcile.DateOnly(e.BusinessDate).Format("2006-01-02"),
		"category":      e.Category, "severity": e.Severity, "provider_ref": e.ProviderRef,
		"our_transaction_id": e.OurTxnID, "ours_minor": e.OursMinor, "theirs_minor": e.TheirsMinor,
		"currency": e.Currency, "status": e.Status, "owner": e.Owner, "notes": e.Notes,
		"created_at": e.CreatedAt, "updated_at": e.UpdatedAt,
	}
}

func adjustmentView(a reconcile.Adjustment) map[string]any {
	return map[string]any{
		"id": a.ID, "exception_id": a.ExceptionID, "proposed_by": a.ProposedBy,
		"approved_by": a.ApprovedBy, "status": a.Status,
		"debit_account": a.DebitAccount, "credit_account": a.CreditAccount,
		"amount_minor": a.AmountMinor, "currency": a.Currency,
		"ledger_reference": a.LedgerReference, "reason": a.Reason,
		"created_at": a.CreatedAt, "approved_at": a.ApprovedAt,
	}
}

func (s *Server) adminReconList(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	q := r.URL.Query()
	f := reconcile.ExceptionFilter{Provider: q.Get("provider"), Status: reconcile.ExceptionStatus(q.Get("status")), Category: reconcile.Category(q.Get("category")), Limit: 50}
	if v := q.Get("business_date"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, reconcile.WAT)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "business_date must be YYYY-MM-DD")
			return
		}
		f.BusinessDate = d
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, 400, "INVALID_REQUEST", "limit must be 1–200")
			return
		}
		f.Limit = n
	}
	list, err := eng.Host.Reconcile().ListExceptions(r.Context(), f)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, exceptionView(e))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func reconExceptionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) adminReconDetail(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	id, ok := reconExceptionID(w, r)
	if !ok {
		return
	}
	e, err := eng.Host.Reconcile().GetException(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, exceptionView(e))
}

func (s *Server) adminReconAssign(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	id, ok := reconExceptionID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
		Owner  string `json:"owner"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	e, err := eng.Assign(r.Context(), actor, id, body.Owner)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, exceptionView(e))
}

func (s *Server) adminReconResolve(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	id, ok := reconExceptionID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
		Status string `json:"status"`
		Notes  string `json:"notes"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	e, err := eng.Resolve(r.Context(), actor, id, reconcile.ExceptionStatus(body.Status), body.Notes)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, exceptionView(e))
}

func (s *Server) adminReconPropose(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	id, ok := reconExceptionID(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason        string `json:"reason"`
		DebitAccount  string `json:"debit_account"`
		CreditAccount string `json:"credit_account"`
		AmountMinor   int64  `json:"amount_minor"`
		Currency      string `json:"currency"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	a, err := eng.ProposeAdjustment(r.Context(), actor, id, ledger.AccountCode(body.DebitAccount), ledger.AccountCode(body.CreditAccount), body.AmountMinor, body.Currency, body.Reason)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, adjustmentView(a))
}

func (s *Server) adminReconApprove(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	id, ok := reconExceptionID(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	a, err := eng.ApproveAdjustment(r.Context(), actor, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, adjustmentView(a))
}

func (s *Server) adminReconSummary(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	date := reconcile.BusinessDate(time.Now())
	if v := r.URL.Query().Get("date"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, reconcile.WAT)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "date must be YYYY-MM-DD")
			return
		}
		date = d
	}
	sum, err := eng.DailySummary(r.Context(), date)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	runs := make([]map[string]any, 0, len(sum.Runs))
	for _, run := range sum.Runs {
		runs = append(runs, map[string]any{
			"id": run.ID, "provider": run.Provider,
			"business_date": reconcile.DateOnly(run.BusinessDate).Format("2006-01-02"),
			"status":        run.Status, "matched_count": run.MatchedCount,
			"matched_minor": run.MatchedMinor, "exception_count": run.ExceptionCount,
		})
	}
	writeJSON(w, 200, map[string]any{
		"business_date":          reconcile.DateOnly(sum.BusinessDate).Format("2006-01-02"),
		"runs":                   runs,
		"exceptions_by_category": sum.ExceptionsByCat,
		"open_count":             sum.OpenCount,
		"matched_count":          sum.MatchedCount,
		"matched_minor":          sum.MatchedMinor,
		"psp_receivable_minor":   sum.ReceivableMinor,
	})
}

func (s *Server) adminReconWalletTopup(w http.ResponseWriter, r *http.Request) {
	eng := s.reconEngine(w)
	if eng == nil {
		return
	}
	var body struct {
		Reason      string `json:"reason"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	posted, err := eng.RecordWalletTopup(r.Context(), actor, body.AmountMinor, body.Currency)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": posted.ID, "reference": posted.Reference, "amount_minor": body.AmountMinor})
}
