package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/security"
)

func (s *Server) securityRoutes(mux *patternMux, user, write func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /v1/security/console", user(s.securityConsole))
	mux.Handle("GET /v1/security/events", user(s.securityEvents))
	mux.Handle("GET /v1/security/sessions", user(s.securitySessions))
	mux.Handle("POST /v1/security/sessions/{id}/revoke", write(s.securityRevokeSession))
	mux.Handle("POST /v1/security/sessions/revoke-others", write(s.securityRevokeOthers))
	mux.Handle("POST /v1/security/step-up/begin", write(s.securityStepUpBegin))
	mux.Handle("POST /v1/security/step-up/finish", write(s.securityStepUpFinish))
}

func (s *Server) passkeySession(r *http.Request) *identity.Session {
	if s.Identity == nil {
		return nil
	}
	tok := s.bearerToken(r)
	if !strings.HasPrefix(tok, identity.SessionPrefix) {
		return nil
	}
	sess, err := s.Identity.LookupSession(r.Context(), tok)
	if err != nil {
		return nil
	}
	return &sess
}

func (s *Server) passkeyCount(r *http.Request, uid string) int {
	if s.Identity == nil {
		return 0
	}
	list, err := s.Identity.ListPasskeys(r.Context(), uid)
	if err != nil {
		return 0
	}
	return len(list)
}

// requireAction enforces the security engine. Nil engine = no extra gate
// (existing tests that don't wire Security keep working).
func (s *Server) requireAction(w http.ResponseWriter, r *http.Request, action string) bool {
	if s.Security == nil {
		return true
	}
	uid := userID(r)
	d := s.Security.Evaluate(r.Context(), security.Request{
		Action:   action,
		UserID:   uid,
		Session:  s.passkeySession(r),
		Passkeys: s.passkeyCount(r, uid),
		IP:       s.clientIP(r),
	})
	if d.Outcome == security.OutcomeAllow {
		return true
	}
	ev := security.Event{
		UserID: uid, Action: action, Outcome: d.Outcome, Reason: d.Reason,
		IPHMAC: s.Security.HashIP(s.clientIP(r)),
	}
	if sess := s.passkeySession(r); sess != nil {
		ev.SessionID = sess.ID
	}
	_ = s.Security.Record(r.Context(), ev)
	if d.Outcome == security.OutcomeDeny {
		code := "FORBIDDEN"
		if action == security.ActionDeletePasskey {
			code = "FORBIDDEN"
		}
		writeErr(w, 403, code, d.Reason)
		return false
	}
	writeErr(w, 403, "STEP_UP_REQUIRED", d.Reason)
	return false
}

func (s *Server) securityConsole(w http.ResponseWriter, r *http.Request) {
	if s.Security == nil {
		writeErr(w, 404, "NOT_FOUND", "security engine is not configured")
		return
	}
	c, err := s.Security.Console(r.Context(), userID(r), s.passkeySession(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) securityEvents(w http.ResponseWriter, r *http.Request) {
	if s.Security == nil {
		writeErr(w, 404, "NOT_FOUND", "security engine is not configured")
		return
	}
	list, err := s.Security.Store.ListSecurityEvents(r.Context(), userID(r), 50)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if list == nil {
		list = []security.Event{}
	}
	writeJSON(w, 200, map[string]any{"data": list})
}

func (s *Server) securitySessions(w http.ResponseWriter, r *http.Request) {
	if s.Security == nil {
		writeErr(w, 404, "NOT_FOUND", "security engine is not configured")
		return
	}
	c, err := s.Security.Console(r.Context(), userID(r), s.passkeySession(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": c.Sessions})
}

func (s *Server) securityRevokeSession(w http.ResponseWriter, r *http.Request) {
	if s.Identity == nil {
		writeErr(w, 404, "NOT_FOUND", "passkeys are not configured")
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "session not found")
		return
	}
	sess, err := s.O.Store().Repos().Auth().GetSession(r.Context(), id)
	if err != nil || sess.UserID != userID(r) {
		writeErr(w, 404, "NOT_FOUND", "session not found")
		return
	}
	if err := s.O.Store().Repos().Auth().RevokeSession(r.Context(), id, time.Now().UTC()); err != nil {
		s.mapErr(w, r, err)
		return
	}
	if s.Security != nil {
		_ = s.Security.Record(r.Context(), security.Event{
			UserID: userID(r), Action: security.ActionRevokeSession, Outcome: security.OutcomeRecorded,
			Reason: "session revoked", SessionID: id,
		})
	}
	writeJSON(w, 200, map[string]any{"revoked": true})
}

func (s *Server) securityRevokeOthers(w http.ResponseWriter, r *http.Request) {
	if s.Security == nil {
		writeErr(w, 404, "NOT_FOUND", "security engine is not configured")
		return
	}
	cur := s.passkeySession(r)
	if cur == nil {
		writeErr(w, 403, "STEP_UP_REQUIRED", "sign in with a passkey to revoke other sessions")
		return
	}
	if !s.requireAction(w, r, security.ActionRevokeOthers) {
		return
	}
	if err := s.Security.RevokeOthers(r.Context(), userID(r), cur.ID); err != nil {
		s.mapErr(w, r, err)
		return
	}
	_ = s.Security.Record(r.Context(), security.Event{
		UserID: userID(r), Action: security.ActionRevokeOthers, Outcome: security.OutcomeRecorded,
		Reason: "other sessions revoked", SessionID: cur.ID,
	})
	writeJSON(w, 200, map[string]any{"revoked_others": true})
}

func (s *Server) securityStepUpBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	var body struct {
		Origin string `json:"origin"`
	}
	_ = decodeJSON(r, &body)
	opt, err := s.Identity.BeginStepUp(r.Context(), requestOrigin(r, body.Origin), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, opt)
}

func (s *Server) securityStepUpFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireIdentity(w) {
		return
	}
	cur := s.passkeySession(r)
	if cur == nil {
		writeErr(w, 401, "UNAUTHENTICATED", "passkey session required")
		return
	}
	var body struct {
		Origin     string                     `json:"origin"`
		Credential identity.AssertionResponse `json:"credential"`
		identity.AssertionResponse
	}
	if !decodeJSON(r, &body) {
		writeErr(w, 400, "INVALID_REQUEST", "invalid request body")
		return
	}
	as := body.Credential
	if as.ClientDataJSON == "" {
		as = body.AssertionResponse
	}
	if err := s.Identity.FinishStepUp(r.Context(), requestOrigin(r, body.Origin), userID(r), as); err != nil {
		if s.authFails != nil {
			if ok, wait := s.authFails.Allow(s.clientIP(r)); !ok {
				tooMany(w, wait)
				return
			}
		}
		s.mapErr(w, r, err)
		return
	}
	if s.Security != nil {
		if err := s.Security.CompleteStepUp(r.Context(), cur.ID); err != nil {
			s.mapErr(w, r, err)
			return
		}
		_ = s.Security.Record(r.Context(), security.Event{
			UserID: userID(r), Action: security.ActionStepUp, Outcome: security.OutcomeAllow,
			Reason: "passkey step-up", SessionID: cur.ID, IPHMAC: s.Security.HashIP(s.clientIP(r)),
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) recordAuthEvent(r *http.Request, uid, action, outcome, reason string, sessionID uuid.UUID) {
	if s.Security == nil {
		return
	}
	_ = s.Security.Record(r.Context(), security.Event{
		UserID: uid, Action: action, Outcome: outcome, Reason: reason, SessionID: sessionID,
		IPHMAC: s.Security.HashIP(s.clientIP(r)),
	})
}
