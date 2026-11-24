// Package registry is the meter registry and capability engine (spec §4.3).
// It validates meters with the vending provider, keeps capability
// snapshots, and enforces per-user access grants.
package registry

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
)

type Registry struct {
	st      store.Store
	box     *secure.Box
	vending providers.VendingProvider
	remote  providers.RemoteCreditProvider // optional
	Now     func() time.Time
}

func New(st store.Store, box *secure.Box, v providers.VendingProvider, r providers.RemoteCreditProvider) *Registry {
	return &Registry{st: st, box: box, vending: v, remote: r, Now: func() time.Time { return time.Now().UTC() }}
}

func notFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return meters.ErrNotFound
	}
	return err
}

// probe asks providers about the meter (network calls; never inside a DB tx).
func (g *Registry) probe(ctx context.Context, number, disco string) (providers.MeterValidationResult, meters.Capabilities, error) {
	res, err := g.vending.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: number, DiscoCode: disco})
	if err != nil {
		return res, meters.Capabilities{}, err
	}
	if !res.Valid {
		return res, meters.Capabilities{}, meters.ErrInvalidMeter
	}
	now := g.Now()
	caps := meters.Capabilities{
		TokenVending:  meters.Capability{Supported: true, Authorised: true, LastVerifiedAt: &now},
		RemoteReading: meters.Capability{Supported: res.RemoteReadingSupported, LastVerifiedAt: &now},
		RemoteCredit:  meters.Capability{Supported: res.RemoteCreditSupported, LastVerifiedAt: &now},
	}
	if res.RemoteCreditSupported && g.remote != nil {
		if eligible, reachable, err := g.remote.CheckEligibility(ctx, number); err == nil {
			caps.RemoteCredit.Authorised = eligible
			caps.RemoteCredit.Reachable = reachable
		}
	}
	return res, caps, nil
}

