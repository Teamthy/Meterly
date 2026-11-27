package httpapi

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
)

func (s *Server) supportRoutes(mux *patternMux, user, write func(http.HandlerFunc) http.Handler) {
	mux.Handle("PUT /v1/support/keys", write(s.putSupportKey))
	mux.Handle("GET /v1/support/keys/me", user(s.getMySupportKey))
	mux.Handle("GET /v1/support/roster", user(s.listSupportRoster))
	mux.Handle("POST /v1/support/threads", write(s.createSupportThread))
	mux.Handle("GET /v1/support/threads", user(s.listSupportThreads))
	mux.Handle("GET /v1/support/threads/{thread_id}", user(s.getSupportThread))
	mux.Handle("GET /v1/support/threads/{thread_id}/messages", user(s.listSupportMessages))
	mux.Handle("POST /v1/support/threads/{thread_id}/messages", write(s.postSupportMessage))
	mux.Handle("POST /v1/support/threads/{thread_id}/escalate", write(s.escalateSupportThread))
	mux.Handle("POST /v1/support/threads/{thread_id}/keys", write(s.putSupportThreadKey))
	mux.Handle("POST /v1/support/threads/{thread_id}/close", write(s.closeSupportThread))
}

func (s *Server) adminSupportRoutes(mux *patternMux) {
	op, writeOp := s.authed(auth.RoleOperator, false), s.authed(auth.RoleOperator, true)
	mux.Handle("GET /v1/admin/support/threads", op(s.adminListSupportThreads))
	mux.Handle("GET /v1/admin/support/threads/{thread_id}", op(s.adminGetSupportThread))
	mux.Handle("GET /v1/admin/support/threads/{thread_id}/messages", op(s.adminListSupportMessages))
	mux.Handle("POST /v1/admin/support/threads/{thread_id}/claim", writeOp(s.adminClaimSupportThread))
	mux.Handle("POST /v1/admin/support/threads/{thread_id}/messages", writeOp(s.adminPostSupportMessage))
	mux.Handle("POST /v1/admin/support/threads/{thread_id}/resolve", writeOp(s.adminResolveSupportThread))
}

func (s *Server) requireSupport(w http.ResponseWriter) bool {
	if s.SupportDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "support chat is not enabled")
		return false
	}
	if s.O == nil || s.O.Box() == nil {
		writeErr(w, http.StatusServiceUnavailable, "INTERNAL_ERROR", "internal error")
		return false
	}
	return true
}

func supportNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) supportRepos() store.SupportRepo {
	return s.O.Store().Repos().Support()
}

