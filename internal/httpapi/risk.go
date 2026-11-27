package httpapi

import (
	"github.com/teamthy/meterly/internal/store"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/risk"
)

func disputeNotice(evt providers.WebhookEvent) disputes.Notice {
	n := disputes.Notice{
		Type: evt.Type, PSPDisputeID: evt.DisputeID, Reference: evt.Reference,
		AmountMinor: evt.AmountMinor, Currency: evt.Currency, PSPStatus: evt.Status,
		Resolution: evt.Resolution, Category: evt.Category,
	}
	if evt.DueAt != nil {
		n.DueAt = *evt.DueAt
	}
	return n
}

func (s *Server) adminRiskRoutes(mux *patternMux) {
	op, admin := s.authed(auth.RoleOperator, false), s.authed(auth.RoleAdmin, false)
	mux.Handle("GET /v1/admin/risk/rules", op(s.adminRiskListRules))
	mux.Handle("PUT /v1/admin/risk/rules/{code}", op(s.adminRiskUpdateRule))
	mux.Handle("GET /v1/admin/risk/denylist", admin(s.adminRiskListDeny))
	mux.Handle("POST /v1/admin/risk/denylist", admin(s.adminRiskAddDeny))
	mux.Handle("DELETE /v1/admin/risk/denylist/{id}", admin(s.adminRiskRemoveDeny))
	mux.Handle("GET /v1/admin/disputes", op(s.adminDisputeList))
	mux.Handle("GET /v1/admin/disputes/{id}", op(s.adminDisputeGet))
	mux.Handle("GET /v1/admin/disputes/{id}/evidence", op(s.adminDisputeEvidence))
}

func ruleView(r risk.Rule) map[string]any {
	return map[string]any{
		"code": r.Code, "enabled": r.Enabled, "action": r.Action,
		"window_seconds": r.WindowSeconds, "threshold": r.Threshold,
		"params": r.Params, "updated_at": r.UpdatedAt, "updated_by": r.UpdatedBy,
	}
}

func (s *Server) adminRiskListRules(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.Store().Repos().Risk().ListRules(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, rule := range list {
		out = append(out, ruleView(rule))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminRiskUpdateRule(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	var body struct {
		Reason        string `json:"reason"`
		Enabled       *bool  `json:"enabled"`
		Action        string `json:"action"`
		WindowSeconds *int   `json:"window_seconds"`
		Threshold     *int64 `json:"threshold"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	repo := s.O.Store().Repos().Risk()
	rule, err := repo.GetRule(r.Context(), code)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	before := rule
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.Action != "" {
		rule.Action = risk.Outcome(strings.ToUpper(body.Action))
		if rule.Action != risk.Deny && rule.Action != risk.Review {
			writeErr(w, 400, "INVALID_REQUEST", "action must be DENY or REVIEW")
			return
		}
	}
	if body.WindowSeconds != nil {
		if *body.WindowSeconds < 0 {
			writeErr(w, 400, "INVALID_REQUEST", "window_seconds must be >= 0")
			return
		}
		rule.WindowSeconds = *body.WindowSeconds
	}
	if body.Threshold != nil {
		if *body.Threshold < 0 {
			writeErr(w, 400, "INVALID_REQUEST", "threshold must be >= 0")
			return
		}
		rule.Threshold = *body.Threshold
	}
	rule.UpdatedBy = actor.UserID
	rule.UpdatedAt = time.Now().UTC()
	// The change and its audit row commit together: a fraud rule must never
	// be loosened without a durable record of who did it and why.
	err = s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		if err := repos.Risk().UpdateRule(r.Context(), rule); err != nil {
			return err
		}
		return repos.Audit().Add(r.Context(), actor.Event("risk.rule.update", "risk_rule", uuid.Nil, map[string]any{
			"code":           code,
			"enabled_before": before.Enabled, "enabled_after": rule.Enabled,
			"action_before": string(before.Action), "action_after": string(rule.Action),
			"window_before": before.WindowSeconds, "window_after": rule.WindowSeconds,
			"threshold_before": before.Threshold, "threshold_after": rule.Threshold,
		}))
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, ruleView(rule))
}

func denyView(e risk.DenyEntry) map[string]any {
	return map[string]any{
		"id": e.ID, "kind": e.Kind, "value": e.Value, "reason": e.Reason,
		"created_at": e.CreatedAt, "created_by": e.CreatedBy,
	}
}

func (s *Server) adminRiskListDeny(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.Store().Repos().Risk().ListDenylist(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, denyView(e))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminRiskAddDeny(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
		Kind   string `json:"kind"`
		Value  string `json:"value"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	if strings.TrimSpace(body.Kind) == "" || strings.TrimSpace(body.Value) == "" {
		writeErr(w, 400, "INVALID_REQUEST", "kind and value are required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	kind := risk.DenyKind(strings.ToUpper(strings.TrimSpace(body.Kind)))
	switch kind {
	case risk.DenyCard, risk.DenyEmail, risk.DenyPhone, risk.DenyIPRange:
	default:
		writeErr(w, 400, "INVALID_REQUEST", "kind must be CARD, EMAIL, PHONE or IP_RANGE")
		return
	}
	e := risk.DenyEntry{
		ID: uuid.New(), Kind: kind, Value: strings.TrimSpace(body.Value),
		Reason: actor.Reason, CreatedAt: time.Now().UTC(), CreatedBy: actor.UserID,
	}
	if err := s.O.Store().Repos().Risk().AddDenylist(r.Context(), e); err != nil {
		s.mapErr(w, r, err)
		return
	}
	_ = s.O.Store().Repos().Audit().Add(r.Context(), actor.Event("risk.denylist.add", "risk_denylist", e.ID, map[string]any{
		"kind": string(kind),
	}))
	writeJSON(w, 201, denyView(e))
}

func (s *Server) adminRiskRemoveDeny(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	if err := s.O.Store().Repos().Risk().RemoveDenylist(r.Context(), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	_ = s.O.Store().Repos().Audit().Add(r.Context(), actor.Event("risk.denylist.remove", "risk_denylist", id, nil))
	w.WriteHeader(http.StatusNoContent)
}

func disputeView(d disputes.Dispute) map[string]any {
	return map[string]any{
		"id": d.ID, "psp_dispute_id": d.PSPDisputeID, "transaction_id": d.TransactionID,
		"payment_ref": d.PaymentRef, "amount_minor": d.Amount.Minor, "currency": d.Amount.Currency,
		"status": d.Status, "psp_status": d.PSPStatus, "resolution": d.Resolution,
		"category": d.Category, "due_at": d.DueAt, "reminded_at": d.RemindedAt,
		"resolved_at": d.ResolvedAt, "created_at": d.CreatedAt, "updated_at": d.UpdatedAt,
	}
}

func (s *Server) adminDisputeList(w http.ResponseWriter, r *http.Request) {
	f := disputes.Filter{Status: disputes.Status(r.URL.Query().Get("status")), Limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, 400, "INVALID_REQUEST", "limit must be 1–200")
			return
		}
		f.Limit = n
	}
	list, err := s.O.Store().Repos().Disputes().List(r.Context(), f)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, disputeView(d))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminDisputeGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	d, err := s.O.Store().Repos().Disputes().Get(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, disputeView(d))
}

func (s *Server) adminDisputeEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	pack, err := s.O.DisputeEvidence(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, pack)
}
