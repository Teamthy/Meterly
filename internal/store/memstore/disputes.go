package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/money"
)

type disputeRepo struct{ *repos }

func cloneDispute(d disputes.Dispute) disputes.Dispute {
	out := d
	if len(d.Evidence) > 0 {
		out.Evidence = append(json.RawMessage(nil), d.Evidence...)
	}
	return out
}

func (r disputeRepo) Upsert(_ context.Context, d disputes.Dispute) (disputes.Dispute, error) {
	defer r.lock()()
	now := time.Now().UTC()
	if d.PSPDisputeID != "" {
		if id, ok := r.s.disputesByPSP[d.PSPDisputeID]; ok {
			cur := r.s.disputes[id]
			prev := *cur
			if d.TransactionID != uuid.Nil {
				cur.TransactionID = d.TransactionID
			}
			if d.PaymentRef != "" {
				cur.PaymentRef = d.PaymentRef
			}
			if d.Amount.Minor > 0 {
				cur.Amount = d.Amount
			}
			if d.Status != "" {
				cur.Status = d.Status
			}
			if d.PSPStatus != "" {
				cur.PSPStatus = d.PSPStatus
			}
			if d.Resolution != "" {
				cur.Resolution = d.Resolution
			}
			if d.Category != "" {
				cur.Category = d.Category
			}
			if !d.DueAt.IsZero() {
				cur.DueAt = d.DueAt
			}
			if !d.RemindedAt.IsZero() {
				cur.RemindedAt = d.RemindedAt
			}
			if !d.ResolvedAt.IsZero() {
				cur.ResolvedAt = d.ResolvedAt
			}
			if len(d.Evidence) > 0 {
				cur.Evidence = append(json.RawMessage(nil), d.Evidence...)
			}
			cur.UpdatedAt = now
			r.onUndo(func() { cp := prev; r.s.disputes[id] = &cp })
			return cloneDispute(*cur), nil
		}
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = disputes.StatusOpen
	}
	if d.Amount.Currency == "" {
		d.Amount.Currency = money.NGN
	}
	cp := d
	r.s.disputes[d.ID] = &cp
	if d.PSPDisputeID != "" {
		r.s.disputesByPSP[d.PSPDisputeID] = d.ID
	}
	r.onUndo(func() {
		delete(r.s.disputes, d.ID)
		delete(r.s.disputesByPSP, d.PSPDisputeID)
	})
	return cloneDispute(d), nil
}

func (r disputeRepo) Get(_ context.Context, id uuid.UUID) (disputes.Dispute, error) {
	defer r.lock()()
	d, ok := r.s.disputes[id]
	if !ok {
		return disputes.Dispute{}, disputes.ErrNotFound
	}
	return cloneDispute(*d), nil
}

func (r disputeRepo) ByPSP(_ context.Context, pspID string) (disputes.Dispute, error) {
	defer r.lock()()
	id, ok := r.s.disputesByPSP[pspID]
	if !ok {
		return disputes.Dispute{}, disputes.ErrNotFound
	}
	return cloneDispute(*r.s.disputes[id]), nil
}

func (r disputeRepo) List(_ context.Context, f disputes.Filter) ([]disputes.Dispute, error) {
	defer r.lock()()
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []disputes.Dispute
	for _, d := range r.s.disputes {
		if f.Status != "" && d.Status != f.Status {
			continue
		}
		out = append(out, cloneDispute(*d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r disputeRepo) CountLostForUser(_ context.Context, userID string) (int, error) {
	defer r.lock()()
	n := 0
	for _, d := range r.s.disputes {
		if d.Status != disputes.StatusLost && d.Status != disputes.StatusAccepted {
			continue
		}
		if d.TransactionID == uuid.Nil {
			continue
		}
		t, ok := r.s.txns[d.TransactionID]
		if ok && t.CustomerID == userID {
			n++
		}
	}
	return n, nil
}

func (r disputeRepo) SetEvidence(_ context.Context, id uuid.UUID, raw json.RawMessage) error {
	defer r.lock()()
	d, ok := r.s.disputes[id]
	if !ok {
		return disputes.ErrNotFound
	}
	prev := append(json.RawMessage(nil), d.Evidence...)
	d.Evidence = append(json.RawMessage(nil), raw...)
	d.UpdatedAt = time.Now().UTC()
	r.onUndo(func() { d.Evidence = prev })
	return nil
}
