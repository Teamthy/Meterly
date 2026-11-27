package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/store"
)

type potRepo struct{ *repos }

func copyPot(p *pots.Pot) *pots.Pot {
	cp := *p
	if p.NoteCiphertext != nil {
		cp.NoteCiphertext = append([]byte(nil), p.NoteCiphertext...)
	}
	return &cp
}

func copyContribution(c *pots.Contribution) *pots.Contribution {
	cp := *c
	if c.EmailCiphertext != nil {
		cp.EmailCiphertext = append([]byte(nil), c.EmailCiphertext...)
	}
	if c.NameCiphertext != nil {
		cp.NameCiphertext = append([]byte(nil), c.NameCiphertext...)
	}
	if c.PhoneCiphertext != nil {
		cp.PhoneCiphertext = append([]byte(nil), c.PhoneCiphertext...)
	}
	if c.PaidAt != nil {
		t := *c.PaidAt
		cp.PaidAt = &t
	}
	return &cp
}

func copyAllocation(a *pots.Allocation) *pots.Allocation {
	cp := *a
	return &cp
}

func (r potRepo) Create(_ context.Context, p *pots.Pot) error {
	defer r.lock()()
	if !r.s.userIDs[p.OwnerID] {
		return store.ErrNotFound
	}
	if p.MeterID != uuid.Nil {
		if _, ok := r.s.meters[p.MeterID]; !ok {
			return store.ErrNotFound
		}
	}
	if _, exists := r.s.pots[p.ID]; exists {
		return store.ErrDuplicate
	}
	if _, exists := r.s.potByCodeHMAC[p.CodeHMAC]; exists {
		return store.ErrDuplicate
	}
	id, ch := p.ID, p.CodeHMAC
	r.s.pots[id] = copyPot(p)
	r.s.potByCodeHMAC[ch] = id
	r.onUndo(func() {
		delete(r.s.pots, id)
		delete(r.s.potByCodeHMAC, ch)
	})
	return nil
}

func (r potRepo) Get(_ context.Context, id uuid.UUID) (*pots.Pot, error) {
	defer r.lock()()
	p, ok := r.s.pots[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyPot(p), nil
}

func (r potRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*pots.Pot, error) {
	return r.Get(ctx, id)
}

func (r potRepo) GetByCodeHMAC(_ context.Context, codeHMAC string) (*pots.Pot, error) {
	defer r.lock()()
	id, ok := r.s.potByCodeHMAC[codeHMAC]
	if !ok {
		return nil, store.ErrNotFound
	}
	p, ok := r.s.pots[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if !pots.ConstantTimeEqualHMAC(p.CodeHMAC, codeHMAC) {
		return nil, store.ErrNotFound
	}
	return copyPot(p), nil
}

func (r potRepo) GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*pots.Pot, error) {
	return r.GetByCodeHMAC(ctx, codeHMAC)
}

func (r potRepo) Update(_ context.Context, p *pots.Pot) error {
	defer r.lock()()
	prev, ok := r.s.pots[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	prevCopy := copyPot(prev)
	r.s.pots[p.ID] = copyPot(p)
	r.onUndo(func() {
		r.s.pots[p.ID] = prevCopy
	})
	return nil
}

func (r potRepo) ClaimDue(_ context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	type cand struct {
		id   uuid.UUID
		next time.Time
	}
	var cands []cand
	for _, p := range r.s.pots {
		if p.ReviewReason != "" || p.Status == pots.StatusPendingApproval || p.Status == pots.StatusSettled || p.Status == pots.StatusRefunded {
			continue
		}
		if p.NextCheckAt.After(now) {
			continue
		}
		overdueOpen := p.Status == pots.StatusOpen && !p.Deadline.After(now)
		activeActionable := p.Status != pots.StatusOpen && !p.UpdatedAt.After(staleBefore)
		staleOpen := p.Status == pots.StatusOpen && !p.UpdatedAt.After(staleBefore)
		// Also allow immediate sweep when Status is FUNDED, EXPIRED, CANCELLED or REFUNDING and NextCheckAt <= now.
		immediateState := p.Status == pots.StatusFunded || p.Status == pots.StatusExpired || p.Status == pots.StatusCancelled || p.Status == pots.StatusRefunding
		if overdueOpen || activeActionable || staleOpen || immediateState {
			cands = append(cands, cand{id: p.ID, next: p.NextCheckAt})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].next.Equal(cands[j].next) {
			return cands[i].id.String() < cands[j].id.String()
		}
		return cands[i].next.Before(cands[j].next)
	})
	if len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]uuid.UUID, 0, len(cands))
	leasedUntil := now.Add(lease)
	for _, c := range cands {
		p := r.s.pots[c.id]
		prev := p.NextCheckAt
		p.NextCheckAt = leasedUntil
		id := c.id
		r.onUndo(func() {
			r.s.pots[id].NextCheckAt = prev
		})
		out = append(out, id)
	}
	return out, nil
}

