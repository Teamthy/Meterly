package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

type partnerRepo struct{ *repos }

func clonePartner(p partners.Partner) partners.Partner {
	out := p
	if p.ProposedActivationAt != nil {
		t := *p.ProposedActivationAt
		out.ProposedActivationAt = &t
	}
	if p.ActivatedAt != nil {
		t := *p.ActivatedAt
		out.ActivatedAt = &t
	}
	if p.SuspendedAt != nil {
		t := *p.SuspendedAt
		out.SuspendedAt = &t
	}
	return out
}

func cloneKey(k partners.APIKey) partners.APIKey {
	out := k
	out.Scopes = append([]string(nil), k.Scopes...)
	if k.LastUsedAt != nil {
		t := *k.LastUsedAt
		out.LastUsedAt = &t
	}
	if k.ExpiresAt != nil {
		t := *k.ExpiresAt
		out.ExpiresAt = &t
	}
	if k.RevokedAt != nil {
		t := *k.RevokedAt
		out.RevokedAt = &t
	}
	if k.OverlapUntil != nil {
		t := *k.OverlapUntil
		out.OverlapUntil = &t
	}
	if k.RotatedFrom != nil {
		id := *k.RotatedFrom
		out.RotatedFrom = &id
	}
	return out
}

func usageKey(id uuid.UUID, day time.Time) string {
	return id.String() + ":" + day.UTC().Format("2006-01-02")
}

func (r partnerRepo) CreatePartner(_ context.Context, p partners.Partner) error {
	defer r.lock()()
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if _, ok := r.s.partners[p.ID]; ok {
		return store.ErrDuplicate
	}
	cp := clonePartner(p)
	r.s.partners[p.ID] = &cp
	r.onUndo(func() { delete(r.s.partners, p.ID) })
	return nil
}

func (r partnerRepo) GetPartner(_ context.Context, id uuid.UUID) (partners.Partner, error) {
	defer r.lock()()
	p, ok := r.s.partners[id]
	if !ok {
		return partners.Partner{}, store.ErrNotFound
	}
	return clonePartner(*p), nil
}

func (r partnerRepo) ListPartners(_ context.Context, limit int) ([]partners.Partner, error) {
	defer r.lock()()
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	out := make([]partners.Partner, 0, len(r.s.partners))
	for _, p := range r.s.partners {
		out = append(out, clonePartner(*p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r partnerRepo) UpdatePartner(_ context.Context, p partners.Partner) error {
	defer r.lock()()
	prev, ok := r.s.partners[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	old := clonePartner(*prev)
	cp := clonePartner(p)
	r.s.partners[p.ID] = &cp
	r.onUndo(func() {
		o := old
		r.s.partners[p.ID] = &o
	})
	return nil
}

func (r partnerRepo) InsertKey(_ context.Context, k partners.APIKey) error {
	defer r.lock()()
	if k.ID == uuid.Nil {
		k.ID = uuid.New()
	}
	if _, ok := r.s.partnerKeys[k.ID]; ok {
		return store.ErrDuplicate
	}
	if _, ok := r.s.partnerKeysPrefix[k.Prefix]; ok {
		return store.ErrDuplicate
	}
	for _, existing := range r.s.partnerKeys {
		if existing.SecretHash == k.SecretHash {
			return store.ErrDuplicate
		}
	}
	cp := cloneKey(k)
	r.s.partnerKeys[k.ID] = &cp
	r.s.partnerKeysPrefix[k.Prefix] = k.ID
	r.onUndo(func() {
		delete(r.s.partnerKeys, k.ID)
		delete(r.s.partnerKeysPrefix, k.Prefix)
	})
	return nil
}

func (r partnerRepo) GetKey(_ context.Context, id uuid.UUID) (partners.APIKey, error) {
	defer r.lock()()
	k, ok := r.s.partnerKeys[id]
	if !ok {
		return partners.APIKey{}, store.ErrNotFound
	}
	return cloneKey(*k), nil
}

func (r partnerRepo) GetKeyByPrefix(_ context.Context, prefix string) (partners.APIKey, error) {
	defer r.lock()()
	id, ok := r.s.partnerKeysPrefix[prefix]
	if !ok {
		return partners.APIKey{}, store.ErrNotFound
	}
	k, ok := r.s.partnerKeys[id]
	if !ok {
		return partners.APIKey{}, store.ErrNotFound
	}
	return cloneKey(*k), nil
}

func (r partnerRepo) ListKeys(_ context.Context, partnerID uuid.UUID) ([]partners.APIKey, error) {
	defer r.lock()()
	var out []partners.APIKey
	for _, k := range r.s.partnerKeys {
		if k.PartnerID == partnerID {
			out = append(out, cloneKey(*k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r partnerRepo) UpdateKey(_ context.Context, k partners.APIKey) error {
	defer r.lock()()
	prev, ok := r.s.partnerKeys[k.ID]
	if !ok {
		return store.ErrNotFound
	}
	old := cloneKey(*prev)
	cp := cloneKey(k)
	r.s.partnerKeys[k.ID] = &cp
	if old.Prefix != k.Prefix {
		delete(r.s.partnerKeysPrefix, old.Prefix)
		r.s.partnerKeysPrefix[k.Prefix] = k.ID
	}
	r.onUndo(func() {
		o := old
		r.s.partnerKeys[k.ID] = &o
		if old.Prefix != k.Prefix {
			delete(r.s.partnerKeysPrefix, k.Prefix)
			r.s.partnerKeysPrefix[old.Prefix] = k.ID
		}
	})
	return nil
}

func (r partnerRepo) TouchKeyUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	defer r.lock()()
	k, ok := r.s.partnerKeys[id]
	if !ok {
		return store.ErrNotFound
	}
	prev := k.LastUsedAt
	t := at.UTC()
	k.LastUsedAt = &t
	r.onUndo(func() { k.LastUsedAt = prev })
	return nil
}

func (r partnerRepo) IncrementUsage(_ context.Context, partnerID uuid.UUID, day time.Time, requests, valueMinor int64) error {
	defer r.lock()()
	day = time.Date(day.UTC().Year(), day.UTC().Month(), day.UTC().Day(), 0, 0, 0, 0, time.UTC)
	k := usageKey(partnerID, day)
	u, ok := r.s.partnerUsage[k]
	if !ok {
		n := partners.Usage{PartnerID: partnerID, Day: day}
		u = &n
		r.s.partnerUsage[k] = u
		r.onUndo(func() { delete(r.s.partnerUsage, k) })
	} else {
		prevReq, prevVal := u.RequestCount, u.ValueMinor
		r.onUndo(func() {
			u.RequestCount = prevReq
			u.ValueMinor = prevVal
		})
	}
	u.RequestCount += requests
	u.ValueMinor += valueMinor
	return nil
}

func (r partnerRepo) ListUsage(_ context.Context, partnerID uuid.UUID, from, to time.Time) ([]partners.Usage, error) {
	defer r.lock()()
	from = time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC)
	to = time.Date(to.UTC().Year(), to.UTC().Month(), to.UTC().Day(), 0, 0, 0, 0, time.UTC)
	var out []partners.Usage
	for _, u := range r.s.partnerUsage {
		if u.PartnerID != partnerID {
			continue
		}
		if u.Day.Before(from) || u.Day.After(to) {
			continue
		}
		cp := *u
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}
