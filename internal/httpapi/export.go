package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/dataexport"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
)

func (s *Server) exportTTL() time.Duration {
	if s.ExportTTL > 0 {
		return s.ExportTTL
	}
	return privacy.DefaultExportTTL
}

func (s *Server) createDataExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAction(w, r, security.ActionDataExport) {
		return
	}
	uid := userID(r)
	uidUUID, err := uuid.Parse(uid)
	if err != nil {
		writeErr(w, 401, "UNAUTHENTICATED", "not authenticated")
		return
	}
	box := s.O.Box()
	if box == nil {
		writeErr(w, 503, "UNAVAILABLE", "export encryption is not configured")
		return
	}
	now := time.Now().UTC()
	if s.O != nil && s.O.Now != nil {
		now = s.O.Now().UTC()
	}
	day := privacy.ExportDay(now)
	repo := s.O.Store().Repos().Privacy()

	existing, err := repo.GetExportForUserDay(r.Context(), uid, day)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.mapErr(w, r, err)
		return
	}
	if err == nil && existing.Status != privacy.ExportFailed {
		s.writeExportJob(w, existing, false)
		return
	}

	token, err := dataexport.NewDownloadToken()
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	id := uuid.New()
	tokCT, err := dataexport.SealToken(box, id, token)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	job := privacy.ExportJob{
		ID: id, UserID: uid, Day: day, Status: privacy.ExportPending,
		TokenHMAC: dataexport.TokenHMAC(box, token), TokenCiphertext: tokCT,
		TokenKeyVersion: box.KeyVersion, ExpiresAt: now.Add(s.exportTTL()),
		CreatedAt: now, UpdatedAt: now,
	}
	if existing.Status == privacy.ExportFailed {
		job.ID = existing.ID
		tokCT, err = dataexport.SealToken(box, job.ID, token)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		job.TokenCiphertext = tokCT
		job.TokenHMAC = dataexport.TokenHMAC(box, token)
		if err := repo.UpdateExport(r.Context(), job); err != nil {
			s.mapErr(w, r, err)
			return
		}
		s.auditExport(r, uid, uidUUID, job, "data_export.request")
		s.writeExportJob(w, job, true)
		return
	}

	err = s.O.Store().InTx(r.Context(), func(rp store.Repos) error {
		if err := rp.Privacy().CreateExport(r.Context(), job); err != nil {
			return err
		}
		actor := audit.Actor{UserID: uid, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
		return rp.Audit().Add(r.Context(), actor.Event("data_export.request", "data_export", job.ID, map[string]any{
			"status": job.Status, "day": job.Day,
		}))
	})
	if errors.Is(err, store.ErrDuplicate) {
		existing, err = repo.GetExportForUserDay(r.Context(), uid, day)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		s.writeExportJob(w, existing, false)
		return
	}
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	s.writeExportJob(w, job, true)
}

func (s *Server) getDataExport(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnExport(w, r)
	if !ok {
		return
	}
	s.writeExportJob(w, job, false)
}

func (s *Server) downloadDataExport(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadOwnExport(w, r)
	if !ok {
		return
	}
	box := s.O.Box()
	if box == nil {
		writeErr(w, 503, "UNAVAILABLE", "export encryption is not configured")
		return
	}
	now := time.Now().UTC()
	if s.O != nil && s.O.Now != nil {
		now = s.O.Now().UTC()
	}
	if !job.ExpiresAt.After(now) || job.Status == privacy.ExportExpired {
		if job.Status != privacy.ExportExpired {
			job.Status = privacy.ExportExpired
			job.ArchiveCiphertext = nil
			job.TokenCiphertext = nil
			job.UpdatedAt = now
			_ = s.O.Store().Repos().Privacy().UpdateExport(r.Context(), job)
		}
		writeErr(w, 410, "EXPORT_EXPIRED", "this export has expired")
		return
	}
	if job.Status == privacy.ExportDownloaded {
		writeErr(w, 410, "EXPORT_USED", "this download link has already been used")
		return
	}
	if job.Status != privacy.ExportReady || len(job.ArchiveCiphertext) == 0 {
		writeErr(w, 409, "EXPORT_NOT_READY", "the archive is not ready yet")
		return
	}
	token := r.Header.Get("X-Export-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	want := dataexport.TokenHMAC(box, token)
	if token == "" || subtle.ConstantTimeCompare([]byte(want), []byte(job.TokenHMAC)) != 1 {
		writeErr(w, 401, "UNAUTHENTICATED", "invalid export token")
		return
	}
	raw, err := dataexport.OpenArchive(box, job.UserID, job.ID, job.ArchiveCiphertext)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	uid := userID(r)
	uidUUID, _ := uuid.Parse(uid)
	job.Status = privacy.ExportDownloaded
	job.ArchiveCiphertext = nil
	job.TokenCiphertext = nil
	t := now
	job.DownloadedAt = &t
	job.UpdatedAt = now
	if err := s.O.Store().Repos().Privacy().UpdateExport(r.Context(), job); err != nil {
		s.mapErr(w, r, err)
		return
	}
	s.auditExport(r, uid, uidUUID, job, "data_export.download")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (s *Server) loadOwnExport(w http.ResponseWriter, r *http.Request) (privacy.ExportJob, bool) {
	id, err := uuid.Parse(r.PathValue("export_id"))
	if err != nil {
		writeErr(w, 404, "NOT_FOUND", "export not found")
		return privacy.ExportJob{}, false
	}
	job, err := s.O.Store().Repos().Privacy().GetExport(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "NOT_FOUND", "export not found")
			return privacy.ExportJob{}, false
		}
		s.mapErr(w, r, err)
		return privacy.ExportJob{}, false
	}
	if job.UserID != userID(r) {
		writeErr(w, 404, "NOT_FOUND", "export not found")
		return privacy.ExportJob{}, false
	}
	return job, true
}

func (s *Server) writeExportJob(w http.ResponseWriter, job privacy.ExportJob, created bool) {
	body := map[string]any{
		"export_id":  job.ID,
		"status":     job.Status,
		"day":        job.Day,
		"expires_at": job.ExpiresAt.UTC(),
		"created_at": job.CreatedAt.UTC(),
	}
	if job.DownloadedAt != nil {
		body["downloaded_at"] = job.DownloadedAt.UTC()
	}
	if s.O != nil && s.O.Box() != nil && len(job.TokenCiphertext) > 0 &&
		(job.Status == privacy.ExportPending || job.Status == privacy.ExportReady) {
		if tok, err := dataexport.OpenToken(s.O.Box(), job.ID, job.TokenCiphertext); err == nil {
			body["download_token"] = tok
		}
	}
	code := 200
	if created && job.Status == privacy.ExportPending {
		code = 201
	}
	writeJSON(w, code, body)
}

func (s *Server) auditExport(r *http.Request, uid string, _ uuid.UUID, job privacy.ExportJob, action string) {
	actor := audit.Actor{UserID: uid, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(r.Context())}
	_ = s.O.Store().Repos().Audit().Add(r.Context(), actor.Event(action, "data_export", job.ID, map[string]any{
		"status": job.Status, "day": job.Day,
	}))
}
