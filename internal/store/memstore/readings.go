package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type readingRepo struct{ *repos }

func (r readingRepo) InsertSample(_ context.Context, s meters.ReadingSample) error {
	defer r.lock()()
	if _, ok := r.s.meters[s.MeterID]; !ok {
		return store.ErrNotFound
	}
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	s.ReadAt = s.ReadAt.UTC()
	list := r.s.readingSamples[s.MeterID]
	for _, existing := range list {
		if existing.ReadAt.Equal(s.ReadAt) {
			return nil
		}
	}
	cloned := s
	r.s.readingSamples[s.MeterID] = append(list, cloned)
	id, meter := s.ID, s.MeterID
	r.onUndo(func() {
		out := r.s.readingSamples[meter][:0]
		for _, x := range r.s.readingSamples[meter] {
			if x.ID != id {
				out = append(out, x)
			}
		}
		r.s.readingSamples[meter] = out
	})
	return nil
}

func (r readingRepo) ListSamples(_ context.Context, meterID uuid.UUID, since time.Time, limit int) ([]meters.ReadingSample, error) {
	defer r.lock()()
	src := r.s.readingSamples[meterID]
	out := make([]meters.ReadingSample, 0, len(src))
	for _, s := range src {
		if !since.IsZero() && s.ReadAt.Before(since) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReadAt.After(out[j].ReadAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
