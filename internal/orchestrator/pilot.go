package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/pilot"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// SetPilot enables PILOT_MODE allow-list + daily caps (kobo, 0 = none).
func (o *Orchestrator) SetPilot(mode bool, userCapMinor, globalCapMinor int64) {
	if o == nil {
		return
	}
	o.pilotMode = mode
	o.pilotUserCap = userCapMinor
	o.pilotGlobalCap = globalCapMinor
}

func (o *Orchestrator) PilotMode() bool { return o != nil && o.pilotMode }

func (o *Orchestrator) PilotCaps() (userMinor, globalMinor int64) {
	if o == nil {
		return 0, 0
	}
	return o.pilotUserCap, o.pilotGlobalCap
}

func (o *Orchestrator) SetServicePaused(paused bool) {
	if o == nil {
		return
	}
	o.servicePaused.Store(paused)
}

func (o *Orchestrator) ServicePaused() bool { return o != nil && o.servicePaused.Load() }

func (o *Orchestrator) ensurePurchasesOpen(ctx context.Context) error {
	if o.ServicePaused() {
		return ErrServicePaused
	}
	return o.ensureVendingOpen(ctx)
}

func (o *Orchestrator) ensurePurchaseAllowed(ctx context.Context, customerID string, meterID uuid.UUID, amountMinor int64) error {
	if err := o.ensurePurchasesOpen(ctx); err != nil {
		return err
	}
	return o.ensurePilot(ctx, customerID, meterID, amountMinor)
}

// ensurePilot enforces the allow-list and daily caps when PILOT_MODE is on.
// Users/meters/DisCos not on the list get ErrServicePaused (no invitation leak).
func (o *Orchestrator) ensurePilot(ctx context.Context, customerID string, meterID uuid.UUID, amountMinor int64) error {
	if o == nil || !o.pilotMode {
		return nil
	}
	repo := o.st.Repos().Pilot()
	ok, err := repo.Has(ctx, pilot.KindUser, customerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrServicePaused
	}
	if meterID != uuid.Nil {
		ok, err = repo.Has(ctx, pilot.KindMeter, meterID.String())
		if err != nil {
			return err
		}
		if !ok {
			return ErrServicePaused
		}
		nDisco, err := repo.CountKind(ctx, pilot.KindDisco)
		if err != nil {
			return err
		}
		if nDisco > 0 {
			m, err := o.st.Repos().Meters().Get(ctx, meterID)
			if err != nil {
				return err
			}
			ok, err = repo.Has(ctx, pilot.KindDisco, strings.ToUpper(strings.TrimSpace(m.DiscoCode)))
			if err != nil {
				return err
			}
			if !ok {
				return ErrServicePaused
			}
		}
	}
	if amountMinor < 0 {
		amountMinor = 0
	}
	start, end := tokenshares.LagosDayBounds(o.Now())
	if o.pilotUserCap > 0 && customerID != "" {
		spent, err := sumConfirmedSpend(ctx, o, customerID, start, end)
		if err != nil {
			return err
		}
		if spent+amountMinor > o.pilotUserCap {
			return ErrPilotCap
		}
	}
	if o.pilotGlobalCap > 0 {
		spent, err := sumConfirmedSpend(ctx, o, "", start, end)
		if err != nil {
			return err
		}
		if spent+amountMinor > o.pilotGlobalCap {
			return ErrPilotCap
		}
	}
	return nil
}

func sumConfirmedSpend(ctx context.Context, o *Orchestrator, customerID string, from, to time.Time) (int64, error) {
	list, err := o.st.Repos().Transactions().ListCreatedBetween(ctx, from, to)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, t := range list {
		if t == nil || t.Payment != tx.PayConfirmed {
			continue
		}
		if customerID != "" && t.CustomerID != customerID {
			continue
		}
		sum += t.Amount.Minor
	}
	return sum, nil
}

func normalizePilotEntry(e *pilot.Entry) {
	e.Value = strings.TrimSpace(e.Value)
	e.Reason = strings.TrimSpace(e.Reason)
	if e.Kind == pilot.KindDisco {
		e.Value = strings.ToUpper(e.Value)
	}
}

func (o *Orchestrator) ListPilot(ctx context.Context) (mode, paused bool, userCap, globalCap int64, entries []pilot.Entry, err error) {
	entries, err = o.st.Repos().Pilot().List(ctx, "")
	if err != nil {
		return o.PilotMode(), o.ServicePaused(), o.pilotUserCap, o.pilotGlobalCap, nil, err
	}
	if entries == nil {
		entries = []pilot.Entry{}
	}
	return o.PilotMode(), o.ServicePaused(), o.pilotUserCap, o.pilotGlobalCap, entries, nil
}

func (o *Orchestrator) AddPilotEntry(ctx context.Context, actor audit.Actor, e pilot.Entry) error {
	normalizePilotEntry(&e)
	if e.Value == "" {
		return ErrInvalidPilot
	}
	if _, ok := pilot.ParseKind(string(e.Kind)); !ok {
		return ErrInvalidPilot
	}
	if e.Kind == pilot.KindMeter {
		if _, err := uuid.Parse(e.Value); err != nil {
			return ErrInvalidPilot
		}
	}
	e.CreatedBy = actor.UserID
	if e.CreatedAt.IsZero() {
		e.CreatedAt = o.Now().UTC()
	}
	if err := o.st.Repos().Pilot().Add(ctx, e); err != nil {
		return err
	}
	return o.st.Repos().Audit().Add(ctx, actor.Event("pilot.allowlist.add", "pilot_allowlist", uuid.Nil, map[string]any{
		"kind": string(e.Kind),
	}))
}

func (o *Orchestrator) RemovePilotEntry(ctx context.Context, actor audit.Actor, kind pilot.Kind, value string) error {
	value = strings.TrimSpace(value)
	if kind == pilot.KindDisco {
		value = strings.ToUpper(value)
	}
	if err := o.st.Repos().Pilot().Remove(ctx, kind, value); err != nil {
		return err
	}
	return o.st.Repos().Audit().Add(ctx, actor.Event("pilot.allowlist.remove", "pilot_allowlist", uuid.Nil, map[string]any{
		"kind": string(kind),
	}))
}

func (o *Orchestrator) PauseService(ctx context.Context, actor audit.Actor) error {
	o.SetServicePaused(true)
	return o.st.Repos().Audit().Add(ctx, actor.Event("pilot.service.pause", "pilot", uuid.Nil, nil))
}

func (o *Orchestrator) ResumeService(ctx context.Context, actor audit.Actor) error {
	o.SetServicePaused(false)
	return o.st.Repos().Audit().Add(ctx, actor.Event("pilot.service.resume", "pilot", uuid.Nil, nil))
}
