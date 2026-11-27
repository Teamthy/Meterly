package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/store"
)

type offlineRepo struct{ *repos }

func copyPhoneLink(l *conversation.PhoneLink) *conversation.PhoneLink {
	cp := *l
	if l.VerifiedAt != nil {
		t := *l.VerifiedAt
		cp.VerifiedAt = &t
	}
	return &cp
}

func copyOTP(o *conversation.ChannelOTP) *conversation.ChannelOTP {
	cp := *o
	if o.ConsumedAt != nil {
		t := *o.ConsumedAt
		cp.ConsumedAt = &t
	}
	return &cp
}

func copySession(s *conversation.Session) *conversation.Session {
	cp := *s
	if len(s.MeterOptions) > 0 {
		cp.MeterOptions = make([]conversation.MeterOption, len(s.MeterOptions))
		copy(cp.MeterOptions, s.MeterOptions)
	}
	if s.SelectedMeterID != nil {
		id := *s.SelectedMeterID
		cp.SelectedMeterID = &id
	}
	return &cp
}

func copyOfflinePurchase(p *conversation.OfflinePurchase) *conversation.OfflinePurchase {
	cp := *p
	return &cp
}

func convProvKey(ch conversation.Channel, providerSessionID string) string {
	return string(ch) + ":" + providerSessionID
}

