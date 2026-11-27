package privacy

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repo is the persistence surface for the consent ledger, legal holds and
// retention purges. Implementations: memstore and pgstore.
type Repo interface {
	ListConsents(ctx context.Context, userID string) ([]Record, error)
	AppendConsent(ctx context.Context, rec Record) error

	AddLegalHold(ctx context.Context, h Hold) error
	ActiveHeldUserIDs(ctx context.Context) (map[string]bool, error)

	RecordRetentionRun(ctx context.Context, run RetentionRun) error

	PurgeChannelOTPs(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)
	PurgeAuthChallenges(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)
	PurgeAuthSessions(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)
	PurgeIdempotencyKeys(ctx context.Context, cutoff time.Time, batch int, dryRun bool) (int, error)
	PurgeRiskObservations(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)
	PurgeClosedSupportThreads(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)
	CountPurgeableDeliveryHMACs(ctx context.Context, cutoff time.Time) (int, error)
	PurgeDeliveryHMACs(ctx context.Context, cutoff time.Time) (int, error)

	CreateExport(ctx context.Context, job ExportJob) error
	GetExport(ctx context.Context, id uuid.UUID) (ExportJob, error)
	GetExportForUserDay(ctx context.Context, userID, day string) (ExportJob, error)
	UpdateExport(ctx context.Context, job ExportJob) error
	ListPendingExports(ctx context.Context, limit int) ([]ExportJob, error)
	ExpireDueExports(ctx context.Context, now time.Time, batch int) (int, error)
	PurgeDataExports(ctx context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error)

	EnsureTombstone(ctx context.Context) (string, error)
	CreateErasure(ctx context.Context, job ErasureRequest) error
	GetErasure(ctx context.Context, id uuid.UUID) (ErasureRequest, error)
	GetOpenErasureForUser(ctx context.Context, userID string) (ErasureRequest, error)
	UpdateErasure(ctx context.Context, job ErasureRequest) error
	ListDueErasures(ctx context.Context, now time.Time, limit int) ([]ErasureRequest, error)
	CountOpenDisputesForUser(ctx context.Context, userID string) (int, error)
	CountUnresolvedTxnsForUser(ctx context.Context, userID string) (int, error)
	CountActivePotsForUser(ctx context.Context, userID string) (int, error)
	CountUnpaidObligationsForUser(ctx context.Context, userID string) (int, error)
	ApplyErasureStep(ctx context.Context, userID, tombstoneID, step string, now time.Time) error
}
