package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/store"
)

type lowUnitRepo struct{ *repos }

func (r lowUnitRepo) Claim(ctx context.Context, meterID uuid.UUID, userID string, remainingMilli int64, now time.Time, cooldown time.Duration) (bool, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return false, store.ErrNotFound
	}
	secs := int(cooldown.Seconds())
	if secs < 0 {
		secs = 0
	}
	var id uuid.UUID
	err = r.db.QueryRow(ctx, `
		INSERT INTO meter_low_unit_alerts (meter_id, user_id, last_notified_at, last_remaining_milli_kwh)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (meter_id, user_id) DO UPDATE
		SET last_notified_at = EXCLUDED.last_notified_at,
		    last_remaining_milli_kwh = EXCLUDED.last_remaining_milli_kwh
		WHERE meter_low_unit_alerts.last_notified_at <= EXCLUDED.last_notified_at - make_interval(secs => $5)
		RETURNING meter_id`,
		meterID, uid, now.UTC(), remainingMilli, secs).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if isFKViolation(err) {
			return false, store.ErrNotFound
		}
		return false, mapErr(err)
	}
	return true, nil
}
