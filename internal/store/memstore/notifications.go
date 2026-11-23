package memstore

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/store"
)

func cloneUserContacts(c notifications.UserContacts) notifications.UserContacts {
	out := c
	if c.EmailCiphertext != nil {
		out.EmailCiphertext = append([]byte(nil), c.EmailCiphertext...)
	}
	if c.PhoneCiphertext != nil {
		out.PhoneCiphertext = append([]byte(nil), c.PhoneCiphertext...)
	}
	return out
}

func (r *repos) UpsertContacts(_ context.Context, c notifications.UserContacts) error {
	defer r.lock()()
	if c.UserID == "" || !r.s.userIDs[c.UserID] {
		return store.ErrNotFound
	}
	prev, had := r.s.userContacts[c.UserID]
	next := cloneUserContacts(prev)
	next.UserID = c.UserID
	if len(c.EmailCiphertext) > 0 {
		next.EmailCiphertext = append([]byte(nil), c.EmailCiphertext...)
		next.EmailHMAC = c.EmailHMAC
		next.EmailMasked = c.EmailMasked
		next.EmailKeyVersion = c.EmailKeyVersion
	}
	if len(c.PhoneCiphertext) > 0 {
		next.PhoneCiphertext = append([]byte(nil), c.PhoneCiphertext...)
		next.PhoneHMAC = c.PhoneHMAC
		next.PhoneMasked = c.PhoneMasked
		next.PhoneKeyVersion = c.PhoneKeyVersion
	}
	if c.DisplayName != "" {
		next.DisplayName = c.DisplayName
	}
	if !c.UpdatedAt.IsZero() {
		next.UpdatedAt = c.UpdatedAt.UTC()
	} else {
		next.UpdatedAt = time.Now().UTC()
	}
	r.s.userContacts[c.UserID] = next
	r.onUndo(func() {
		if had {
			r.s.userContacts[c.UserID] = prev
		} else {
			delete(r.s.userContacts, c.UserID)
		}
	})
	return nil
}

func (r *repos) Contacts(_ context.Context, userID string) (notifications.UserContacts, error) {
	defer r.lock()()
	if userID == "" || !r.s.userIDs[userID] {
		return notifications.UserContacts{}, store.ErrNotFound
	}
	c, ok := r.s.userContacts[userID]
	if !ok {
		return notifications.UserContacts{UserID: userID}, nil
	}
	return cloneUserContacts(c), nil
}

// ---- notifications ------------------------------------------------------------

type notifRepo struct{ *repos }

func cloneDelivery(d *notifications.Delivery) *notifications.Delivery {
	cp := *d
	cp.NextAttemptAt = d.NextAttemptAt.UTC()
	cp.CreatedAt = d.CreatedAt.UTC()
	cp.UpdatedAt = d.UpdatedAt.UTC()
	if d.SentAt != nil {
		t := d.SentAt.UTC()
		cp.SentAt = &t
	}
	return &cp
}

func deliveryUniqueKey(eventID uuid.UUID, kind notifications.RecipientKind, ch notifications.Channel) string {
	return eventID.String() + "|" + string(kind) + "|" + string(ch)
}

func optOutKey(destHMAC string, ch notifications.Channel) string {
	return destHMAC + "|" + string(ch)
}

func (r notifRepo) GetPreferences(_ context.Context, userID string) (notifications.Preferences, error) {
	defer r.lock()()
	if userID == "" || !r.s.userIDs[userID] {
		return notifications.Preferences{}, store.ErrNotFound
	}
	if p, ok := r.s.notifPrefs[userID]; ok {
		return p, nil
	}
	return notifications.DefaultPreferences(), nil
}

func (r notifRepo) SetPreferences(_ context.Context, userID string, p notifications.Preferences, _ time.Time) error {
	defer r.lock()()
	if userID == "" || !r.s.userIDs[userID] {
		return store.ErrNotFound
	}
	prev, had := r.s.notifPrefs[userID]
	r.s.notifPrefs[userID] = p
	r.onUndo(func() {
		if had {
			r.s.notifPrefs[userID] = prev
		} else {
			delete(r.s.notifPrefs, userID)
		}
	})
	return nil
}

func (r notifRepo) AddOptOut(_ context.Context, o notifications.OptOut) (bool, error) {
	defer r.lock()()
	if o.DestinationHMAC == "" || o.Channel == "" {
		return false, errors.New("notifications: opt-out requires destination_hmac and channel")
	}
	k := optOutKey(o.DestinationHMAC, o.Channel)
	if _, exists := r.s.optOuts[k]; exists {
		return false, nil
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	} else {
		o.CreatedAt = o.CreatedAt.UTC()
	}
	r.s.optOuts[k] = o
	r.onUndo(func() { delete(r.s.optOuts, k) })
	return true, nil
}

func (r notifRepo) IsOptedOut(_ context.Context, destinationHMAC string, ch notifications.Channel) (bool, error) {
	defer r.lock()()
	if destinationHMAC == "" {
		return false, nil
	}
	_, ok := r.s.optOuts[optOutKey(destinationHMAC, ch)]
	return ok, nil
}

func (r notifRepo) RecordWhatsAppInbound(_ context.Context, destinationHMAC string, at time.Time) error {
	defer r.lock()()
	if destinationHMAC == "" {
		return errors.New("notifications: destination_hmac required")
	}
	prev, had := r.s.waWindows[destinationHMAC]
	ts := at.UTC()
	if had && prev.After(ts) {
		return nil
	}
	r.s.waWindows[destinationHMAC] = ts
	r.onUndo(func() {
		if had {
			r.s.waWindows[destinationHMAC] = prev
		} else {
			delete(r.s.waWindows, destinationHMAC)
		}
	})
	return nil
}

