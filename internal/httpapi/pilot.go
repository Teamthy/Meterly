package httpapi

import (
	"net/http"
	"strings"

	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/pilot"
)

func (s *Server) adminPilotRoutes(mux *patternMux) {
	op, admin := s.authed(auth.RoleOperator, false), s.authed(auth.RoleAdmin, false)
	mux.Handle("GET /v1/admin/pilot", op(s.adminPilotGet))
	mux.Handle("POST /v1/admin/pilot/allowlist", admin(s.adminPilotAdd))
	mux.Handle("DELETE /v1/admin/pilot/allowlist", admin(s.adminPilotRemove))
	mux.Handle("POST /v1/admin/pilot/pause", admin(s.adminPilotPause))
	mux.Handle("POST /v1/admin/pilot/resume", admin(s.adminPilotResume))
}

func (s *Server) adminPilotGet(w http.ResponseWriter, r *http.Request) {
	mode, paused, userCap, globalCap, entries, err := s.O.ListPilot(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"kind": e.Kind, "value": e.Value, "reason": e.Reason,
			"created_at": e.CreatedAt, "created_by": e.CreatedBy,
		})
	}
	writeJSON(w, 200, map[string]any{
		"pilot_mode":             mode,
		"service_paused":         paused,
		"daily_user_cap_minor":   userCap,
		"daily_global_cap_minor": globalCap,
		"allowlist":              out,
	})
}

func (s *Server) adminPilotAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
		Kind   string `json:"kind"`
		Value  string `json:"value"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	kind, ok := pilot.ParseKind(body.Kind)
	if !ok || strings.TrimSpace(body.Value) == "" {
		writeErr(w, 400, "INVALID_REQUEST", "kind must be USER, METER or DISCO and value is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	e := pilot.Entry{Kind: kind, Value: body.Value, Reason: body.Reason}
	if err := s.O.AddPilotEntry(r.Context(), actor, e); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"kind": kind, "value": strings.TrimSpace(body.Value)})
}

func (s *Server) adminPilotRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
		Kind   string `json:"kind"`
		Value  string `json:"value"`
	}
	if !decode(r, &body) || len(body.Reason) < 5 || len(body.Reason) > 500 {
		writeErr(w, 400, "INVALID_REQUEST", "a reason (5–500 characters) is required")
		return
	}
	kind, ok := pilot.ParseKind(body.Kind)
	if !ok || strings.TrimSpace(body.Value) == "" {
		writeErr(w, 400, "INVALID_REQUEST", "kind must be USER, METER or DISCO and value is required")
		return
	}
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorOperator, Reason: body.Reason, CorrelationID: audit.CorrelationFrom(r.Context())}
	if err := s.O.RemovePilotEntry(r.Context(), actor, kind, body.Value); err != nil {
		if err == pilot.ErrNotFound {
			writeErr(w, 404, "NOT_FOUND", "not found")
			return
		}
		s.mapErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminPilotPause(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	if err := s.O.PauseService(r.Context(), actor); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"service_paused": true})
}

func (s *Server) adminPilotResume(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	if err := s.O.ResumeService(r.Context(), actor); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"service_paused": false})
}
