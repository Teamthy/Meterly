package memstore

import (
	"context"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/pilot"
)

type pilotRepo struct{ *repos }

func keyPilot(k pilot.Kind, v string) string { return string(k) + "\x00" + v }

func (r pilotRepo) List(_ context.Context, kind pilot.Kind) ([]pilot.Entry, error) {
	defer r.lock()()
	var out []pilot.Entry
	for _, e := range r.s.pilotAllow {
		if kind == "" || e.Kind == kind {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (r pilotRepo) Add(_ context.Context, e pilot.Entry) error {
	defer r.lock()()
	e.Value = strings.TrimSpace(e.Value)
	if e.Value == "" {
		return nil
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	k := keyPilot(e.Kind, e.Value)
	if _, ok := r.s.pilotAllow[k]; ok {
		return nil
	}
	cp := e
	r.s.pilotAllow[k] = &cp
	r.onUndo(func() { delete(r.s.pilotAllow, k) })
	return nil
}

func (r pilotRepo) Remove(_ context.Context, kind pilot.Kind, value string) error {
	defer r.lock()()
	k := keyPilot(kind, strings.TrimSpace(value))
	prev, ok := r.s.pilotAllow[k]
	if !ok {
		return pilot.ErrNotFound
	}
	delete(r.s.pilotAllow, k)
	r.onUndo(func() { r.s.pilotAllow[k] = prev })
	return nil
}

func (r pilotRepo) Has(_ context.Context, kind pilot.Kind, value string) (bool, error) {
	defer r.lock()()
	_, ok := r.s.pilotAllow[keyPilot(kind, strings.TrimSpace(value))]
	return ok, nil
}

func (r pilotRepo) CountKind(_ context.Context, kind pilot.Kind) (int, error) {
	defer r.lock()()
	n := 0
	for _, e := range r.s.pilotAllow {
		if e.Kind == kind {
			n++
		}
	}
	return n, nil
}