func (s *Server) putSupportKey(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	var req struct {
		PublicKey string `json:"public_key"`
		Algorithm string `json:"algorithm"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "public_key is required")
		return
	}
	algo := strings.TrimSpace(req.Algorithm)
	if algo == "" {
		algo = support.AlgorithmP256
	}
	if algo != support.AlgorithmP256 {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "algorithm must be P-256")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.PublicKey))
	if err != nil || len(raw) < support.MinPublicKeyBytes || len(raw) > support.MaxPublicKeyBytes {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "public_key must be standard-base64 P-256 SPKI")
		return
	}
	now := s.O.Now().UTC()
	key := support.IdentityKey{
		UserID:    userID(r),
		PublicKey: raw,
		Algorithm: algo,
		Staff:     callerOf(r).Principal.Has(auth.RoleOperator),
		CreatedAt: now,
		RotatedAt: now,
	}
	if err := s.supportRepos().UpsertIdentityKey(r.Context(), key); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, identityJSON(key))
}

func (s *Server) getMySupportKey(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	key, err := s.supportRepos().GetIdentityKey(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, identityJSON(key))
}

func (s *Server) listSupportRoster(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	keys, err := s.supportRepos().ListStaffKeys(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, k := range keys {
		out = append(out, identityJSON(k))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) createSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	now := s.O.Now().UTC()
	th := &support.Thread{
		ID:            uuid.New(),
		CustomerID:    userID(r),
		Mode:          support.ModeAgent,
		Status:        support.StatusOpen,
		CreatedAt:     now,
		UpdatedAt:     now,
		LastMessageAt: now,
	}
	welcomeID := uuid.New()
	bodyEnc, err := s.sealSupport(th.ID, welcomeID, support.Welcome())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	welcome := &support.Message{
		ID: welcomeID, ThreadID: th.ID, SenderID: "agent", SenderKind: support.SenderAgent,
		BodyEnc: bodyEnc, CreatedAt: now,
	}
	err = s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		if err := repos.Support().CreateThread(r.Context(), th); err != nil {
			return err
		}
		return repos.Support().AddMessage(r.Context(), welcome)
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"thread":   threadJSON(*th),
		"messages": []map[string]any{s.messageJSON(welcome)},
	})
}

func (s *Server) listSupportThreads(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	list, err := s.supportRepos().ListThreadsForCustomer(r.Context(), userID(r))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, t := range list {
		out = append(out, threadJSON(*t))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) getSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnThread(w, r)
	if !ok {
		return
	}
	s.writeThreadWithKeys(w, r, t)
}

func (s *Server) listSupportMessages(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnThread(w, r)
	if !ok {
		return
	}
	s.writeMessages(w, r, t.ID)
}

func (s *Server) postSupportMessage(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnThread(w, r)
	if !ok {
		return
	}
	if t.Closed() {
		writeErr(w, http.StatusConflict, "INVALID_REQUEST", "this conversation is closed")
		return
	}
	var req struct {
		Body       string `json:"body"`
		Ciphertext string `json:"ciphertext"`
		Nonce      string `json:"nonce"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "body or ciphertext is required")
		return
	}
	now := s.O.Now().UTC()

	if t.Mode == support.ModeHuman {
		ct, n, err := decodeE2E(req.Ciphertext, req.Nonce)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "ciphertext and nonce must be standard-base64")
			return
		}
		msg := &support.Message{
			ID: uuid.New(), ThreadID: t.ID, SenderID: userID(r), SenderKind: support.SenderCustomer,
			Ciphertext: ct, Nonce: n, CreatedAt: now,
		}
		t.LastMessageAt, t.UpdatedAt = now, now
		if err := s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
			if err := repos.Support().AddMessage(r.Context(), msg); err != nil {
				return err
			}
			return repos.Support().UpdateThread(r.Context(), t)
		}); err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, 201, map[string]any{"message": s.messageJSON(msg)})
		return
	}

	body := strings.TrimSpace(req.Body)
	if body == "" || utf8.RuneCountInString(body) > support.MaxBodyRunes {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "body must be 1–2000 characters")
		return
	}
	custID := uuid.New()
	custEnc, err := s.sealSupport(t.ID, custID, body)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	custMsg := &support.Message{
		ID: custID, ThreadID: t.ID, SenderID: userID(r), SenderKind: support.SenderCustomer,
		BodyEnc: custEnc, CreatedAt: now,
	}
	txns := s.ownTxnSummaries(r, userID(r))
	reply := support.Answer(body, txns)
	agentID := uuid.New()
	agentEnc, err := s.sealSupport(t.ID, agentID, reply.Body)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	agentAt := now.Add(time.Millisecond)
	agentMsg := &support.Message{
		ID: agentID, ThreadID: t.ID, SenderID: "agent", SenderKind: support.SenderAgent,
		BodyEnc: agentEnc, CreatedAt: agentAt,
	}
	t.LastMessageAt, t.UpdatedAt = agentAt, agentAt
	if err := s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		if err := repos.Support().AddMessage(r.Context(), custMsg); err != nil {
			return err
		}
		if err := repos.Support().AddMessage(r.Context(), agentMsg); err != nil {
			return err
		}
		return repos.Support().UpdateThread(r.Context(), t)
	}); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"message":       s.messageJSON(custMsg),
		"agent_message": s.messageJSON(agentMsg),
		"suggest_human": reply.SuggestHuman,
	})
}

