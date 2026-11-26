package memstore

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type lowUnitAlert struct {
	MeterID        uuid.UUID
	UserID         string
	At             time.Time
	RemainingMilli int64
}

type lowUnitRepo struct{ *repos }

func (r lowUnitRepo) Claim(_ context.Context, meterID uuid.UUID, userID string, remainingMilli int64, now time.Time, cooldown time.Duration) (bool, error) {
	defer r.lock()()
	key := meterID.String() + ":" + userID
	now = now.UTC()
	if prev, ok := r.s.lowUnitAlerts[key]; ok {
		if cooldown > 0 && now.Sub(prev.At) < cooldown {
			return false, nil
		}
		old := prev
		r.s.lowUnitAlerts[key] = lowUnitAlert{MeterID: meterID, UserID: userID, At: now, RemainingMilli: remainingMilli}
		r.onUndo(func() { r.s.lowUnitAlerts[key] = old })
		return true, nil
	}
	r.s.lowUnitAlerts[key] = lowUnitAlert{MeterID: meterID, UserID: userID, At: now, RemainingMilli: remainingMilli}
	r.onUndo(func() { delete(r.s.lowUnitAlerts, key) })
	return true, nil
}
