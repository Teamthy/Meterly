package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/risk"
)

type riskRepo struct{ *repos }

func cloneRiskRule(r risk.Rule) risk.Rule { return r }

func cloneRiskDecision(d risk.Decision) risk.Decision { return d }

func cloneRiskObs(o risk.Observation) risk.Observation { return o }

func cloneDenyEntry(e risk.DenyEntry) risk.DenyEntry { return e }

func (r riskRepo) ListRules(_ context.Context) ([]risk.Rule, error) {
	defer r.lock()()
	out := make([]risk.Rule, 0, len(r.s.riskRules))
	for _, rule := range r.s.riskRules {
		out = append(out, cloneRiskRule(*rule))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

func (r riskRepo) GetRule(_ context.Context, code string) (risk.Rule, error) {
	defer r.lock()()
	rule, ok := r.s.riskRules[code]
	if !ok {
		return risk.Rule{}, risk.ErrNotFound
	}
	return cloneRiskRule(*rule), nil
}

func (r riskRepo) UpdateRule(_ context.Context, rule risk.Rule) error {
	defer r.lock()()
	prev, ok := r.s.riskRules[rule.Code]
	var old risk.Rule
	if ok {
		old = *prev
	}
	cp := rule
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
	r.s.riskRules[rule.Code] = &cp
	r.onUndo(func() {
		if ok {
			o := old
			r.s.riskRules[rule.Code] = &o
		} else {
			delete(r.s.riskRules, rule.Code)
		}
	})
	return nil
}

func (r riskRepo) InsertObservation(_ context.Context, o risk.Observation) error {
	defer r.lock()()
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	r.s.riskObs = append(r.s.riskObs, o)
	n := len(r.s.riskObs)
	r.onUndo(func() { r.s.riskObs = r.s.riskObs[:n-1] })
	return nil
}

func matchObs(o risk.Observation, f risk.ObservationFilter) bool {
	if f.Kind != "" && o.Kind != f.Kind {
		return false
	}
	if f.UserID != "" && o.UserID != f.UserID {
		return false
	}
	if f.MeterID != uuid.Nil && o.MeterID != f.MeterID {
		return false
	}
	if f.LinkID != uuid.Nil && o.LinkID != f.LinkID {
		return false
	}
	if f.EmailHMAC != "" && o.EmailHMAC != f.EmailHMAC {
		return false
	}
	if f.IPHMAC != "" && o.IPHMAC != f.IPHMAC {
		return false
	}
	if f.DeviceHMAC != "" && o.DeviceHMAC != f.DeviceHMAC {
		return false
	}
	if f.CardSignature != "" && o.CardSignature != f.CardSignature {
		return false
	}
	if !f.Since.IsZero() && o.ObservedAt.Before(f.Since) {
		return false
	}
	return true
}

func (r riskRepo) CountObservations(_ context.Context, f risk.ObservationFilter) (int, error) {
	defer r.lock()()
	n := 0
	for _, o := range r.s.riskObs {
		if matchObs(o, f) {
			n++
		}
	}
	return n, nil
}

func (r riskRepo) DistinctCards(_ context.Context, f risk.ObservationFilter) (int, error) {
	defer r.lock()()
	seen := map[string]struct{}{}
	cf := f
	cf.Kind = ""
	for _, o := range r.s.riskObs {
		if o.CardSignature == "" || !matchObs(o, cf) {
			continue
		}
		seen[o.CardSignature] = struct{}{}
	}
	return len(seen), nil
}

func (r riskRepo) RecentAmounts(_ context.Context, userID string, limit int) ([]int64, error) {
	defer r.lock()()
	var out []int64
	for i := len(r.s.riskObs) - 1; i >= 0 && len(out) < limit; i-- {
		o := r.s.riskObs[i]
		if o.Kind == risk.KindPaymentOK && o.UserID == userID && o.AmountMinor > 0 {
			out = append(out, o.AmountMinor)
		}
	}
	return out, nil
}

func (r riskRepo) InsertDecision(_ context.Context, d risk.Decision) error {
	defer r.lock()()
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	cp := d
	r.s.riskDec[d.ID] = &cp
	r.onUndo(func() { delete(r.s.riskDec, d.ID) })
	return nil
}

func (r riskRepo) BindDecisionTxn(_ context.Context, id, txnID uuid.UUID) error {
	defer r.lock()()
	d, ok := r.s.riskDec[id]
	if !ok {
		return risk.ErrNotFound
	}
	prev := d.TransactionID
	d.TransactionID = txnID
	r.onUndo(func() { d.TransactionID = prev })
	return nil
}

func (r riskRepo) ListDecisionsByTxn(_ context.Context, txnID uuid.UUID) ([]risk.Decision, error) {
	defer r.lock()()
	var out []risk.Decision
	for _, d := range r.s.riskDec {
		if d.TransactionID == txnID {
			out = append(out, cloneRiskDecision(*d))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r riskRepo) ListDenylist(_ context.Context) ([]risk.DenyEntry, error) {
	defer r.lock()()
	out := make([]risk.DenyEntry, 0, len(r.s.riskDeny))
	for _, e := range r.s.riskDeny {
		out = append(out, cloneDenyEntry(*e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r riskRepo) AddDenylist(_ context.Context, e risk.DenyEntry) error {
	defer r.lock()()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	for _, cur := range r.s.riskDeny {
		if cur.Kind == e.Kind && cur.Value == e.Value {
			return nil
		}
	}
	cp := e
	r.s.riskDeny[e.ID] = &cp
	r.onUndo(func() { delete(r.s.riskDeny, e.ID) })
	return nil
}

func (r riskRepo) RemoveDenylist(_ context.Context, id uuid.UUID) error {
	defer r.lock()()
	e, ok := r.s.riskDeny[id]
	if !ok {
		return risk.ErrNotFound
	}
	prev := *e
	delete(r.s.riskDeny, id)
	r.onUndo(func() { p := prev; r.s.riskDeny[id] = &p })
	return nil
}

func (r riskRepo) MatchDenylist(_ context.Context, kind risk.DenyKind, value string) (risk.DenyEntry, error) {
	defer r.lock()()
	for _, e := range r.s.riskDeny {
		if e.Kind == kind && e.Value == value {
			return cloneDenyEntry(*e), nil
		}
	}
	return risk.DenyEntry{}, risk.ErrNotFound
}

func (r riskRepo) ListIPRanges(_ context.Context) ([]risk.DenyEntry, error) {
	defer r.lock()()
	var out []risk.DenyEntry
	for _, e := range r.s.riskDeny {
		if e.Kind == risk.DenyIPRange {
			out = append(out, cloneDenyEntry(*e))
		}
	}
	return out, nil
}

func (r riskRepo) PauseLink(_ context.Context, p risk.LinkPause) error {
	defer r.lock()()
	prev, ok := r.s.riskPauses[p.LinkID]
	if p.PausedAt.IsZero() {
		p.PausedAt = time.Now().UTC()
	}
	r.s.riskPauses[p.LinkID] = p
	r.onUndo(func() {
		if ok {
			r.s.riskPauses[p.LinkID] = prev
		} else {
			delete(r.s.riskPauses, p.LinkID)
		}
	})
	return nil
}

func (r riskRepo) LinkPause(_ context.Context, linkID uuid.UUID) (risk.LinkPause, error) {
	defer r.lock()()
	p, ok := r.s.riskPauses[linkID]
	if !ok {
		return risk.LinkPause{}, risk.ErrNotFound
	}
	return p, nil
}

func (r riskRepo) ClearLinkPause(_ context.Context, linkID uuid.UUID) error {
	defer r.lock()()
	prev, ok := r.s.riskPauses[linkID]
	if !ok {
		return nil
	}
	delete(r.s.riskPauses, linkID)
	r.onUndo(func() { r.s.riskPauses[linkID] = prev })
	return nil
}
