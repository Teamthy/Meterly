package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/providers"
)

func (s *Server) remoteCreditWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Remote == nil {
		writeErr(w, 404, "NOT_FOUND", "remote credit is not configured")
		return
	}
	if r.PathValue("provider") != s.Remote.Name() {
		writeErr(w, 404, "INVALID_REQUEST", "unknown provider")
		return
	}
	parser, ok := s.Remote.(providers.RemoteCreditWebhookParser)
	if !ok {
		writeErr(w, 404, "NOT_FOUND", "remote credit webhooks are not supported by this provider")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "unreadable body")
		return
	}
	evt, err := parser.ParseWebhook(r.Header, body)
	if errors.Is(err, providers.ErrBadSignature) {
		writeErr(w, 401, "UNAUTHENTICATED", "invalid webhook signature")
		return
	}
	if err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "malformed webhook")
		return
	}
	if !evt.Relevant {
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	if _, err := s.O.HandleRemoteCreditNotification(r.Context(), evt.IdempotencyKey); err != nil {
		s.Log.Warn("remote credit webhook processing", "err", err)
	}
	writeJSON(w, 200, map[string]string{"status": "received"})
}

func (s *Server) adminCreateRemoteCohort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Disco        string `json:"disco"`
		NumberPrefix string `json:"number_prefix"`
		Manufacturer string `json:"manufacturer"`
		Evidence     string `json:"evidence"`
	}
	if !decode(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	actor := audit.Actor{
		UserID: userID(r), Type: audit.ActorOperator, Reason: body.Evidence,
		CorrelationID: audit.CorrelationFrom(r.Context()),
	}
	c, err := s.O.CreateRemoteCohort(r.Context(), actor, body.Disco, body.NumberPrefix, body.Manufacturer, body.Evidence)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) adminListRemoteCohorts(w http.ResponseWriter, r *http.Request) {
	list, err := s.O.ListRemoteCohorts(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []meters.RemoteCohort{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) adminRevokeRemoteCohort(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "not found")
		return
	}
	a, ok := s.actor(w, r)
	if !ok {
		return
	}
	if err := s.O.RevokeRemoteCohort(r.Context(), a, id); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "revoked": true})
}
