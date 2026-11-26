package memstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type remoteCohortRepo struct{ *repos }

func (r remoteCohortRepo) Create(_ context.Context, c meters.RemoteCohort) error {
	defer r.lock()()
	for _, existing := range r.s.remoteCohorts {
		if existing.RevokedAt == nil && existing.DiscoCode == c.DiscoCode && existing.NumberPrefix == c.NumberPrefix {
			return store.ErrDuplicate
		}
	}
	if _, ok := r.s.remoteCohorts[c.ID]; ok {
		return store.ErrDuplicate
	}
	r.s.remoteCohorts[c.ID] = c
	r.onUndo(func() { delete(r.s.remoteCohorts, c.ID) })
	return nil
}

func (r remoteCohortRepo) Get(_ context.Context, id uuid.UUID) (meters.RemoteCohort, error) {
	defer r.lock()()
	c, ok := r.s.remoteCohorts[id]
	if !ok {
		return meters.RemoteCohort{}, store.ErrNotFound
	}
	return c, nil
}

func (r remoteCohortRepo) List(_ context.Context) ([]meters.RemoteCohort, error) {
	defer r.lock()()
	out := make([]meters.RemoteCohort, 0, len(r.s.remoteCohorts))
	for _, c := range r.s.remoteCohorts {
		out = append(out, c)
	}
	return out, nil
}

func (r remoteCohortRepo) Match(_ context.Context, disco, meterNumber string) (meters.RemoteCohort, error) {
	defer r.lock()()
	var best meters.RemoteCohort
	found := false
	for _, c := range r.s.remoteCohorts {
		if !c.Matches(disco, meterNumber) {
			continue
		}
		if !found || len(c.NumberPrefix) > len(best.NumberPrefix) {
			best, found = c, true
		}
	}
	if !found {
		return meters.RemoteCohort{}, store.ErrNotFound
	}
	return best, nil
}

func (r remoteCohortRepo) Revoke(_ context.Context, id uuid.UUID, at time.Time) error {
	defer r.lock()()
	c, ok := r.s.remoteCohorts[id]
	if !ok {
		return store.ErrNotFound
	}
	if c.RevokedAt != nil {
		return store.ErrNotFound
	}
	prev := c
	ts := at.UTC()
	c.RevokedAt = &ts
	c.Authorised = false
	r.s.remoteCohorts[id] = c
	r.onUndo(func() { r.s.remoteCohorts[id] = prev })
	return nil
}