func (s *Server) escalateSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnThread(w, r)
	if !ok {
		return
	}
	if t.Closed() {
		writeErr(w, http.StatusConflict, "INVALID_REQUEST", "this conversation is closed")
		return
	}
	var req struct {
		WrappedKeys []struct {
			HolderUserID string `json:"holder_user_id"`
			WrappedKey   string `json:"wrapped_key"`
			Nonce        string `json:"nonce"`
		} `json:"wrapped_keys"`
	}
	if r.ContentLength > 0 {
		_ = decode(r, &req)
	}
	now := s.O.Now().UTC()
	sysID := uuid.New()
	notice := "You're now in an end-to-end encrypted chat with Meterly Support. Meterly cannot read these messages. Keys stay on your device."
	sysEnc, err := s.sealSupport(t.ID, sysID, notice)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	sysMsg := &support.Message{
		ID: sysID, ThreadID: t.ID, SenderID: "system", SenderKind: support.SenderSystem,
		BodyEnc: sysEnc, CreatedAt: now,
	}
	t.Mode = support.ModeHuman
	t.Status = support.StatusWaitingHuman
	t.UpdatedAt, t.LastMessageAt = now, now
	actor := audit.Actor{UserID: userID(r), Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
	err = s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		cur, err := repos.Support().GetThreadForUpdate(r.Context(), t.ID)
		if err != nil {
			return err
		}
		if cur.Closed() {
			return store.ErrConflict
		}
		t.AssignedOperatorID = cur.AssignedOperatorID
		if err := repos.Support().UpdateThread(r.Context(), t); err != nil {
			return err
		}
		if err := repos.Support().AddMessage(r.Context(), sysMsg); err != nil {
			return err
		}
		for _, wk := range req.WrappedKeys {
			raw, n, err := decodeE2E(wk.WrappedKey, wk.Nonce)
			if err != nil || strings.TrimSpace(wk.HolderUserID) == "" {
				continue
			}
			if err := repos.Support().UpsertWrappedKey(r.Context(), support.WrappedKey{
				ThreadID: t.ID, HolderUserID: strings.TrimSpace(wk.HolderUserID),
				WrappedKey: raw, Nonce: n, CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		return repos.Audit().Add(r.Context(), actor.Event("support.escalate", "support_threads", t.ID, map[string]any{"mode": "HUMAN"}))
	})
	if err != nil {
		if err == store.ErrConflict {
			writeErr(w, http.StatusConflict, "INVALID_REQUEST", "this conversation is closed")
			return
		}
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"thread": threadJSON(*t), "message": s.messageJSON(sysMsg)})
}

func (s *Server) putSupportThreadKey(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnOrStaffThread(w, r, false)
	if !ok {
		return
	}
	var req struct {
		HolderUserID string `json:"holder_user_id"`
		WrappedKey   string `json:"wrapped_key"`
		Nonce        string `json:"nonce"`
	}
	if !decode(r, &req) || strings.TrimSpace(req.HolderUserID) == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "holder_user_id, wrapped_key and nonce are required")
		return
	}
	raw, n, err := decodeE2E(req.WrappedKey, req.Nonce)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "wrapped_key and nonce must be standard-base64")
		return
	}
	now := s.O.Now().UTC()
	if err := s.supportRepos().UpsertWrappedKey(r.Context(), support.WrappedKey{
		ThreadID: t.ID, HolderUserID: strings.TrimSpace(req.HolderUserID),
		WrappedKey: raw, Nonce: n, CreatedAt: now,
	}); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) closeSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnThread(w, r)
	if !ok {
		return
	}
	now := s.O.Now().UTC()
	t.Status = support.StatusClosed
	t.UpdatedAt = now
	if err := s.supportRepos().UpdateThread(r.Context(), t); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, threadJSON(*t))
}

func (s *Server) adminListSupportThreads(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	status := support.ThreadStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	list, err := s.supportRepos().ListThreadsForStaff(r.Context(), status, support.MaxListLimit)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, t := range list {
		out = append(out, threadJSON(*t))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) adminGetSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnOrStaffThread(w, r, true)
	if !ok {
		return
	}
	s.writeThreadWithKeys(w, r, t)
}

func (s *Server) adminListSupportMessages(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnOrStaffThread(w, r, true)
	if !ok {
		return
	}
	s.writeMessages(w, r, t.ID)
}

func (s *Server) adminClaimSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	a, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, ok := parseThreadID(w, r)
	if !ok {
		return
	}
	now := s.O.Now().UTC()
	var out *support.Thread
	err := s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		t, err := repos.Support().GetThreadForUpdate(r.Context(), id)
		if err != nil {
			return err
		}
		if t.Mode != support.ModeHuman || (t.Status != support.StatusWaitingHuman && t.Status != support.StatusAssigned) {
			return store.ErrConflict
		}
		if t.Status == support.StatusAssigned && t.AssignedOperatorID != "" && t.AssignedOperatorID != userID(r) {
			return store.ErrConflict
		}
		t.Status = support.StatusAssigned
		t.AssignedOperatorID = userID(r)
		t.UpdatedAt = now
		if err := repos.Support().UpdateThread(r.Context(), t); err != nil {
			return err
		}
		if err := repos.Audit().Add(r.Context(), a.Event("support.claim", "support_threads", t.ID, map[string]any{})); err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		if err == store.ErrConflict {
			writeErr(w, http.StatusConflict, "INVALID_REQUEST", "thread is not waiting for an operator")
			return
		}
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, threadJSON(*out))
}

