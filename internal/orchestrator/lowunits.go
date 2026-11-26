package orchestrator

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/store"
)

const (
	lowUnitKWh      = 5.0
	lowUnitHours    = 12.0
	lowUnitCooldown = 24 * time.Hour
	lowUnitLookback = 7 * 24 * time.Hour
)

func isLowUnits(u MeterUsage) bool {
	if u.RemainingKWh == nil {
		return false
	}
	if *u.RemainingKWh <= lowUnitKWh {
		return true
	}
	if u.EstimatedHoursRemaining != nil && *u.EstimatedHoursRemaining <= lowUnitHours {
		return true
	}
	return false
}

// PollLowUnits emits at most one meter.low_units outbox event per user+meter
// per 24h when remote remaining units are low. Claim + event are one InTx.
func (o *Orchestrator) PollLowUnits(ctx context.Context) (int, error) {
	now := o.Now().UTC()
	ids, err := o.st.Repos().Readings().ListDistinctMeterIDs(ctx, now.Add(-lowUnitLookback))
	if err != nil {
		return 0, err
	}
	notified := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return notified, ctx.Err()
		}
		n, err := o.pollLowUnitsMeter(ctx, id, now)
		if err != nil {
			o.log.Warn("low-unit poll failed", "meter", id, "err", err)
			continue
		}
		notified += n
	}
	return notified, nil
}

func (o *Orchestrator) pollLowUnitsMeter(ctx context.Context, meterID uuid.UUID, now time.Time) (int, error) {
	m, err := o.Meters.Get(ctx, meterID)
	if err != nil {
		return 0, err
	}
	if !m.Capabilities.RemoteReading.Supported {
		return 0, nil
	}
	u := o.usageForMeter(ctx, "", m)
	if !isLowUnits(u) {
		return 0, nil
	}
	users, err := o.st.Repos().Meters().ListActiveUserIDs(ctx, meterID)
	if err != nil {
		return 0, err
	}
	remainingMilli := int64(*u.RemainingKWh * 1000)
	n := 0
	for _, uid := range users {
		claimed := false
		err := o.st.InTx(ctx, func(r store.Repos) error {
			ok, err := r.LowUnitAlerts().Claim(ctx, meterID, uid, remainingMilli, now, lowUnitCooldown)
			if err != nil || !ok {
				return err
			}
			claimed = true
			payload := map[string]any{"user_id": uid, "remaining_kwh": *u.RemainingKWh}
			if u.EstimatedHoursRemaining != nil {
				payload["estimated_hours_remaining"] = *u.EstimatedHoursRemaining
			}
			return r.Outbox().Add(ctx, events.NewForAggregate("meter", "meter.low_units", meterID, payload))
		})
		if err != nil {
			return n, err
		}
		if claimed {
			n++
		}
	}
	return n, nil
}
