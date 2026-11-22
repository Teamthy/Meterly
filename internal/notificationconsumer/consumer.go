// Package notificationconsumer records held notification candidates without
// sending anything. It runs only when explicitly enabled with a cutoff.
package notificationconsumer

import (
	"context"
	"time"

	"github.com/teamthy/meterly/internal/store"
)

// ConsumeBatch independently scans both published and unpublished outbox
// events created at or after the mandatory cutoff and atomically records
// candidates. It does not alter published_at, invoke a sender, infer consent,
// or decide a channel or destination.
func ConsumeBatch(ctx context.Context, st store.Store, since time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		return 0, ErrInvalidLimit
	}
	if since.IsZero() {
		return 0, ErrInvalidCutoff
	}
	count := 0
	err := st.InTx(ctx, func(r store.Repos) error {
		candidates, err := r.NotificationCandidates().Claim(ctx, since.UTC(), limit)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, c := range candidates {
			c.CreatedAt = now
			if err := r.NotificationCandidates().Record(ctx, c); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
