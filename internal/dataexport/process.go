package dataexport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
)

func archiveContext(userID string, id uuid.UUID) []byte {
	return []byte("data-export:" + userID + ":" + id.String())
}

func tokenContext(id uuid.UUID) []byte {
	return []byte("export-token:" + id.String())
}

func TokenHMAC(box *secure.Box, token string) string {
	if box == nil {
		return ""
	}
	return box.Hash("export-download:" + token)
}

func NewDownloadToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func SealArchive(box *secure.Box, userID string, id uuid.UUID, raw []byte) ([]byte, error) {
	if box == nil {
		return nil, errors.New("dataexport: encryption box required")
	}
	if len(raw) == 0 {
		return nil, errors.New("dataexport: empty archive")
	}
	return box.EncryptBound(string(raw), archiveContext(userID, id))
}

func OpenArchive(box *secure.Box, userID string, id uuid.UUID, ct []byte) ([]byte, error) {
	if box == nil {
		return nil, errors.New("dataexport: encryption box required")
	}
	s, err := box.DecryptBound(ct, archiveContext(userID, id))
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func SealToken(box *secure.Box, id uuid.UUID, token string) ([]byte, error) {
	if box == nil {
		return nil, errors.New("dataexport: encryption box required")
	}
	return box.EncryptBound(token, tokenContext(id))
}

func OpenToken(box *secure.Box, id uuid.UUID, ct []byte) (string, error) {
	if box == nil {
		return "", errors.New("dataexport: encryption box required")
	}
	return box.DecryptBound(ct, tokenContext(id))
}

// Process expires overdue jobs then builds pending archives. Returns how many
// archives were written.
func Process(ctx context.Context, st store.Store, box *secure.Box, now time.Time, limit int, log *slog.Logger) (int, error) {
	if st == nil {
		return 0, errors.New("dataexport: store required")
	}
	if log == nil {
		log = slog.Default()
	}
	now = now.UTC()
	if limit <= 0 {
		limit = 20
	}
	repo := st.Repos().Privacy()
	if _, err := repo.ExpireDueExports(ctx, now, limit); err != nil {
		return 0, err
	}
	pending, err := repo.ListPendingExports(ctx, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, job := range pending {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if err := buildOne(ctx, st, box, job, now, log); err != nil {
			log.Warn("data export failed", "export_id", job.ID, "err", err)
			job.Status = privacy.ExportFailed
			job.Error = "build_failed"
			job.UpdatedAt = now
			_ = repo.UpdateExport(ctx, job)
			continue
		}
		n++
	}
	return n, nil
}

func buildOne(ctx context.Context, st store.Store, box *secure.Box, job privacy.ExportJob, now time.Time, log *slog.Logger) error {
	if !job.ExpiresAt.After(now) {
		job.Status = privacy.ExportExpired
		job.ArchiveCiphertext = nil
		job.TokenCiphertext = nil
		job.UpdatedAt = now
		return st.Repos().Privacy().UpdateExport(ctx, job)
	}
	if status, err := st.Repos().Users().Status(ctx, job.UserID); err == nil && status == store.UserErased {
		job.Status = privacy.ExportFailed
		job.Error = "account_erased"
		job.ArchiveCiphertext = nil
		job.TokenCiphertext = nil
		job.UpdatedAt = now
		return st.Repos().Privacy().UpdateExport(ctx, job)
	}
	arch, err := Build(ctx, st, box, job.UserID, now)
	if err != nil {
		return err
	}
	raw, err := Marshal(arch)
	if err != nil {
		return err
	}
	ct, err := SealArchive(box, job.UserID, job.ID, raw)
	if err != nil {
		return err
	}
	job.ArchiveCiphertext = ct
	if box != nil {
		job.ArchiveKeyVersion = box.KeyVersion
	}
	job.Status = privacy.ExportReady
	job.Error = ""
	job.UpdatedAt = now
	if err := st.Repos().Privacy().UpdateExport(ctx, job); err != nil {
		return err
	}
	log.Info("data export ready", "export_id", job.ID)
	return nil
}