func (r potRepo) CreateContribution(_ context.Context, c *pots.Contribution) error {
	defer r.lock()()
	if _, ok := r.s.pots[c.PotID]; !ok {
		return store.ErrNotFound
	}
	if c.PayerUserID != "" && !r.s.userIDs[c.PayerUserID] {
		return store.ErrNotFound
	}
	if _, exists := r.s.contribs[c.ID]; exists {
		return store.ErrDuplicate
	}
	if c.PaymentProviderRef != "" {
		if _, exists := r.s.contribByPayRef[c.PaymentProviderRef]; exists {
			return store.ErrDuplicate
		}
	}
	id := c.ID
	ref := c.PaymentProviderRef
	r.s.contribs[id] = copyContribution(c)
	if ref != "" {
		r.s.contribByPayRef[ref] = id
	}
	r.onUndo(func() {
		delete(r.s.contribs, id)
		if ref != "" {
			delete(r.s.contribByPayRef, ref)
		}
	})
	return nil
}

func (r potRepo) GetContribution(_ context.Context, id uuid.UUID) (*pots.Contribution, error) {
	defer r.lock()()
	c, ok := r.s.contribs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyContribution(c), nil
}

func (r potRepo) GetContributionForUpdate(ctx context.Context, id uuid.UUID) (*pots.Contribution, error) {
	return r.GetContribution(ctx, id)
}

func (r potRepo) ContributionByPaymentRef(_ context.Context, ref string) (*pots.Contribution, error) {
	defer r.lock()()
	if ref == "" {
		return nil, store.ErrNotFound
	}
	id, ok := r.s.contribByPayRef[ref]
	if !ok {
		return nil, store.ErrNotFound
	}
	c, ok := r.s.contribs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyContribution(c), nil
}

func (r potRepo) UpdateContribution(_ context.Context, c *pots.Contribution) error {
	defer r.lock()()
	prev, ok := r.s.contribs[c.ID]
	if !ok {
		return store.ErrNotFound
	}
	if c.PaymentProviderRef != "" && c.PaymentProviderRef != prev.PaymentProviderRef {
		if existingID, dup := r.s.contribByPayRef[c.PaymentProviderRef]; dup && existingID != c.ID {
			return store.ErrDuplicate
		}
	}
	prevCopy := copyContribution(prev)
	oldRef := prev.PaymentProviderRef
	newRef := c.PaymentProviderRef
	if oldRef != "" && oldRef != newRef {
		delete(r.s.contribByPayRef, oldRef)
	}
	if newRef != "" {
		r.s.contribByPayRef[newRef] = c.ID
	}
	r.s.contribs[c.ID] = copyContribution(c)
	r.onUndo(func() {
		if newRef != "" && newRef != oldRef {
			delete(r.s.contribByPayRef, newRef)
		}
		if oldRef != "" {
			r.s.contribByPayRef[oldRef] = c.ID
		}
		r.s.contribs[c.ID] = prevCopy
	})
	return nil
}

func (r potRepo) ListContributions(_ context.Context, potID uuid.UUID) ([]*pots.Contribution, error) {
	defer r.lock()()
	var out []*pots.Contribution
	for _, c := range r.s.contribs {
		if c.PotID == potID {
			out = append(out, copyContribution(c))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (r potRepo) ListContributionsForUpdate(ctx context.Context, potID uuid.UUID) ([]*pots.Contribution, error) {
	return r.ListContributions(ctx, potID)
}

func (r potRepo) CreateAllocation(_ context.Context, a *pots.Allocation) error {
	defer r.lock()()
	if _, ok := r.s.pots[a.PotID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := r.s.txns[a.TransactionID]; !ok {
		return store.ErrNotFound
	}
	if _, exists := r.s.allocs[a.ID]; exists {
		return store.ErrDuplicate
	}
	if _, exists := r.s.allocByTxn[a.TransactionID]; exists {
		return store.ErrDuplicate
	}
	id, tid := a.ID, a.TransactionID
	r.s.allocs[id] = copyAllocation(a)
	r.s.allocByTxn[tid] = id
	r.onUndo(func() {
		delete(r.s.allocs, id)
		delete(r.s.allocByTxn, tid)
	})
	return nil
}

func (r potRepo) GetAllocationByTxn(_ context.Context, txnID uuid.UUID) (*pots.Allocation, error) {
	defer r.lock()()
	id, ok := r.s.allocByTxn[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	a, ok := r.s.allocs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyAllocation(a), nil
}

func (r potRepo) GetAllocationByTxnForUpdate(ctx context.Context, txnID uuid.UUID) (*pots.Allocation, error) {
	return r.GetAllocationByTxn(ctx, txnID)
}

func (r potRepo) UpdateAllocation(_ context.Context, a *pots.Allocation) error {
	defer r.lock()()
	prev, ok := r.s.allocs[a.ID]
	if !ok {
		return store.ErrNotFound
	}
	prevCopy := copyAllocation(prev)
	r.s.allocs[a.ID] = copyAllocation(a)
	r.onUndo(func() {
		r.s.allocs[a.ID] = prevCopy
	})
	return nil
}

func (r potRepo) ListAllocations(_ context.Context, potID uuid.UUID) ([]*pots.Allocation, error) {
	defer r.lock()()
	var out []*pots.Allocation
	for _, a := range r.s.allocs {
		if a.PotID == potID {
			out = append(out, copyAllocation(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