func (r notifRepo) HasActiveWhatsAppWindow(_ context.Context, destinationHMAC string, now time.Time) (bool, error) {
	defer r.lock()()
	last, ok := r.s.waWindows[destinationHMAC]
	if !ok {
		return false, nil
	}
	age := now.UTC().Sub(last)
	return age >= 0 && age < 24*time.Hour, nil
}

func (r notifRepo) CreateDelivery(_ context.Context, d *notifications.Delivery) (bool, error) {
	defer r.lock()()
	if _, ok := r.s.txns[d.TransactionID]; !ok {
		return false, store.ErrNotFound
	}
	uk := deliveryUniqueKey(d.EventID, d.RecipientKind, d.Channel)
	if existingID, ok := r.s.deliveryByKey[uk]; ok {
		*d = *cloneDelivery(r.s.deliveries[existingID])
		return false, nil
	}
	cp := cloneDelivery(d)
	if cp.ID == uuid.Nil {
		cp.ID = uuid.New()
	}
	now := time.Now().UTC()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if cp.NextAttemptAt.IsZero() {
		cp.NextAttemptAt = cp.CreatedAt
	}
	if cp.Locale == "" {
		cp.Locale = notifications.LocaleEN
	}
	r.s.deliveries[cp.ID] = cp
	r.s.deliveryByKey[uk] = cp.ID
	id := cp.ID
	r.onUndo(func() {
		delete(r.s.deliveries, id)
		delete(r.s.deliveryByKey, uk)
	})
	*d = *cloneDelivery(cp)
	return true, nil
}

func (r notifRepo) GetDelivery(_ context.Context, id uuid.UUID) (*notifications.Delivery, error) {
	defer r.lock()()
	d, ok := r.s.deliveries[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneDelivery(d), nil
}

func (r notifRepo) GetDeliveryForUpdate(ctx context.Context, id uuid.UUID) (*notifications.Delivery, error) {
	if !r.inTx {
		return nil, errors.New("delivery lock requires transaction")
	}
	return r.GetDelivery(ctx, id)
}

func (r notifRepo) UpdateDelivery(_ context.Context, d *notifications.Delivery) error {
	defer r.lock()()
	prev, ok := r.s.deliveries[d.ID]
	if !ok {
		return store.ErrNotFound
	}
	cp := cloneDelivery(d)
	r.s.deliveries[d.ID] = cp
	r.onUndo(func() { r.s.deliveries[d.ID] = prev })
	return nil
}

func (r notifRepo) ListDueDeliveries(_ context.Context, now, staleSendingBefore time.Time, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	now = now.UTC()
	staleSendingBefore = staleSendingBefore.UTC()
	var due []*notifications.Delivery
	for _, d := range r.s.deliveries {
		switch d.Status {
		case notifications.StatusPending:
			if !d.NextAttemptAt.After(now) {
				due = append(due, d)
			}
		case notifications.StatusUnknown:
			if !d.TokenBearing() && d.Attempts < 2 && !d.NextAttemptAt.After(now) {
				due = append(due, d)
			} else if d.ProviderRef != "" && !d.NextAttemptAt.After(now) {
				due = append(due, d)
			}
		case notifications.StatusSending:
			if !staleSendingBefore.IsZero() && !d.UpdatedAt.After(staleSendingBefore) {
				due = append(due, d)
			}
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextAttemptAt.Equal(due[j].NextAttemptAt) {
			return due[i].ID.String() < due[j].ID.String()
		}
		return due[i].NextAttemptAt.Before(due[j].NextAttemptAt)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	ids := make([]uuid.UUID, len(due))
	for i, d := range due {
		ids[i] = d.ID
	}
	return ids, nil
}

func (r notifRepo) ForTransaction(_ context.Context, txnID uuid.UUID) ([]notifications.Delivery, error) {
	defer r.lock()()
	var out []notifications.Delivery
	for _, d := range r.s.deliveries {
		if d.TransactionID == txnID {
			out = append(out, *cloneDelivery(d))
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

func (r notifRepo) PurgeOldDestinationHMACs(_ context.Context, cutoff time.Time) (int, error) {
	defer r.lock()()
	cutoff = cutoff.UTC()
	var purged int
	type prevEntry struct {
		id   uuid.UUID
		hmac string
		upd  time.Time
	}
	var prevs []prevEntry
	now := time.Now().UTC()
	for id, d := range r.s.deliveries {
		if d.DestinationHMAC == "" || !d.Status.Terminal() {
			continue
		}
		if d.CreatedAt.Before(cutoff) {
			prevs = append(prevs, prevEntry{id: id, hmac: d.DestinationHMAC, upd: d.UpdatedAt})
			cp := cloneDelivery(d)
			cp.DestinationHMAC = ""
			cp.UpdatedAt = now
			r.s.deliveries[id] = cp
			purged++
		}
	}
	if len(prevs) > 0 {
		r.onUndo(func() {
			for _, p := range prevs {
				if cur, ok := r.s.deliveries[p.id]; ok {
					cp := cloneDelivery(cur)
					cp.DestinationHMAC = p.hmac
					cp.UpdatedAt = p.upd
					r.s.deliveries[p.id] = cp
				}
			}
		})
	}
	return purged, nil
}
