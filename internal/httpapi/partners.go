package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/security"
)

func (s *Server) requirePartnerAPI(w http.ResponseWriter) bool {
	if s.PartnerDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "partner API is not enabled")
		return false
	}
	return true
}

func (s *Server) partnerRoutes(mux *patternMux) {
	me := s.partnerAuthed("")
	mux.Handle("GET /v1/partner/me", me(s.partnerMe))
	mux.Handle("POST /v1/partner/meters/validate", s.partnerAuthed(partners.ScopeMetersValidate)(s.validateMeter))
	mux.Handle("POST /v1/partner/quotes", s.partnerAuthed(partners.ScopeQuotesCreate)(s.partnerCreateQuote))
	mux.Handle("POST /v1/partner/purchases", s.partnerAuthed(partners.ScopePurchasesCreate)(s.partnerCreatePurchase))
	mux.Handle("GET /v1/partner/purchases/{id}", s.partnerAuthed(partners.ScopePurchasesRead)(s.partnerGetPurchase))
	mux.Handle("GET /v1/partner/purchases", s.partnerAuthed(partners.ScopePurchasesRead)(s.partnerLookupPurchase))
	mux.Handle("GET /v1/partner/float", s.partnerAuthed(partners.ScopeFloatRead)(s.partnerGetFloat))
}

func (s *Server) adminPartnerRoutes(mux *patternMux) {
	op := s.authed(auth.RoleOperator, false)
	mux.Handle("POST /v1/admin/partners", op(s.adminCreatePartner))
	mux.Handle("GET /v1/admin/partners", op(s.adminListPartners))
	mux.Handle("GET /v1/admin/partners/{id}", op(s.adminGetPartner))
	mux.Handle("POST /v1/admin/partners/{id}/activate", op(s.adminActivatePartner))
	mux.Handle("POST /v1/admin/partners/{id}/suspend", op(s.adminSuspendPartner))
	mux.Handle("POST /v1/admin/partners/{id}/keys", op(s.adminIssuePartnerKey))
	mux.Handle("GET /v1/admin/partners/{id}/keys", op(s.adminListPartnerKeys))
	mux.Handle("POST /v1/admin/partners/{id}/keys/{key_id}/rotate", op(s.adminRotatePartnerKey))
	mux.Handle("POST /v1/admin/partners/{id}/keys/{key_id}/revoke", op(s.adminRevokePartnerKey))
	mux.Handle("GET /v1/admin/partners/{id}/usage", op(s.adminPartnerUsage))
	mux.Handle("GET /v1/admin/partners/{id}/float", op(s.adminGetPartnerFloat))
	mux.Handle("POST /v1/admin/partners/{id}/float/topups", op(s.adminProposePartnerFloatTopup))
	mux.Handle("POST /v1/admin/partners/{id}/float/topups/{topup_id}/approve", op(s.adminApprovePartnerFloatTopup))
	mux.Handle("POST /v1/admin/partners/{id}/float/topups/{topup_id}/reject", op(s.adminRejectPartnerFloatTopup))
}

func (s *Server) partnerAuthed(scope string) func(http.HandlerFunc) http.Handler {
	return func(h http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.requirePartnerAPI(w) {
				return
			}
			p, err := s.Auth.Authenticate(r)
			if err != nil || !p.IsPartner() {
				if ok, wait := s.authFails.Allow(s.clientIP(r)); !ok {
					tooMany(w, wait)
					return
				}
				writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
				return
			}
			if !p.HasScope(scope) {
				writeErr(w, 403, "SCOPE_DENIED", "missing required scope")
				return
			}
			if s.partnerLim != nil {
				if ok, wait := s.partnerLim.Allow(p.PartnerID); !ok {
					tooMany(w, wait)
					return
				}
			}
			if s.Partners != nil && s.Partners.Repo != nil {
				now := time.Now().UTC()
				if kid, err := uuid.Parse(p.PartnerKeyID); err == nil {
					_ = s.Partners.Repo.TouchKeyUsed(r.Context(), kid, now)
				}
				if pid, err := uuid.Parse(p.PartnerID); err == nil {
					_ = s.Partners.Repo.IncrementUsage(r.Context(), pid, now, 1, 0)
				}
			}
			h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller{Principal: p})))
		})
	}
}