func (s *Server) adminPostSupportMessage(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	t, ok := s.loadOwnOrStaffThread(w, r, true)
	if !ok {
		return
	}
	if !t.AcceptsHuman() {
		writeErr(w, http.StatusConflict, "INVALID_REQUEST", "thread is not in an encrypted human conversation")
		return
	}
	var req struct {
		Ciphertext string `json:"ciphertext"`
		Nonce      string `json:"nonce"`
	}
	if !decode(r, &req) {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "ciphertext and nonce are required")
		return
	}
	ct, n, err := decodeE2E(req.Ciphertext, req.Nonce)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "ciphertext and nonce must be standard-base64")
		return
	}
	now := s.O.Now().UTC()
	msg := &support.Message{
		ID: uuid.New(), ThreadID: t.ID, SenderID: userID(r), SenderKind: support.SenderOperator,
		Ciphertext: ct, Nonce: n, CreatedAt: now,
	}
	t.LastMessageAt, t.UpdatedAt = now, now
	if t.AssignedOperatorID == "" {
		t.AssignedOperatorID = userID(r)
		t.Status = support.StatusAssigned
	}
	if err := s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		if err := repos.Support().AddMessage(r.Context(), msg); err != nil {
			return err
		}
		return repos.Support().UpdateThread(r.Context(), t)
	}); err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"message": s.messageJSON(msg)})
}

func (s *Server) adminResolveSupportThread(w http.ResponseWriter, r *http.Request) {
	supportNoStore(w)
	if !s.requireSupport(w) {
		return
	}
	a, ok := s.actor(w, r)
	if !ok {
		return
	}
	id, ok := parseThreadID(w, r)
	if !ok {
		return
	}
	now := s.O.Now().UTC()
	var out *support.Thread
	err := s.O.Store().InTx(r.Context(), func(repos store.Repos) error {
		t, err := repos.Support().GetThreadForUpdate(r.Context(), id)
		if err != nil {
			return err
		}
		t.Status = support.StatusResolved
		t.UpdatedAt = now
		if err := repos.Support().UpdateThread(r.Context(), t); err != nil {
			return err
		}
		if err := repos.Audit().Add(r.Context(), a.Event("support.resolve", "support_threads", t.ID, map[string]any{})); err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, threadJSON(*out))
}

func (s *Server) loadOwnThread(w http.ResponseWriter, r *http.Request) (*support.Thread, bool) {
	id, ok := parseThreadID(w, r)
	if !ok {
		return nil, false
	}
	t, err := s.supportRepos().GetThread(r.Context(), id)
	if err != nil || t.CustomerID != userID(r) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return nil, false
	}
	return t, true
}

func (s *Server) loadOwnOrStaffThread(w http.ResponseWriter, r *http.Request, staff bool) (*support.Thread, bool) {
	id, ok := parseThreadID(w, r)
	if !ok {
		return nil, false
	}
	t, err := s.supportRepos().GetThread(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return nil, false
	}
	if staff && callerOf(r).Principal.Has(auth.RoleOperator) {
		return t, true
	}
	if t.CustomerID == userID(r) {
		return t, true
	}
	writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	return nil, false
}

