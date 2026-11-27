package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
)

func (s *Server) erasureCooling() time.Duration {
	if s.ErasureCooling > 0 {
		return s.ErasureCooling
	}
	return privacy.DefaultErasureCooling
}

func (s *Server) requireErasure(w http.ResponseWriter) bool {
	if s.ErasureDisabled {
		writeErr(w, http.StatusNotFound, "FEATURE_DISABLED", "account erasure is not enabled")
		return false
	}
	return true
}

func erasureView(job privacy.ErasureRequest) map[string]any {
	out := map[string]any{
		"erasure_id": job.ID.String(),
		"status":     job.Status,
		"due_at":     job.DueAt.UTC().Format(time.RFC3339),
	}
	if job.Error != "" {
		out["block_code"] = job.Error
	}
	if job.CancelledAt != nil {
		out["cancelled_at"] = job.CancelledAt.UTC().Format(time.RFC3339)
	}
	if job.FinishedAt != nil {
		out["finished_at"] = job.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) createDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireErasure(w) {
		return
	}
	if !s.requireAction(w, r, security.ActionDeleteAccount) {
		return
	}
	uid := userID(r)
	st, err := s.O.Store().Repos().Users().Status(r.Context(), uid)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if st == store.UserErased {
		writeErr(w, http.StatusConflict, "ACCOUNT_ERASED", "this account has been erased")
		return
	}
	repo := s.O.Store().Repos().Privacy()
	existing, err := repo.GetOpenErasureForUser(r.Context(), uid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.mapErr(w, r, err)
		return
	}
	if err == nil {
		writeJSON(w, 200, erasureView(existing))
		return
	}
	now := time.Now().UTC()
	if s.O != nil && s.O.Now != nil {
		now = s.O.Now().UTC()
	}
	if _, err := repo.EnsureTombstone(r.Context()); err != nil {
		s.mapErr(w, r, err)
		return
	}
	job := privacy.ErasureRequest{
		ID: uuid.New(), UserID: uid, Status: privacy.ErasureCoolingOff,
		DueAt: now.Add(s.erasureCooling()), CreatedAt: now, UpdatedAt: now,
	}
	// Optional JSON reason is accepted and discarded — it would be PII in audit.
	err = s.O.Store().InTx(r.Context(), func(rp store.Repos) error {
		if err := rp.Privacy().CreateErasure(r.Context(), job); err != nil {
			return err
		}
		actor := audit.Actor{UserID: uid, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
		return rp.Audit().Add(r.Context(), actor.Event("erasure.request", "erasure_request", job.ID, map[string]any{
			"status": job.Status,
		}))
	})
	if errors.Is(err, store.ErrDuplicate) {
		existing, err = repo.GetOpenErasureForUser(r.Context(), uid)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		writeJSON(w, 200, erasureView(existing))
		return
	}
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 201, erasureView(job))
}

func (s *Server) cancelDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireErasure(w) {
		return
	}
	uid := userID(r)
	repo := s.O.Store().Repos().Privacy()
	job, err := repo.GetOpenErasureForUser(r.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusConflict, "ERASURE_NOT_PENDING", "no cooling-off deletion to cancel")
			return
		}
		s.mapErr(w, r, err)
		return
	}
	if job.Status != privacy.ErasureCoolingOff {
		writeErr(w, http.StatusConflict, "ERASURE_NOT_PENDING", "deletion can only be cancelled during cooling-off")
		return
	}
	now := time.Now().UTC()
	if s.O != nil && s.O.Now != nil {
		now = s.O.Now().UTC()
	}
	t := now
	job.Status = privacy.ErasureCancelled
	job.CancelledAt = &t
	job.UpdatedAt = now
	err = s.O.Store().InTx(r.Context(), func(rp store.Repos) error {
		if err := rp.Privacy().UpdateErasure(r.Context(), job); err != nil {
			return err
		}
		actor := audit.Actor{UserID: uid, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
		return rp.Audit().Add(r.Context(), actor.Event("erasure.cancel", "erasure_request", job.ID, map[string]any{
			"status": job.Status,
		}))
	})
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeJSON(w, 200, erasureView(job))
}