func partnerView(p partners.Partner) map[string]any {
	out := map[string]any{
		"id":                     p.ID.String(),
		"organisation":           p.Organisation,
		"status":                 p.Status,
		"environment":            p.Environment,
		"contact_ref":            p.ContactRef,
		"daily_value_cap_minor":  p.DailyValueCapMinor,
		"rate_limit_per_minute":  p.RateLimitPerMinute,
		"created_by":             p.CreatedBy,
		"created_at":             p.CreatedAt.UTC().Format(time.RFC3339),
		"proposed_activation_by": p.ProposedActivationBy,
		"activated_by":           p.ActivatedBy,
		"suspended_by":           p.SuspendedBy,
	}
	if p.ProposedActivationAt != nil {
		out["proposed_activation_at"] = p.ProposedActivationAt.UTC().Format(time.RFC3339)
	}
	if p.ActivatedAt != nil {
		out["activated_at"] = p.ActivatedAt.UTC().Format(time.RFC3339)
	}
	if p.SuspendedAt != nil {
		out["suspended_at"] = p.SuspendedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func keyView(k partners.APIKey) map[string]any {
	out := map[string]any{
		"id":          k.ID.String(),
		"partner_id":  k.PartnerID.String(),
		"prefix":      k.Prefix,
		"environment": k.Environment,
		"scopes":      k.Scopes,
		"created_by":  k.CreatedBy,
		"created_at":  k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if k.LastUsedAt != nil {
		out["last_used_at"] = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if k.ExpiresAt != nil {
		out["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.RevokedAt != nil {
		out["revoked_at"] = k.RevokedAt.UTC().Format(time.RFC3339)
	}
	if k.OverlapUntil != nil {
		out["overlap_until"] = k.OverlapUntil.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) partnerMe(w http.ResponseWriter, r *http.Request) {
	p := callerOf(r).Principal
	writeJSON(w, 200, map[string]any{
		"partner_id":   p.PartnerID,
		"organisation": p.DisplayName,
		"environment":  p.PartnerEnv,
		"scopes":       p.Scopes,
		"key_prefix":   p.PartnerPrefix,
	})
}

func (s *Server) parsePartnerID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) adminCreatePartner(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	var body struct {
		Reason             string `json:"reason"`
		Organisation       string `json:"organisation"`
		Environment        string `json:"environment"`
		ContactRef         string `json:"contact_ref"`
		DailyValueCapMinor int64  `json:"daily_value_cap_minor"`
		RateLimitPerMinute int    `json:"rate_limit_per_minute"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	repo := s.O.Store().Repos()
	p, err := s.Partners.Create(r.Context(), repo.Partners(), actor.UserID, body.Organisation, body.Environment, body.ContactRef, body.DailyValueCapMinor, body.RateLimitPerMinute)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if err := repo.Audit().Add(r.Context(), actor.Event("partner.created", "partner", p.ID, map[string]any{
		"organisation": p.Organisation, "environment": p.Environment, "status": p.Status,
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, partnerView(p))
}

func (s *Server) adminListPartners(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.O.Store().Repos().Partners().ListPartners(r.Context(), limit)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, partnerView(p))
	}
	writeJSON(w, 200, map[string]any{"partners": out})
}

func (s *Server) adminGetPartner(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	p, err := s.O.Store().Repos().Partners().GetPartner(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, partnerView(p))
}

func (s *Server) adminActivatePartner(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	repo := s.O.Store().Repos()
	p, err := s.Partners.Activate(r.Context(), repo.Partners(), actor.UserID, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	action := "partner.activated"
	if p.Status == partners.StatusPending {
		action = "partner.activation.proposed"
	}
	if err := repo.Audit().Add(r.Context(), actor.Event(action, "partner", p.ID, map[string]any{
		"status": p.Status, "environment": p.Environment,
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, partnerView(p))
}

func (s *Server) adminSuspendPartner(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	repo := s.O.Store().Repos()
	p, err := s.Partners.Suspend(r.Context(), repo.Partners(), actor.UserID, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if err := repo.Audit().Add(r.Context(), actor.Event("partner.suspended", "partner", p.ID, map[string]any{
		"status": p.Status,
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, partnerView(p))
}

func (s *Server) adminIssuePartnerKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	var body struct {
		Reason    string   `json:"reason"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	var expires *time.Time
	if strings.TrimSpace(body.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "expires_at must be RFC 3339")
			return
		}
		expires = &t
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	repo := s.O.Store().Repos()
	plain, key, err := s.Partners.Issue(r.Context(), repo.Partners(), actor.UserID, id, body.Scopes, expires)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if err := repo.Audit().Add(r.Context(), actor.Event("partner.key.issued", "partner_api_key", key.ID, map[string]any{
		"partner_id": id.String(), "prefix": key.Prefix, "scopes": key.Scopes, "environment": key.Environment,
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	view := keyView(key)
	view["secret"] = plain
	writeJSON(w, 201, view)
}

func (s *Server) adminListPartnerKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	if _, err := s.O.Store().Repos().Partners().GetPartner(r.Context(), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	keys, err := s.O.Store().Repos().Partners().ListKeys(r.Context(), id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView(k))
	}
	writeJSON(w, 200, map[string]any{"keys": out})
}

func (s *Server) adminRotatePartnerKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	var body struct {
		Reason         string `json:"reason"`
		OverlapSeconds int    `json:"overlap_seconds"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	keyID, err := uuid.Parse(r.PathValue("key_id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	overlap := time.Duration(0)
	if body.OverlapSeconds > 0 {
		overlap = time.Duration(body.OverlapSeconds) * time.Second
		if overlap > 7*24*time.Hour {
			overlap = 7 * 24 * time.Hour
		}
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	repo := s.O.Store().Repos()
	plain, key, err := s.Partners.Rotate(r.Context(), repo.Partners(), actor.UserID, id, keyID, nil, overlap)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if err := repo.Audit().Add(r.Context(), actor.Event("partner.key.rotated", "partner_api_key", key.ID, map[string]any{
		"partner_id": id.String(), "prefix": key.Prefix, "rotated_from": keyID.String(),
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	view := keyView(key)
	view["secret"] = plain
	writeJSON(w, 201, view)
}

func (s *Server) adminRevokePartnerKey(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) || !s.requireAction(w, r, security.ActionPartnerAdmin) {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	keyID, err := uuid.Parse(r.PathValue("key_id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	repo := s.O.Store().Repos()
	key, err := s.Partners.Revoke(r.Context(), repo.Partners(), actor.UserID, id, keyID)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if err := repo.Audit().Add(r.Context(), actor.Event("partner.key.revoked", "partner_api_key", key.ID, map[string]any{
		"partner_id": id.String(), "prefix": key.Prefix,
	})); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, keyView(key))
}

func (s *Server) adminPartnerUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requirePartnerAPI(w) {
		return
	}
	id, ok := s.parsePartnerID(w, r)
	if !ok {
		return
	}
	if _, err := s.O.Store().Repos().Partners().GetPartner(r.Context(), id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	from, to := now.AddDate(0, 0, -30), now
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "from must be YYYY-MM-DD")
			return
		}
		from = t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeErr(w, 400, "INVALID_REQUEST", "to must be YYYY-MM-DD")
			return
		}
		to = t
	}
	rows, err := s.O.Store().Repos().Partners().ListUsage(r.Context(), id, from, to)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, map[string]any{
			"day":           u.Day.Format("2006-01-02"),
			"request_count": u.RequestCount,
			"value_minor":   u.ValueMinor,
		})
	}
	writeJSON(w, 200, map[string]any{"usage": out})
}