// upsert writes the validated meter inside r's transaction.
func (g *Registry) upsert(ctx context.Context, r store.Repos, number, disco string, res providers.MeterValidationResult, caps meters.Capabilities) (*meters.Meter, error) {
	hash := g.box.Hash(disco + ":" + number)
	now := g.Now()
	for attempt := 0; attempt < 2; attempt++ {
		m, err := r.Meters().GetByHash(ctx, hash)
		if err == nil {
			// Keep evidence of confirmed remote capabilities across re-validation.
			confirmed := m.Capabilities.RemoteCredit.Confirmed && caps.RemoteCredit.Supported
			readAuth := m.Capabilities.RemoteReading.Authorised && caps.RemoteReading.Supported
			readConf := m.Capabilities.RemoteReading.Confirmed && caps.RemoteReading.Supported
			m.MeterType, m.CustomerName, m.Address, m.ValidatedAt = res.MeterType, res.CustomerName, res.Address, now
			m.Capabilities = caps
			m.Capabilities.RemoteCredit.Confirmed = confirmed
			m.Capabilities.RemoteReading.Authorised = readAuth
			m.Capabilities.RemoteReading.Confirmed = readConf
			return m, r.Meters().Update(ctx, m)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		enc, err := g.box.Encrypt(number)
		if err != nil {
			return nil, err
		}
		m = &meters.Meter{
			ID: uuid.New(), NumberEncrypted: enc, KeyVersion: g.box.KeyVersion, NumberHash: hash,
			Masked: secure.Mask(number, 4), DiscoCode: disco, MeterType: res.MeterType,
			CustomerName: res.CustomerName, Address: res.Address, ValidatedAt: now, Capabilities: caps,
		}
		err = r.Meters().Insert(ctx, m)
		if err == nil {
			return m, nil
		}
		if !errors.Is(err, store.ErrDuplicate) {
			return nil, err
		}
		// Lost an insert race; loop to update the winner's row.
	}
	return nil, store.ErrConflict
}

// Validate checks a meter with the provider and upserts the meter record.
// It grants no access by itself.
func (g *Registry) Validate(ctx context.Context, number, disco string) (*meters.Meter, error) {
	res, caps, err := g.probe(ctx, number, disco)
	if err != nil {
		return nil, err
	}
	var m *meters.Meter
	err = g.st.InTx(ctx, func(r store.Repos) error {
		var err error
		m, err = g.upsert(ctx, r, number, disco, res, caps)
		return err
	})
	return m, err
}

// Register saves a validated meter for purchases. Provider validation proves
// neither ownership nor authority to manage access, so self-service never
// grants MANAGE or elevates an existing higher grant.
func (g *Registry) Register(ctx context.Context, userID, number, disco, label string) (*meters.Meter, error) {
	res, caps, err := g.probe(ctx, number, disco)
	if err != nil {
		return nil, err
	}
	var m *meters.Meter
	err = g.st.InTx(ctx, func(r store.Repos) error {
		var err error
		if m, err = g.upsert(ctx, r, number, disco, res, caps); err != nil {
			return err
		}
		return r.Meters().UpsertGrant(ctx, meters.Grant{MeterID: m.ID, UserID: userID, Permission: meters.PermPurchase, Label: label, GrantedBy: userID})
	})
	return m, err
}

func (g *Registry) Get(ctx context.Context, id uuid.UUID) (*meters.Meter, error) {
	m, err := g.st.Repos().Meters().Get(ctx, id)
	return m, notFound(err)
}

// Authorize returns the meter if the user holds at least want on it.
func (g *Registry) Authorize(ctx context.Context, userID string, id uuid.UUID, want meters.Permission) (*meters.Meter, error) {
	return AuthorizeIn(ctx, g.st.Repos(), userID, id, want)
}

// AuthorizeIn checks a grant without locking. Do not use this for a write
// that must serialize with access changes; use AuthorizeForUpdateIn instead.
func AuthorizeIn(ctx context.Context, r store.Repos, userID string, id uuid.UUID, want meters.Permission) (*meters.Meter, error) {
	m, err := r.Meters().Get(ctx, id)
	return authorizeGrant(ctx, r, userID, id, want, m, err)
}

// AuthorizeForUpdateIn locks the meter before checking access and holds that
// lock until the surrounding transaction commits. Access modifications must
// take the same lock before changing grants.
func AuthorizeForUpdateIn(ctx context.Context, r store.Repos, userID string, id uuid.UUID, want meters.Permission) (*meters.Meter, error) {
	m, err := r.Meters().GetForUpdate(ctx, id)
	return authorizeGrant(ctx, r, userID, id, want, m, err)
}

func authorizeGrant(ctx context.Context, r store.Repos, userID string, id uuid.UUID, want meters.Permission, m *meters.Meter, err error) (*meters.Meter, error) {
	if err != nil {
		return nil, notFound(err)
	}
	gr, err := r.Meters().ActiveGrant(ctx, id, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !gr.Permission.Allows(want)) {
		return nil, meters.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (g *Registry) ListForUser(ctx context.Context, userID string) ([]meters.Listed, error) {
	return g.st.Repos().Meters().ListForUser(ctx, userID)
}

// Number decrypts the meter number for provider calls only.
func (g *Registry) Number(m *meters.Meter) (string, error) { return g.box.Decrypt(m.NumberEncrypted) }

// MarkRemoteCreditConfirmedIn records, inside r's transaction, evidence
// that remote delivery works for this meter.
func MarkRemoteCreditConfirmedIn(ctx context.Context, r store.Repos, id uuid.UUID) error {
	m, err := r.Meters().Get(ctx, id)
	if err != nil {
		return err
	}
	if m.Capabilities.RemoteCredit.Confirmed {
		return nil
	}
	m.Capabilities.RemoteCredit.Confirmed = true
	return r.Meters().Update(ctx, m)
}

// MarkRemoteReadingConfirmedIn records, inside r's transaction, that remote
// balance reading is authorised and confirmed for this meter.
func MarkRemoteReadingConfirmedIn(ctx context.Context, r store.Repos, id uuid.UUID, at time.Time) error {
	m, err := r.Meters().GetForUpdate(ctx, id)
	if err != nil {
		return err
	}
	if !m.Capabilities.RemoteReading.Supported {
		return meters.ErrInvalidMeter
	}
	ts := at.UTC()
	m.Capabilities.RemoteReading.Authorised = true
	m.Capabilities.RemoteReading.Confirmed = true
	m.Capabilities.RemoteReading.LastVerifiedAt = &ts
	return r.Meters().Update(ctx, m)
}

// RemoveSavedMeter revokes only the caller's own active access, not that of
// household members. In-flight purchases are not cancelled. Active AutoPower
// rules for (userID, id) are atomically paused and their unstarted runs
// cancelled so revoked meters are never charged.
func (g *Registry) RemoveSavedMeter(ctx context.Context, userID string, id uuid.UUID) error {
	now := g.Now().UTC()
	return g.st.InTx(ctx, func(r store.Repos) error {
		if _, err := r.Meters().GetForUpdate(ctx, id); err != nil {
			return notFound(err)
		}
		if _, err := r.Meters().ActiveGrant(ctx, id, userID); errors.Is(err, store.ErrNotFound) {
			return meters.ErrNotFound
		} else if err != nil {
			return err
		}
		if err := r.Meters().Revoke(ctx, id, userID, now); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		userRules, err := r.AutoPower().ListRulesForUser(ctx, userID)
		if err != nil {
			return err
		}
		for _, candidate := range userRules {
			if candidate.MeterID != id || candidate.Status == autopower.RuleCancelled {
				continue
			}
			rl, err := r.AutoPower().GetRuleForUpdate(ctx, candidate.ID)
			if err != nil {
				return err
			}
			if rl.Status == autopower.RuleCancelled {
				continue
			}
			wasActive := rl.Status == autopower.RuleActive
			rl.Status = autopower.RulePaused
			rl.PauseReason = autopower.PauseMeterAccessRevoked
			rl.NextScheduledAt = nil
			rl.NextPollAt = nil
			rl.LeaseUntil = time.Time{}
			rl.UpdatedAt = now
			if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
				return err
			}
			if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
				return err
			}
			if err := r.Audit().Add(ctx, actor.Event("autopower.rule.paused", "autopower_rule", rl.ID, map[string]any{
				"reason": autopower.PauseMeterAccessRevoked,
			})); err != nil {
				return err
			}
			if wasActive {
				ev := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{
					"reason": autopower.PauseMeterAccessRevoked,
				})
				ev.CreatedAt = now
				if err := r.Outbox().Add(ctx, ev); err != nil {
					return err
				}
			}
		}
		return r.Audit().Add(ctx, actor.Event("meter.self_access_removed", "meter", id, nil))
	})
}
