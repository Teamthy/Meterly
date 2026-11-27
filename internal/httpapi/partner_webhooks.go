package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
)

func (s *Server) partnerWebhookRoutes(mux *patternMux) {
	wh := s.partnerAuthed(partners.ScopeWebhooksManage)
	mux.Handle("POST /v1/partner/webhooks", wh(s.partnerCreateWebhook))
	mux.Handle("GET /v1/partner/webhooks", wh(s.partnerListWebhooks))
	mux.Handle("GET /v1/partner/webhooks/{id}", wh(s.partnerGetWebhook))
	mux.Handle("PATCH /v1/partner/webhooks/{id}", wh(s.partnerUpdateWebhook))
	mux.Handle("DELETE /v1/partner/webhooks/{id}", wh(s.partnerDeleteWebhook))
	mux.Handle("POST /v1/partner/webhooks/{id}/rotate", wh(s.partnerRotateWebhook))
	mux.Handle("POST /v1/partner/webhooks/{id}/ping", wh(s.partnerPingWebhook))
	mux.Handle("GET /v1/partner/webhooks/{id}/deliveries", wh(s.partnerListWebhookDeliveries))
	mux.Handle("POST /v1/partner/webhooks/{id}/deliveries/{delivery_id}/redeliver", wh(s.partnerRedeliverWebhook))
}

func webhookView(ep partners.WebhookEndpoint, secret string) map[string]any {
	out := map[string]any{
		"id":            ep.ID.String(),
		"partner_id":    ep.PartnerID.String(),
		"url":           ep.URL,
		"description":   ep.Description,
		"events":        ep.Events,
		"include_token": ep.IncludeToken,
		"enabled":       ep.Enabled,
		"created_at":    ep.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    ep.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if secret != "" {
		out["secret"] = secret
	}
	if ep.OverlapUntil != nil {
		out["overlap_until"] = ep.OverlapUntil.UTC().Format(time.RFC3339)
	}
	return out
}

func deliveryView(d partners.WebhookDelivery) map[string]any {
	out := map[string]any{
		"id":              d.ID.String(),
		"endpoint_id":     d.EndpointID.String(),
		"event_id":        d.EventID.String(),
		"event_type":      d.EventType,
		"status":          d.Status,
		"attempt_count":   d.AttemptCount,
		"response_code":   d.ResponseCode,
		"error_class":     d.ErrorClass,
		"next_attempt_at": d.NextAttemptAt.UTC().Format(time.RFC3339),
		"created_at":      d.CreatedAt.UTC().Format(time.RFC3339),
	}
	if d.TransactionID != uuid.Nil {
		out["purchase_id"] = d.TransactionID.String()
	}
	if d.FirstAttemptAt != nil {
		out["first_attempt_at"] = d.FirstAttemptAt.UTC().Format(time.RFC3339)
	}
	if d.LastAttemptAt != nil {
		out["last_attempt_at"] = d.LastAttemptAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) parseWebhookID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) partnerCreateWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	var body struct {
		URL          string   `json:"url"`
		Description  string   `json:"description"`
		Events       []string `json:"events"`
		IncludeToken bool     `json:"include_token"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "url and events are required")
		return
	}
	if body.IncludeToken {
		p := callerOf(r).Principal
		if !p.HasScope(partners.ScopePurchasesCreate) && !p.HasScope(partners.ScopePurchasesRead) {
			writeErr(w, 403, "SCOPE_DENIED", "include_token requires purchases:create or purchases:read")
			return
		}
	}
	if s.Partners == nil {
		writeErr(w, 500, "INTERNAL_ERROR", "internal error")
		return
	}
	ep, secret, err := s.Partners.CreateWebhook(r.Context(), nil, pid, body.URL, body.Description, body.Events, body.IncludeToken)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, webhookView(ep, secret))
}

func (s *Server) partnerListWebhooks(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	list, err := s.Partners.ListWebhooks(r.Context(), nil, pid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, ep := range list {
		out = append(out, webhookView(ep, ""))
	}
	writeJSON(w, 200, map[string]any{"endpoints": out})
}

func (s *Server) partnerGetWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	ep, err := s.Partners.GetWebhook(r.Context(), nil, pid, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, webhookView(ep, ""))
}

func (s *Server) partnerUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	var body struct {
		URL          *string  `json:"url"`
		Description  *string  `json:"description"`
		Events       []string `json:"events"`
		IncludeToken *bool    `json:"include_token"`
		Enabled      *bool    `json:"enabled"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	if body.IncludeToken != nil && *body.IncludeToken {
		p := callerOf(r).Principal
		if !p.HasScope(partners.ScopePurchasesCreate) && !p.HasScope(partners.ScopePurchasesRead) {
			writeErr(w, 403, "SCOPE_DENIED", "include_token requires purchases:create or purchases:read")
			return
		}
	}
	ep, err := s.Partners.UpdateWebhook(r.Context(), nil, pid, id, body.URL, body.Description, body.Events, body.IncludeToken, body.Enabled)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, webhookView(ep, ""))
}

func (s *Server) partnerDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	ep, err := s.Partners.DisableWebhook(r.Context(), nil, pid, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, webhookView(ep, ""))
}

func (s *Server) partnerRotateWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	ep, secret, err := s.Partners.RotateWebhook(r.Context(), nil, pid, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, webhookView(ep, secret))
}

func (s *Server) partnerPingWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	d, err := s.Partners.Ping(r.Context(), nil, pid, id)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, deliveryView(d))
}

func (s *Server) partnerListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	list, err := s.Partners.ListDeliveries(r.Context(), nil, pid, id, 50)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, deliveryView(d))
	}
	writeJSON(w, 200, map[string]any{"deliveries": out})
}

func (s *Server) partnerRedeliverWebhook(w http.ResponseWriter, r *http.Request) {
	pid, ok := partnerIDOf(r)
	if !ok {
		writeErr(w, 401, "UNAUTHENTICATED", "authentication required")
		return
	}
	id, ok := s.parseWebhookID(w, r)
	if !ok {
		return
	}
	did, err := uuid.Parse(r.PathValue("delivery_id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	d, err := s.Partners.Redeliver(r.Context(), nil, pid, id, did)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, deliveryView(d))
}