func (r offlineRepo) FindUserIDByPhoneHMAC(_ context.Context, phoneHMAC string) (string, error) {
	defer r.lock()()
	if phoneHMAC == "" {
		return "", store.ErrNotFound
	}
	var uids []string
	for uid, c := range r.s.userContacts {
		if c.PhoneHMAC == phoneHMAC {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return "", store.ErrNotFound
	}
	sort.Strings(uids)
	return uids[0], nil
}

func (r offlineRepo) GetPhoneLink(_ context.Context, phoneHMAC string) (*conversation.PhoneLink, error) {
	defer r.lock()()
	l, ok := r.s.phoneLinks[phoneHMAC]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyPhoneLink(l), nil
}

func (r offlineRepo) GetPhoneLinkForUpdate(ctx context.Context, phoneHMAC string) (*conversation.PhoneLink, error) {
	return r.GetPhoneLink(ctx, phoneHMAC)
}

func (r offlineRepo) UpsertPhoneLink(_ context.Context, link *conversation.PhoneLink) error {
	defer r.lock()()
	if !r.s.userIDs[link.UserID] {
		return store.ErrNotFound
	}
	prev, had := r.s.phoneLinks[link.PhoneHMAC]
	r.s.phoneLinks[link.PhoneHMAC] = copyPhoneLink(link)
	r.onUndo(func() {
		if had {
			r.s.phoneLinks[link.PhoneHMAC] = prev
		} else {
			delete(r.s.phoneLinks, link.PhoneHMAC)
		}
	})
	return nil
}

func (r offlineRepo) CreateOTP(_ context.Context, otp *conversation.ChannelOTP) error {
	defer r.lock()()
	if _, exists := r.s.channelOTPs[otp.ID]; exists {
		return store.ErrDuplicate
	}
	r.s.channelOTPs[otp.ID] = copyOTP(otp)
	r.onUndo(func() { delete(r.s.channelOTPs, otp.ID) })
	return nil
}

func (r offlineRepo) GetLatestOTPForUpdate(_ context.Context, phoneHMAC string) (*conversation.ChannelOTP, error) {
	defer r.lock()()
	var best *conversation.ChannelOTP
	for _, o := range r.s.channelOTPs {
		if o.PhoneHMAC != phoneHMAC {
			continue
		}
		if best == nil || o.CreatedAt.After(best.CreatedAt) || (o.CreatedAt.Equal(best.CreatedAt) && o.ID.String() > best.ID.String()) {
			best = o
		}
	}
	if best == nil {
		return nil, store.ErrNotFound
	}
	return copyOTP(best), nil
}

func (r offlineRepo) UpdateOTP(_ context.Context, otp *conversation.ChannelOTP) error {
	defer r.lock()()
	prev, ok := r.s.channelOTPs[otp.ID]
	if !ok {
		return store.ErrNotFound
	}
	r.s.channelOTPs[otp.ID] = copyOTP(otp)
	r.onUndo(func() { r.s.channelOTPs[otp.ID] = prev })
	return nil
}

func (r offlineRepo) CreateSession(_ context.Context, s *conversation.Session) error {
	defer r.lock()()
	if _, exists := r.s.convSessions[s.ID]; exists {
		return store.ErrDuplicate
	}
	pk := convProvKey(s.Channel, s.ProviderSessionID)
	if _, exists := r.s.convSessionByProv[pk]; exists {
		return store.ErrDuplicate
	}
	r.s.convSessions[s.ID] = copySession(s)
	r.s.convSessionByProv[pk] = s.ID
	r.onUndo(func() {
		delete(r.s.convSessions, s.ID)
		delete(r.s.convSessionByProv, pk)
	})
	return nil
}

func (r offlineRepo) GetSessionByProviderIDForUpdate(_ context.Context, ch conversation.Channel, providerSessionID string) (*conversation.Session, error) {
	defer r.lock()()
	id, ok := r.s.convSessionByProv[convProvKey(ch, providerSessionID)]
	if !ok {
		return nil, store.ErrNotFound
	}
	s, ok := r.s.convSessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copySession(s), nil
}

func (r offlineRepo) GetActiveSessionForPhoneForUpdate(_ context.Context, ch conversation.Channel, phoneHMAC string, now time.Time) (*conversation.Session, error) {
	defer r.lock()()
	var best *conversation.Session
	for _, s := range r.s.convSessions {
		if s.Channel != ch || s.PhoneHMAC != phoneHMAC || s.Expired(now) {
			continue
		}
		if best == nil || s.UpdatedAt.After(best.UpdatedAt) || (s.UpdatedAt.Equal(best.UpdatedAt) && s.ID.String() > best.ID.String()) {
			best = s
		}
	}
	if best == nil {
		return nil, store.ErrNotFound
	}
	return copySession(best), nil
}

func (r offlineRepo) UpdateSession(_ context.Context, s *conversation.Session) error {
	defer r.lock()()
	prev, ok := r.s.convSessions[s.ID]
	if !ok {
		return store.ErrNotFound
	}
	r.s.convSessions[s.ID] = copySession(s)
	r.onUndo(func() { r.s.convSessions[s.ID] = prev })
	return nil
}

func (r offlineRepo) RecordProcessedMessage(_ context.Context, ch conversation.Channel, messageID, _ string, at time.Time) (bool, error) {
	defer r.lock()()
	k := convProvKey(ch, messageID)
	if _, exists := r.s.processedMsgs[k]; exists {
		return false, nil
	}
	r.s.processedMsgs[k] = at
	r.onUndo(func() { delete(r.s.processedMsgs, k) })
	return true, nil
}

func (r offlineRepo) RecordPurchase(_ context.Context, p *conversation.OfflinePurchase) error {
	defer r.lock()()
	if _, exists := r.s.offlinePurchases[p.TransactionID]; exists {
		return store.ErrDuplicate
	}
	r.s.offlinePurchases[p.TransactionID] = copyOfflinePurchase(p)
	r.onUndo(func() { delete(r.s.offlinePurchases, p.TransactionID) })
	return nil
}

func (r offlineRepo) GetPurchase(_ context.Context, txnID uuid.UUID) (*conversation.OfflinePurchase, error) {
	defer r.lock()()
	p, ok := r.s.offlinePurchases[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyOfflinePurchase(p), nil
}

func (r offlineRepo) SumPhoneSpendMinor(_ context.Context, phoneHMAC string, startUTC, endUTC time.Time) (int64, error) {
	defer r.lock()()
	var sum int64
	for _, p := range r.s.offlinePurchases {
		if p.PhoneHMAC != phoneHMAC {
			continue
		}
		if p.CreatedAt.Before(startUTC) || !p.CreatedAt.Before(endUTC) {
			continue
		}
		// Count unless the underlying transaction failed payment or was refunded.
		if t, ok := r.s.txns[p.TransactionID]; ok {
			if t.Payment == "FAILED" || t.Payment == "REFUNDED" {
				continue
			}
		}
		sum += p.AmountMinor
	}
	return sum, nil
}

func (r offlineRepo) RecordTokenResend(_ context.Context, tr *conversation.TokenResend) error {
	defer r.lock()()
	cp := *tr
	if cp.ID == uuid.Nil {
		cp.ID = uuid.New()
	}
	n := len(r.s.tokenResends)
	r.s.tokenResends = append(r.s.tokenResends, &cp)
	r.onUndo(func() { r.s.tokenResends = r.s.tokenResends[:n] })
	return nil
}

func (r offlineRepo) CountTokenResends(_ context.Context, userID string, startUTC, endUTC time.Time) (int, error) {
	defer r.lock()()
	cnt := 0
	for _, tr := range r.s.tokenResends {
		if tr.UserID != userID {
			continue
		}
		if !tr.CreatedAt.Before(startUTC) && tr.CreatedAt.Before(endUTC) {
			cnt++
		}
	}
	return cnt, nil
}
