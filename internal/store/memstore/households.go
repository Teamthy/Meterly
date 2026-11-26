package memstore

import (
	"context"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type householdRepo struct{ *repos }

func hhMemberKey(hid uuid.UUID, userID string) string {
	return hid.String() + ":" + userID
}
func hhMeterKey(hid, mid uuid.UUID) string { return hid.String() + ":" + mid.String() }
func ownKey(mid uuid.UUID, userID string) string {
	return mid.String() + ":" + userID
}

func (r householdRepo) Create(_ context.Context, h households.Household) error {
	defer r.lock()()
	if _, ok := r.s.households[h.ID]; ok {
		return store.ErrDuplicate
	}
	r.s.households[h.ID] = h
	r.onUndo(func() { delete(r.s.households, h.ID) })
	return nil
}

func (r householdRepo) Get(_ context.Context, id uuid.UUID) (households.Household, error) {
	defer r.lock()()
	h, ok := r.s.households[id]
	if !ok {
		return households.Household{}, store.ErrNotFound
	}
	return h, nil
}

func (r householdRepo) HouseholdIDsForUser(_ context.Context, userID string) ([]uuid.UUID, error) {
	defer r.lock()()
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, m := range r.s.hhMembers {
		if m.UserID == userID && (m.Status == households.StatusActive || m.Status == households.StatusInvited) && !seen[m.HouseholdID] {
			seen[m.HouseholdID] = true
			ids = append(ids, m.HouseholdID)
		}
	}
	return ids, nil
}

func (r householdRepo) UpsertMember(_ context.Context, m households.Member) error {
	defer r.lock()()
	k := hhMemberKey(m.HouseholdID, m.UserID)
	prev, had := r.s.hhMembers[k]
	r.s.hhMembers[k] = m
	r.onUndo(func() {
		if had {
			r.s.hhMembers[k] = prev
		} else {
			delete(r.s.hhMembers, k)
		}
	})
	return nil
}

func (r householdRepo) Member(_ context.Context, householdID uuid.UUID, userID string) (households.Member, error) {
	defer r.lock()()
	m, ok := r.s.hhMembers[hhMemberKey(householdID, userID)]
	if !ok {
		return households.Member{}, store.ErrNotFound
	}
	return m, nil
}

func (r householdRepo) ListMembers(_ context.Context, householdID uuid.UUID) ([]households.Member, error) {
	defer r.lock()()
	var out []households.Member
	for _, m := range r.s.hhMembers {
		if m.HouseholdID == householdID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r householdRepo) AddMeter(_ context.Context, link households.MeterLink) error {
	defer r.lock()()
	k := hhMeterKey(link.HouseholdID, link.MeterID)
	if _, ok := r.s.hhMeters[k]; ok {
		return store.ErrDuplicate
	}
	r.s.hhMeters[k] = link
	r.onUndo(func() { delete(r.s.hhMeters, k) })
	return nil
}

func (r householdRepo) ListMeters(_ context.Context, householdID uuid.UUID) ([]households.MeterLink, error) {
	defer r.lock()()
	var out []households.MeterLink
	for _, l := range r.s.hhMeters {
		if l.HouseholdID == householdID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (r householdRepo) UpsertOwnership(_ context.Context, o households.Ownership) error {
	defer r.lock()()
	k := ownKey(o.MeterID, o.UserID)
	prev, had := r.s.ownership[k]
	if had {
		o.ID = prev.ID
	}
	r.s.ownership[k] = o
	r.onUndo(func() {
		if had {
			r.s.ownership[k] = prev
		} else {
			delete(r.s.ownership, k)
		}
	})
	return nil
}

func (r householdRepo) ActiveOwnership(_ context.Context, meterID uuid.UUID, userID string) (households.Ownership, error) {
	defer r.lock()()
	o, ok := r.s.ownership[ownKey(meterID, userID)]
	if !ok || o.RevokedAt != nil {
		return households.Ownership{}, store.ErrNotFound
	}
	return o, nil
}

func (r householdRepo) ListOwnership(_ context.Context, meterID uuid.UUID) ([]households.Ownership, error) {
	defer r.lock()()
	var out []households.Ownership
	for _, o := range r.s.ownership {
		if o.MeterID == meterID && o.RevokedAt == nil {
			out = append(out, o)
		}
	}
	return out, nil
}

func (r householdRepo) PermissionForMeter(_ context.Context, userID string, meterID uuid.UUID) (meters.Permission, error) {
	defer r.lock()()
	var best meters.Permission
	found := false
	for _, l := range r.s.hhMeters {
		if l.MeterID != meterID {
			continue
		}
		m, ok := r.s.hhMembers[hhMemberKey(l.HouseholdID, userID)]
		if !ok || m.Status != households.StatusActive {
			continue
		}
		p := households.PermissionForRole(m.Role)
		if p == "" {
			continue
		}
		if !found || p.Allows(best) {
			best = p
			found = true
		}
	}
	if !found {
		return "", store.ErrNotFound
	}
	return best, nil
}

var _ store.HouseholdRepo = householdRepo{}
