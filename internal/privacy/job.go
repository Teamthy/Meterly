package privacy

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/observability"
)

// Result is counts only — never identifiers.
type Result struct {
	DryRun bool
	Counts map[string]int
}

// Run applies DELETE/ANONYMISE policies in batches. Financial ANONYMISE is a
// no-op until B3. Dry-run counts without mutating. A second run is a no-op
// once everything eligible is gone. Rows for users on an active legal hold
// are skipped.
func Run(ctx context.Context, repo Repo, settings Settings, metrics *observability.Registry, now time.Time, log *slog.Logger) (Result, error) {
	settings = settings.withDefaults()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if log == nil {
		log = slog.Default()
	}
	res := Result{DryRun: settings.DryRun, Counts: map[string]int{}}
	started := now
	runID := uuid.New()

	held, err := repo.ActiveHeldUserIDs(ctx)
	if err != nil {
		return res, err
	}

	for _, p := range Policies(settings) {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		cutoff := now.Add(-p.Duration)
		n, err := apply(ctx, repo, p, cutoff, held, settings.DryRun)
		if err != nil {
			_ = repo.RecordRetentionRun(ctx, failedRun(runID, started, settings.DryRun, res.Counts, err.Error()))
			return res, err
		}
		if n > 0 {
			res.Counts[p.Table] = n
			if metrics != nil {
				metrics.AddRetentionPurged(p.Table, n)
			}
		}
	}

	fin := time.Now().UTC()
	if err := repo.RecordRetentionRun(ctx, RetentionRun{
		ID:         runID,
		StartedAt:  started,
		FinishedAt: &fin,
		DryRun:     settings.DryRun,
		Status:     "COMPLETED",
		Counts:     res.Counts,
	}); err != nil {
		return res, err
	}
	log.Info("retention run", "id", runID, "dry_run", res.DryRun, "counts", res.Counts)
	return res, nil
}

func apply(ctx context.Context, repo Repo, p Policy, cutoff time.Time, held map[string]bool, dryRun bool) (int, error) {
	if p.Financial {
		return 0, nil
	}
	switch p.Table {
	case "channel_otps":
		return repo.PurgeChannelOTPs(ctx, cutoff, p.BatchSize, dryRun, held)
	case "auth_challenges":
		return repo.PurgeAuthChallenges(ctx, cutoff, p.BatchSize, dryRun, held)
	case "auth_sessions":
		return repo.PurgeAuthSessions(ctx, cutoff, p.BatchSize, dryRun, held)
	case "idempotency_keys":
		return repo.PurgeIdempotencyKeys(ctx, cutoff, p.BatchSize, dryRun)
	case "risk_observations":
		return repo.PurgeRiskObservations(ctx, cutoff, p.BatchSize, dryRun, held)
	case "support_threads":
		return repo.PurgeClosedSupportThreads(ctx, cutoff, p.BatchSize, dryRun, held)
	case "notification_deliveries":
		if dryRun {
			return repo.CountPurgeableDeliveryHMACs(ctx, cutoff)
		}
		return repo.PurgeDeliveryHMACs(ctx, cutoff)
	default:
		return 0, nil
	}
}

func failedRun(id uuid.UUID, started time.Time, dryRun bool, counts map[string]int, errText string) RetentionRun {
	fin := time.Now().UTC()
	if counts == nil {
		counts = map[string]int{}
	}
	return RetentionRun{
		ID: id, StartedAt: started, FinishedAt: &fin,
		DryRun: dryRun, Status: "FAILED", Counts: counts, Error: errText,
	}
}
