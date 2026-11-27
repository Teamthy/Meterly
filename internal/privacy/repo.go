package privacy

import (
	"context"
	"time"
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
}
