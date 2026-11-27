package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/store"
)

type reconRepo struct{ *repos }

func reconKey(provider string, date time.Time) string {
	return provider + "|" + reconcile.DateOnly(date).Format("2006-01-02")
}

func exceptionFP(e reconcile.Exception) string { return e.Fingerprint() }

func cloneReconRun(r reconcile.Run) reconcile.Run { return r }

func cloneEx(e reconcile.Exception) reconcile.Exception { return e }

func cloneAdj(a reconcile.Adjustment) reconcile.Adjustment { return a }

func (r reconRepo) ClaimRun(_ context.Context, provider string, date time.Time, id uuid.UUID) (reconcile.Run, error) {
	defer r.lock()()
	date = reconcile.DateOnly(date)
	k := reconKey(provider, date)
	if existing, ok := r.s.reconRuns[k]; ok {
		if existing.Status == reconcile.RunCompleted {
			return cloneReconRun(*existing), nil
		}
		prev := *existing
		existing.Status = reconcile.RunRunning
		existing.StartedAt = time.Now().UTC()
		existing.Error = ""
		r.onUndo(func() { cp := prev; r.s.reconRuns[k] = &cp })
		return cloneReconRun(*existing), nil
	}
	run := &reconcile.Run{ID: id, Provider: provider, BusinessDate: date, Status: reconcile.RunRunning, StartedAt: time.Now().UTC()}
	r.s.reconRuns[k] = run
	r.onUndo(func() { delete(r.s.reconRuns, k) })
	return cloneReconRun(*run), nil
}

func (r reconRepo) GetRun(_ context.Context, provider string, date time.Time) (reconcile.Run, error) {
	defer r.lock()()
	run, ok := r.s.reconRuns[reconKey(provider, date)]
	if !ok {
		return reconcile.Run{}, store.ErrNotFound
	}
	return cloneReconRun(*run), nil
}

func (r reconRepo) CompleteRun(_ context.Context, run reconcile.Run) error {
	defer r.lock()()
	k := reconKey(run.Provider, run.BusinessDate)
	cur, ok := r.s.reconRuns[k]
	if !ok {
		return store.ErrNotFound
	}
	prev := *cur
	*cur = run
	cur.Status = reconcile.RunCompleted
	r.onUndo(func() { cp := prev; r.s.reconRuns[k] = &cp })
	return nil
}

func (r reconRepo) FailRun(_ context.Context, id uuid.UUID, errText string) error {
	defer r.lock()()
	for _, run := range r.s.reconRuns {
		if run.ID == id {
			prev := *run
			run.Status = reconcile.RunFailed
			run.Error = errText
			cp := prev
			r.onUndo(func() { *run = cp })
			return nil
		}
	}
	return store.ErrNotFound
}

func (r reconRepo) UpsertException(_ context.Context, e *reconcile.Exception) error {
	defer r.lock()()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	e.UpdatedAt = time.Now().UTC()
	fp := exceptionFP(*e)
	if id, ok := r.s.reconExFP[fp]; ok {
		cur := r.s.reconEx[id]
		prev := *cur
		if cur.Status == reconcile.StatusResolved || cur.Status == reconcile.StatusWrittenOff {
			e.ID = cur.ID
			return nil
		}
		cur.RunID = e.RunID
		cur.OursMinor = e.OursMinor
		cur.TheirsMinor = e.TheirsMinor
		cur.UpdatedAt = e.UpdatedAt
		e.ID = cur.ID
		e.Status = cur.Status
		r.onUndo(func() { cp := prev; r.s.reconEx[id] = &cp })
		return nil
	}
	cp := cloneEx(*e)
	r.s.reconEx[e.ID] = &cp
	r.s.reconExFP[fp] = e.ID
	r.onUndo(func() { delete(r.s.reconEx, e.ID); delete(r.s.reconExFP, fp) })
	return nil
}

func (r reconRepo) GetException(_ context.Context, id uuid.UUID) (reconcile.Exception, error) {
	defer r.lock()()
	e, ok := r.s.reconEx[id]
	if !ok {
		return reconcile.Exception{}, store.ErrNotFound
	}
	return cloneEx(*e), nil
}

func (r reconRepo) ListExceptions(_ context.Context, f reconcile.ExceptionFilter) ([]reconcile.Exception, error) {
	defer r.lock()()
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	var out []reconcile.Exception
	for _, e := range r.s.reconEx {
		if f.Provider != "" && e.Provider != f.Provider {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.Category != "" && e.Category != f.Category {
			continue
		}
		if !f.BusinessDate.IsZero() && reconcile.DateOnly(e.BusinessDate) != reconcile.DateOnly(f.BusinessDate) {
			continue
		}
		out = append(out, cloneEx(*e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r reconRepo) UpdateException(_ context.Context, e reconcile.Exception) error {
	defer r.lock()()
	cur, ok := r.s.reconEx[e.ID]
	if !ok {
		return store.ErrNotFound
	}
	prev := *cur
	e.UpdatedAt = time.Now().UTC()
	*cur = e
	r.onUndo(func() { cp := prev; r.s.reconEx[e.ID] = &cp })
	return nil
}

func (r reconRepo) CountOpen(_ context.Context) ([]reconcile.OpenCount, error) {
	defer r.lock()()
	type key struct {
		p string
		c reconcile.Category
	}
	n := map[key]int64{}
	for _, e := range r.s.reconEx {
		if e.Status != reconcile.StatusOpen && e.Status != reconcile.StatusInvestigating {
			continue
		}
		n[key{e.Provider, e.Category}]++
	}
	var out []reconcile.OpenCount
	for k, v := range n {
		out = append(out, reconcile.OpenCount{Provider: k.p, Category: k.c, N: v})
	}
	return out, nil
}

func (r reconRepo) CreateAdjustment(_ context.Context, a *reconcile.Adjustment) error {
	defer r.lock()()
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	for _, ex := range r.s.reconAdj {
		if ex.ExceptionID == a.ExceptionID && ex.Status == reconcile.AdjProposed {
			return store.ErrDuplicate
		}
	}
	cp := cloneAdj(*a)
	r.s.reconAdj[a.ID] = &cp
	r.onUndo(func() { delete(r.s.reconAdj, a.ID) })
	return nil
}

func (r reconRepo) GetAdjustment(_ context.Context, id uuid.UUID) (reconcile.Adjustment, error) {
	defer r.lock()()
	a, ok := r.s.reconAdj[id]
	if !ok {
		return reconcile.Adjustment{}, store.ErrNotFound
	}
	return cloneAdj(*a), nil
}

func (r reconRepo) UpdateAdjustment(_ context.Context, a reconcile.Adjustment) error {
	defer r.lock()()
	cur, ok := r.s.reconAdj[a.ID]
	if !ok {
		return store.ErrNotFound
	}
	prev := *cur
	*cur = a
	r.onUndo(func() { cp := prev; r.s.reconAdj[a.ID] = &cp })
	return nil
}
