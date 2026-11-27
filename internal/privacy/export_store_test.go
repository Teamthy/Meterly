package privacy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
)

func TestExportJobStoreCRUD(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "export-crud-user")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		repo := st.Repos().Privacy()
		job := privacy.ExportJob{
			ID: uuid.New(), UserID: uid, Day: privacy.ExportDay(now),
			Status: privacy.ExportPending, TokenHMAC: "hmac-export-crud",
			TokenCiphertext: []byte("tok-ct"), TokenKeyVersion: "t1",
			ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.CreateExport(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetExport(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Day != job.Day || got.Status != privacy.ExportPending || got.UserID != uid {
			t.Fatalf("get %+v", got)
		}
		byDay, err := repo.GetExportForUserDay(ctx, uid, job.Day)
		if err != nil || byDay.ID != job.ID {
			t.Fatalf("by day %+v err=%v", byDay, err)
		}
		dup := job
		dup.ID = uuid.New()
		if err := repo.CreateExport(ctx, dup); !errors.Is(err, store.ErrDuplicate) {
			t.Fatalf("want ErrDuplicate, got %v", err)
		}
		pending, err := repo.ListPendingExports(ctx, 10)
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending n=%d err=%v", len(pending), err)
		}
		job.Status = privacy.ExportReady
		job.ArchiveCiphertext = []byte("arch-ct")
		job.ArchiveKeyVersion = "t1"
		job.UpdatedAt = now.Add(time.Second)
		if err := repo.UpdateExport(ctx, job); err != nil {
			t.Fatal(err)
		}
		n, err := repo.ExpireDueExports(ctx, now.Add(25*time.Hour), 10)
		if err != nil || n != 1 {
			t.Fatalf("expire n=%d err=%v", n, err)
		}
		expired, err := repo.GetExport(ctx, job.ID)
		if err != nil || expired.Status != privacy.ExportExpired || len(expired.ArchiveCiphertext) != 0 || len(expired.TokenCiphertext) != 0 {
			t.Fatalf("expired %+v err=%v", expired, err)
		}
		n, err = repo.PurgeDataExports(ctx, now.Add(time.Second), 10, false, nil)
		if err != nil || n != 1 {
			t.Fatalf("purge n=%d err=%v", n, err)
		}
		if _, err := repo.GetExport(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("purged still present: %v", err)
		}
	})
}
