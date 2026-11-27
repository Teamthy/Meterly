// Package erasure runs the NDPA account-deletion job after cooling-off.
// Financial rows are anonymised onto a tombstone user; identity is deleted.
package erasure

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
)

// ErrBlocked is returned when a job cannot proceed; job.Error holds the code.
var ErrBlocked = errors.New("erasure: blocked")

// Process claims due erasure jobs and runs remaining steps. Each step commits
// on its own so an interrupted run resumes. Returns how many jobs finished.
func Process(ctx context.Context, st store.Store, now time.Time, limit int, log *slog.Logger) (int, error) {
	if st == nil {
		return 0, errors.New("erasure: store required")
	}
	if log == nil {
		log = slog.Default()
	}
	now = now.UTC()
	if limit <= 0 {
		limit = 20
	}
	if _, err := st.Repos().Privacy().EnsureTombstone(ctx); err != nil {
		return 0, err
	}
	due, err := st.Repos().Privacy().ListDueErasures(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, job := range due {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if err := runOne(ctx, st, job, now, log); err != nil {
			if errors.Is(err, ErrBlocked) {
				continue
			}
			log.Warn("erasure job failed", "erasure_id", job.ID, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

func runOne(ctx context.Context, st store.Store, job privacy.ErasureRequest, now time.Time, log *slog.Logger) error {
	repo := st.Repos().Privacy()
	if job.Status == privacy.ErasureCoolingOff {
		if job.DueAt.After(now) {
			return nil
		}
		job.Status = privacy.ErasureReady
		job.UpdatedAt = now
		if err := repo.UpdateErasure(ctx, job); err != nil {
			return err
		}
	}
	if code := blockReason(ctx, repo, job.UserID); code != "" {
		job.Status = privacy.ErasureBlocked
		job.Error = code
		job.UpdatedAt = now
		if err := repo.UpdateErasure(ctx, job); err != nil {
			return err
		}
		_ = st.Repos().Audit().Add(ctx, audit.Actor{Type: audit.ActorSystem}.Event(
			"erasure.blocked", "erasure_request", job.ID, map[string]any{"code": code}))
		return ErrBlocked
	}

	tomb, err := repo.EnsureTombstone(ctx)
	if err != nil {
		return err
	}
	if job.Status != privacy.ErasureRunning {
		t := now
		job.Status = privacy.ErasureRunning
		job.StartedAt = &t
		job.Error = ""
		job.TombstoneID = tomb
		job.UpdatedAt = now
		if err := repo.UpdateErasure(ctx, job); err != nil {
			return err
		}
	}

	start := privacy.StepIndex(job.Step) + 1
	for i := start; i < len(privacy.ErasureSteps); i++ {
		step := privacy.ErasureSteps[i]
		if err := st.InTx(ctx, func(rp store.Repos) error {
			if err := rp.Privacy().ApplyErasureStep(ctx, job.UserID, tomb, step, now); err != nil {
				return err
			}
			job.Step = step
			job.UpdatedAt = now
			return rp.Privacy().UpdateErasure(ctx, job)
		}); err != nil {
			job.Status = privacy.ErasureFailed
			job.Error = step
			job.UpdatedAt = now
			_ = repo.UpdateErasure(ctx, job)
			return err
		}
	}

	if err := st.InTx(ctx, func(rp store.Repos) error {
		if err := rp.Users().SetStatus(ctx, job.UserID, store.UserErased); err != nil {
			return err
		}
		t := now
		job.Status = privacy.ErasureCompleted
		job.FinishedAt = &t
		job.TombstoneID = tomb
		job.Error = ""
		job.UpdatedAt = now
		if err := rp.Privacy().UpdateErasure(ctx, job); err != nil {
			return err
		}
		return rp.Audit().Add(ctx, audit.Actor{Type: audit.ActorSystem}.Event(
			"erasure.completed", "erasure_request", job.ID, map[string]any{"steps": len(privacy.ErasureSteps)}))
	}); err != nil {
		job.Status = privacy.ErasureFailed
		job.Error = "complete"
		job.UpdatedAt = now
		_ = repo.UpdateErasure(ctx, job)
		return err
	}
	log.Info("erasure completed", "erasure_id", job.ID)
	return nil
}

func blockReason(ctx context.Context, repo privacy.Repo, userID string) string {
	if n, err := repo.CountOpenDisputesForUser(ctx, userID); err == nil && n > 0 {
		return privacy.BlockOpenDispute
	}
	if n, err := repo.CountUnresolvedTxnsForUser(ctx, userID); err == nil && n > 0 {
		return privacy.BlockUnresolvedTxn
	}
	if n, err := repo.CountActivePotsForUser(ctx, userID); err == nil && n > 0 {
		return privacy.BlockPotWithFunds
	}
	if n, err := repo.CountUnpaidObligationsForUser(ctx, userID); err == nil && n > 0 {
		return privacy.BlockUnpaidObligation
	}
	return ""
}
