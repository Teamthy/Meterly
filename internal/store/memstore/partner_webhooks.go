package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

func cloneWebhook(ep partners.WebhookEndpoint) partners.WebhookEndpoint {
	out := ep
	out.Events = append([]string(nil), ep.Events...)
	if len(ep.SecretCiphertext) > 0 {
		out.SecretCiphertext = append([]byte(nil), ep.SecretCiphertext...)
	}
	if len(ep.PrevSecretCiphertext) > 0 {
		out.PrevSecretCiphertext = append([]byte(nil), ep.PrevSecretCiphertext...)
	}
	if ep.OverlapUntil != nil {
		t := *ep.OverlapUntil
		out.OverlapUntil = &t
	}
	return out
}

func cloneWebhookDelivery(d partners.WebhookDelivery) partners.WebhookDelivery {
	out := d
	if len(d.Payload) > 0 {
		out.Payload = append([]byte(nil), d.Payload...)
	}
	if d.FirstAttemptAt != nil {
		t := *d.FirstAttemptAt
		out.FirstAttemptAt = &t
	}
	if d.LastAttemptAt != nil {
		t := *d.LastAttemptAt
		out.LastAttemptAt = &t
	}
	return out
}

func (r partnerRepo) CreateWebhookEndpoint(_ context.Context, ep partners.WebhookEndpoint) error {
	defer r.lock()()
	if ep.ID == uuid.Nil {
		ep.ID = uuid.New()
	}
	if _, ok := r.s.partnerWebhooks[ep.ID]; ok {
		return store.ErrDuplicate
	}
	cp := cloneWebhook(ep)
	r.s.partnerWebhooks[ep.ID] = &cp
	r.onUndo(func() { delete(r.s.partnerWebhooks, ep.ID) })
	return nil
}

func (r partnerRepo) getWebhookLocked(id uuid.UUID) (partners.WebhookEndpoint, error) {
	ep, ok := r.s.partnerWebhooks[id]
	if !ok {
		return partners.WebhookEndpoint{}, store.ErrNotFound
	}
	return cloneWebhook(*ep), nil
}

func (r partnerRepo) GetWebhookEndpoint(_ context.Context, id uuid.UUID) (partners.WebhookEndpoint, error) {
	defer r.lock()()
	return r.getWebhookLocked(id)
}

func (r partnerRepo) GetWebhookEndpointForUpdate(ctx context.Context, id uuid.UUID) (partners.WebhookEndpoint, error) {
	return r.GetWebhookEndpoint(ctx, id)
}

func (r partnerRepo) ListWebhookEndpoints(_ context.Context, partnerID uuid.UUID) ([]partners.WebhookEndpoint, error) {
	defer r.lock()()
	out := make([]partners.WebhookEndpoint, 0)
	for _, ep := range r.s.partnerWebhooks {
		if ep.PartnerID == partnerID {
			out = append(out, cloneWebhook(*ep))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r partnerRepo) UpdateWebhookEndpoint(_ context.Context, ep partners.WebhookEndpoint) error {
	defer r.lock()()
	cur, ok := r.s.partnerWebhooks[ep.ID]
	if !ok {
		return store.ErrNotFound
	}
	prev := cloneWebhook(*cur)
	cp := cloneWebhook(ep)
	*cur = cp
	r.onUndo(func() { *cur = prev })
	return nil
}

func (r partnerRepo) InsertWebhookDelivery(_ context.Context, d partners.WebhookDelivery) error {
	defer r.lock()()
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	for _, existing := range r.s.partnerDeliveries {
		if existing.EndpointID == d.EndpointID && existing.EventID == d.EventID {
			return nil
		}
	}
	if _, ok := r.s.partnerDeliveries[d.ID]; ok {
		return nil
	}
	cp := cloneWebhookDelivery(d)
	r.s.partnerDeliveries[d.ID] = &cp
	r.onUndo(func() { delete(r.s.partnerDeliveries, d.ID) })
	return nil
}

func (r partnerRepo) getDeliveryLocked(id uuid.UUID) (partners.WebhookDelivery, error) {
	d, ok := r.s.partnerDeliveries[id]
	if !ok {
		return partners.WebhookDelivery{}, store.ErrNotFound
	}
	return cloneWebhookDelivery(*d), nil
}

func (r partnerRepo) GetWebhookDelivery(_ context.Context, id uuid.UUID) (partners.WebhookDelivery, error) {
	defer r.lock()()
	return r.getDeliveryLocked(id)
}

func (r partnerRepo) GetWebhookDeliveryForUpdate(ctx context.Context, id uuid.UUID) (partners.WebhookDelivery, error) {
	return r.GetWebhookDelivery(ctx, id)
}

func (r partnerRepo) ListWebhookDeliveries(_ context.Context, partnerID, endpointID uuid.UUID, limit int) ([]partners.WebhookDelivery, error) {
	defer r.lock()()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := make([]partners.WebhookDelivery, 0)
	for _, d := range r.s.partnerDeliveries {
		if d.PartnerID == partnerID && d.EndpointID == endpointID {
			out = append(out, cloneWebhookDelivery(*d))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r partnerRepo) ListDueWebhookDeliveries(_ context.Context, now, staleDeliveringBefore time.Time, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	type row struct {
		id uuid.UUID
		at time.Time
	}
	var due []row
	for _, d := range r.s.partnerDeliveries {
		switch d.Status {
		case partners.DeliveryPending, partners.DeliveryFailed:
			if !d.NextAttemptAt.After(now) {
				due = append(due, row{d.ID, d.NextAttemptAt})
			}
		case partners.DeliveryDelivering:
			if d.LastAttemptAt == nil || !d.LastAttemptAt.After(staleDeliveringBefore) {
				at := now
				if d.LastAttemptAt != nil {
					at = *d.LastAttemptAt
				}
				due = append(due, row{d.ID, at})
			}
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].at.Equal(due[j].at) {
			return due[i].id.String() < due[j].id.String()
		}
		return due[i].at.Before(due[j].at)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]uuid.UUID, len(due))
	for i, r := range due {
		out[i] = r.id
	}
	return out, nil
}

func (r partnerRepo) UpdateWebhookDelivery(_ context.Context, d partners.WebhookDelivery) error {
	defer r.lock()()
	cur, ok := r.s.partnerDeliveries[d.ID]
	if !ok {
		return store.ErrNotFound
	}
	prev := cloneWebhookDelivery(*cur)
	cp := cloneWebhookDelivery(d)
	*cur = cp
	r.onUndo(func() { *cur = prev })
	return nil
}