func parseThreadID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("thread_id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) writeThreadWithKeys(w http.ResponseWriter, r *http.Request, t *support.Thread) {
	wraps, err := s.supportRepos().ListWrappedKeys(r.Context(), t.ID)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	keys := []map[string]any{}
	me := userID(r)
	for _, wk := range wraps {
		if wk.HolderUserID != me {
			continue
		}
		keys = append(keys, map[string]any{
			"holder_user_id": wk.HolderUserID,
			"wrapped_key":    base64.StdEncoding.EncodeToString(wk.WrappedKey),
			"nonce":          base64.StdEncoding.EncodeToString(wk.Nonce),
		})
	}
	out := map[string]any{"thread": threadJSON(*t), "wrapped_keys": keys, "me": me}
	if ck, err := s.supportRepos().GetIdentityKey(r.Context(), t.CustomerID); err == nil {
		out["customer_public_key"] = base64.StdEncoding.EncodeToString(ck.PublicKey)
	}
	if t.AssignedOperatorID != "" {
		if ok, err := s.supportRepos().GetIdentityKey(r.Context(), t.AssignedOperatorID); err == nil {
			out["assigned_operator_public_key"] = base64.StdEncoding.EncodeToString(ok.PublicKey)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) writeMessages(w http.ResponseWriter, r *http.Request, threadID uuid.UUID) {
	after := time.Time{}
	if v := strings.TrimSpace(r.URL.Query().Get("after")); v != "" {
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, v)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "after must be RFC3339")
			return
		}
		after = parsed.UTC()
	}
	list, err := s.supportRepos().ListMessages(r.Context(), threadID, after, support.MaxListLimit)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, m := range list {
		out = append(out, s.messageJSON(m))
	}
	writeJSON(w, 200, map[string]any{"data": out})
}

func (s *Server) ownTxnSummaries(r *http.Request, uid string) []support.TxnSummary {
	list, err := s.O.ListForCustomer(r.Context(), uid)
	if err != nil {
		return nil
	}
	out := make([]support.TxnSummary, 0, len(list))
	for _, t := range list {
		out = append(out, support.TxnSummary{
			ID: t.ID.String(), Status: string(t.Status()), Payment: string(t.Payment),
			Vending: string(t.Vending), Delivery: string(t.Delivery), DeliveryMethod: string(t.DeliveryMethod),
			AmountMinor: t.Amount.Minor, Currency: string(t.Total.Currency), IsGift: t.IsGift,
			CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func (s *Server) sealSupport(threadID, msgID uuid.UUID, body string) ([]byte, error) {
	return s.O.Box().EncryptBound(body, []byte("support-msg:"+threadID.String()+":"+msgID.String()))
}

func (s *Server) messageJSON(m *support.Message) map[string]any {
	out := map[string]any{
		"id": m.ID, "thread_id": m.ThreadID, "sender_id": m.SenderID,
		"sender_kind": m.SenderKind, "e2e": m.EndToEnd(), "created_at": m.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if m.EndToEnd() {
		out["ciphertext"] = base64.StdEncoding.EncodeToString(m.Ciphertext)
		out["nonce"] = base64.StdEncoding.EncodeToString(m.Nonce)
		return out
	}
	if len(m.BodyEnc) > 0 && s.O != nil && s.O.Box() != nil {
		if body, err := s.O.Box().DecryptBound(m.BodyEnc, []byte("support-msg:"+m.ThreadID.String()+":"+m.ID.String())); err == nil {
			out["body"] = body
		}
	}
	return out
}

func threadJSON(t support.Thread) map[string]any {
	return map[string]any{
		"id": t.ID, "customer_id": t.CustomerID, "mode": t.Mode, "status": t.Status,
		"assigned_operator_id": t.AssignedOperatorID,
		"e2e":                  t.Mode == support.ModeHuman,
		"created_at":           t.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":           t.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"last_message_at":      t.LastMessageAt.UTC().Format(time.RFC3339Nano),
	}
}

func identityJSON(k support.IdentityKey) map[string]any {
	return map[string]any{
		"user_id":    k.UserID,
		"public_key": base64.StdEncoding.EncodeToString(k.PublicKey),
		"algorithm":  k.Algorithm,
		"staff":      k.Staff,
	}
}

func decodeE2E(ct, nonce string) ([]byte, []byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ct))
	if err != nil || len(raw) == 0 || len(raw) > support.MaxCiphertextBytes {
		return nil, nil, errOrInvalid(err)
	}
	n, err := base64.StdEncoding.DecodeString(strings.TrimSpace(nonce))
	if err != nil || len(n) < 12 || len(n) > 24 {
		return nil, nil, errOrInvalid(err)
	}
	return raw, n, nil
}

type invalidE2E struct{}

func (invalidE2E) Error() string { return "invalid e2e payload" }

func errOrInvalid(err error) error {
	if err != nil {
		return err
	}
	return invalidE2E{}
}
