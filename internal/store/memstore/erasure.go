package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (r privacyRepo) EnsureTombstone(_ context.Context) (string, error) {
	defer r.lock()()
	id := privacy.TombstoneUserID.String()
	if r.s.userIDs[id] {
		return id, nil
	}
	ext := privacy.TombstoneExternalID
	r.s.users[ext] = id
	r.s.userIDs[id] = true
	r.s.userExt[id] = ext
	r.s.userStatus[id] = store.UserErased
	r.onUndo(func() {
		delete(r.s.users, ext)
		delete(r.s.userIDs, id)
		delete(r.s.userExt, id)
		delete(r.s.userStatus, id)
	})
	return id, nil
}

func (r privacyRepo) CreateErasure(_ context.Context, job privacy.ErasureRequest) error {
	defer r.lock()()
	if job.ID == uuid.Nil || job.UserID == "" {
		return store.ErrNotFound
	}
	if _, ok := r.s.erasureReqs[job.ID]; ok {
		return store.ErrDuplicate
	}
	if privacy.ErasureOpen(job.Status) {
		if _, ok := r.s.erasureByUser[job.UserID]; ok {
			return store.ErrDuplicate
		}
		r.s.erasureByUser[job.UserID] = job.ID
	}
	r.s.erasureReqs[job.ID] = privacy.CloneErasure(job)
	r.onUndo(func() {
		delete(r.s.erasureReqs, job.ID)
		if privacy.ErasureOpen(job.Status) {
			delete(r.s.erasureByUser, job.UserID)
		}
	})
	return nil
}

func (r privacyRepo) GetErasure(_ context.Context, id uuid.UUID) (privacy.ErasureRequest, error) {
	defer r.lock()()
	j, ok := r.s.erasureReqs[id]
	if !ok {
		return privacy.ErasureRequest{}, store.ErrNotFound
	}
	return privacy.CloneErasure(j), nil
}

func (r privacyRepo) GetOpenErasureForUser(_ context.Context, userID string) (privacy.ErasureRequest, error) {
	defer r.lock()()
	id, ok := r.s.erasureByUser[userID]
	if !ok {
		return privacy.ErasureRequest{}, store.ErrNotFound
	}
	j, ok := r.s.erasureReqs[id]
	if !ok || !privacy.ErasureOpen(j.Status) {
		return privacy.ErasureRequest{}, store.ErrNotFound
	}
	return privacy.CloneErasure(j), nil
}

func (r privacyRepo) UpdateErasure(_ context.Context, job privacy.ErasureRequest) error {
	defer r.lock()()
	prev, ok := r.s.erasureReqs[job.ID]
	if !ok {
		return store.ErrNotFound
	}
	oldUser := prev.UserID
	r.s.erasureReqs[job.ID] = privacy.CloneErasure(job)
	if privacy.ErasureOpen(prev.Status) && r.s.erasureByUser[oldUser] == job.ID {
		delete(r.s.erasureByUser, oldUser)
	}
	if privacy.ErasureOpen(job.Status) {
		r.s.erasureByUser[job.UserID] = job.ID
	}
	r.onUndo(func() {
		r.s.erasureReqs[job.ID] = prev
		if privacy.ErasureOpen(job.Status) && r.s.erasureByUser[job.UserID] == job.ID {
			delete(r.s.erasureByUser, job.UserID)
		}
		if privacy.ErasureOpen(prev.Status) {
			r.s.erasureByUser[oldUser] = job.ID
		}
	})
	return nil
}

func (r privacyRepo) ListDueErasures(_ context.Context, now time.Time, limit int) ([]privacy.ErasureRequest, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 20
	}
	now = now.UTC()
	var out []privacy.ErasureRequest
	for _, j := range r.s.erasureReqs {
		switch j.Status {
		case privacy.ErasureCoolingOff:
			if !j.DueAt.After(now) {
				out = append(out, privacy.CloneErasure(j))
			}
		case privacy.ErasureReady, privacy.ErasureRunning, privacy.ErasureBlocked, privacy.ErasureFailed:
			out = append(out, privacy.CloneErasure(j))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r privacyRepo) CountOpenDisputesForUser(_ context.Context, userID string) (int, error) {
	defer r.lock()()
	n := 0
	for _, d := range r.s.disputes {
		if d == nil || (d.Status != disputes.StatusOpen && d.Status != disputes.StatusReminded) {
			continue
		}
		t, ok := r.s.txns[d.TransactionID]
		if ok && t != nil && t.CustomerID == userID {
			n++
		}
	}
	return n, nil
}

func (r privacyRepo) CountUnresolvedTxnsForUser(_ context.Context, userID string) (int, error) {
	defer r.lock()()
	n := 0
	for _, t := range r.s.txns {
		if t == nil || t.CustomerID != userID {
			continue
		}
		switch t.Status() {
		case tx.OverallManualReview, tx.OverallReconciliation:
			n++
		}
	}
	return n, nil
}

func (r privacyRepo) CountActivePotsForUser(_ context.Context, userID string) (int, error) {
	defer r.lock()()
	n := 0
	for _, p := range r.s.pots {
		if p == nil || p.OwnerID != userID {
			continue
		}
		switch p.Status {
		case "FUNDED", "ALLOCATING", "REFUNDING":
			n++
		case "OPEN":
			for _, c := range r.s.contribs {
				if c != nil && c.PotID == p.ID && (c.Status == "PAID" || c.Status == "REFUND_PENDING") {
					n++
					break
				}
			}
		}
	}
	return n, nil
}

func (r privacyRepo) CountUnpaidObligationsForUser(_ context.Context, userID string) (int, error) {
	defer r.lock()()
	n := 0
	for _, p := range r.s.payReqs {
		if p != nil && p.RequesterID == userID && p.Status == "OPEN" {
			n++
		}
	}
	for _, c := range r.s.contribs {
		if c == nil || c.PayerUserID != userID {
			continue
		}
		if c.Status != "PAID" && c.Status != "REFUND_PENDING" {
			continue
		}
		p := r.s.pots[c.PotID]
		if p == nil {
			continue
		}
		switch p.Status {
		case "SETTLED", "REFUNDED", "CANCELLED":
		default:
			n++
		}
	}
	return n, nil
}

func (r privacyRepo) ApplyErasureStep(_ context.Context, userID, tombstoneID, step string, now time.Time) error {
	defer r.lock()()
	if userID == "" || tombstoneID == "" {
		return store.ErrNotFound
	}
	now = now.UTC()
	switch step {
	case privacy.ErasureStepFinancial:
		r.eraseFinancial(userID, tombstoneID)
	case privacy.ErasureStepIdentity:
		r.eraseIdentity(userID, now)
	case privacy.ErasureStepMemberships:
		r.eraseMemberships(userID, now)
	case privacy.ErasureStepSupport:
		r.eraseSupport(userID)
	case privacy.ErasureStepNotify:
		r.eraseNotify(userID)
	case privacy.ErasureStepCards:
		r.eraseCards(userID, now)
	case privacy.ErasureStepExports:
		r.eraseExports(userID)
	default:
		return store.ErrNotFound
	}
	return nil
}

func (r privacyRepo) eraseFinancial(userID, tombstoneID string) {
	for _, q := range r.s.quotes {
		if q != nil && q.CustomerID == userID {
			q.CustomerID = tombstoneID
		}
	}
	for _, t := range r.s.txns {
		if t == nil || t.CustomerID != userID {
			continue
		}
		t.CustomerID = tombstoneID
		t.RecipientName = ""
	}
	for id, g := range r.s.gifts {
		if g.PurchaserID != userID {
			continue
		}
		g.PurchaserID = tombstoneID
		g.RecipientName = ""
		g.ContactCiphertext = nil
		g.ContactKeyVersion = ""
		g.NotificationRequested = false
		r.s.gifts[id] = g
	}
	for i, sh := range r.s.tokenShares {
		if sh.UserID == userID {
			sh.UserID = tombstoneID
			sh.DestHMAC = ""
			sh.DestCipher = nil
			sh.DestKeyVer = ""
			sh.DestMasked = ""
			r.s.tokenShares[i] = sh
		}
	}
	for _, p := range r.s.pots {
		if p != nil && p.OwnerID == userID {
			p.OwnerID = tombstoneID
			p.NoteCiphertext = nil
			p.NoteKeyVersion = ""
		}
	}
	for _, c := range r.s.contribs {
		if c == nil || c.PayerUserID != userID {
			continue
		}
		c.PayerUserID = tombstoneID
		c.EmailCiphertext = nil
		c.NameCiphertext = nil
		c.PhoneCiphertext = nil
		c.Initials = ""
	}
	for _, p := range r.s.payReqs {
		if p != nil && p.RequesterID == userID {
			p.RequesterID = tombstoneID
			p.NoteCiphertext = nil
			p.NoteKeyVersion = ""
		}
	}
}

func (r privacyRepo) eraseIdentity(userID string, now time.Time) {
	ext := r.s.userExt[userID]
	if ext != "" && ext != privacy.TombstoneExternalID {
		delete(r.s.users, ext)
	}
	newExt := "erased:" + userID
	r.s.users[newExt] = userID
	r.s.userExt[userID] = newExt
	r.s.userStatus[userID] = store.UserErased
	p := r.s.userProfiles[userID]
	p.DisplayName = ""
	p.EmailMasked = ""
	p.PhoneMasked = ""
	p.ExternalID = newExt
	r.s.userProfiles[userID] = p
	delete(r.s.userContacts, userID)
	for k, c := range r.s.authCreds {
		if c.UserID == userID {
			delete(r.s.authCreds, k)
		}
	}
	for k, s := range r.s.authSessions {
		if s.UserID == userID {
			delete(r.s.authSessions, k)
		}
	}
	for k, c := range r.s.authChallenges {
		if c.UserID == userID {
			delete(r.s.authChallenges, k)
		}
	}
	out := r.s.securityEvents[:0]
	for _, e := range r.s.securityEvents {
		if e.UserID != userID {
			out = append(out, e)
		}
	}
	r.s.securityEvents = out
	for _, o := range r.s.offlinePurchases {
		if o != nil && o.UserID == userID {
			o.PhoneHMAC = ""
		}
	}
	for _, tr := range r.s.tokenResends {
		if tr != nil && tr.UserID == userID {
			tr.PhoneHMAC = ""
		}
	}
	for i, o := range r.s.riskObs {
		if o.UserID == userID {
			o.EmailHMAC = ""
			o.IPHMAC = ""
			o.PhoneHMAC = ""
			r.s.riskObs[i] = o
		}
	}
	for id, d := range r.s.riskDec {
		if d != nil && d.UserID == userID {
			d.EmailHMAC = ""
			d.IPHMAC = ""
			r.s.riskDec[id] = d
		}
	}
	_ = now
}

func (r privacyRepo) eraseMemberships(userID string, now time.Time) {
	// Anonymise meter names/addresses only when no other active grant remains.
	owned := map[uuid.UUID]bool{}
	shared := map[uuid.UUID]bool{}
	for _, g := range r.s.grants {
		if g == nil || g.RevokedAt != nil {
			continue
		}
		if g.UserID == userID {
			owned[g.MeterID] = true
		} else {
			shared[g.MeterID] = true
		}
	}
	for id, m := range r.s.meters {
		if m == nil || !owned[id] || shared[id] {
			continue
		}
		m.CustomerName = ""
		m.Address = ""
	}
	for _, g := range r.s.grants {
		if g != nil && g.UserID == userID && g.RevokedAt == nil {
			t := now
			g.RevokedAt = &t
		}
	}
	for k, m := range r.s.hhMembers {
		if m.UserID == userID && (m.Status == households.StatusActive || m.Status == households.StatusInvited) {
			m.Status = households.StatusRevoked
			t := now
			m.RevokedAt = &t
			r.s.hhMembers[k] = m
		}
	}
	for id, h := range r.s.households {
		if h.OwnerID == userID {
			h.OwnerID = privacy.TombstoneUserID.String()
			h.Name = ""
			r.s.households[id] = h
		}
	}
	for k, o := range r.s.ownership {
		if o.UserID == userID && o.RevokedAt == nil {
			t := now
			o.RevokedAt = &t
			r.s.ownership[k] = o
		}
	}
	for id, rl := range r.s.apRules {
		if rl != nil && rl.UserID == userID && rl.Status != autopower.RuleCancelled && rl.Status != autopower.RuleCompleted {
			rl.Status = autopower.RuleCancelled
			rl.UpdatedAt = now
			r.s.apRules[id] = rl
		}
	}
	for id, run := range r.s.apRuns {
		if run == nil {
			continue
		}
		rl := r.s.apRules[run.RuleID]
		if rl != nil && rl.UserID == userID && (run.Status == autopower.RunScheduled) {
			run.Status = autopower.RunCancelled
			r.s.apRuns[id] = run
		}
	}
	for k, mem := range r.s.orgMembers {
		if mem != nil && mem.UserID == userID {
			delete(r.s.orgMembers, k)
		}
	}
	for k, l := range r.s.phoneLinks {
		if l != nil && l.UserID == userID {
			delete(r.s.phoneLinks, k)
		}
	}
}

func (r privacyRepo) eraseSupport(userID string) {
	drop := map[uuid.UUID]bool{}
	for id, th := range r.s.supportThreads {
		if th != nil && th.CustomerID == userID {
			drop[id] = true
			delete(r.s.supportThreads, id)
		}
	}
	for id, msg := range r.s.supportMessages {
		if msg != nil && drop[msg.ThreadID] {
			delete(r.s.supportMessages, id)
		}
	}
	for k, w := range r.s.supportWraps {
		if w != nil && drop[w.ThreadID] {
			delete(r.s.supportWraps, k)
		}
	}
	delete(r.s.supportKeys, userID)
}

func (r privacyRepo) eraseNotify(userID string) {
	delete(r.s.notifPrefs, userID)
	kept := r.s.consents[:0]
	for _, rec := range r.s.consents {
		if rec.UserID != userID {
			kept = append(kept, rec)
		}
	}
	r.s.consents = kept
	for id, h := range r.s.legalHolds {
		if h.UserID == userID {
			delete(r.s.legalHolds, id)
		}
	}
}

func (r privacyRepo) eraseCards(userID string, now time.Time) {
	for _, m := range r.s.payMethods {
		if m == nil || m.UserID != userID || m.DeletedAt != nil {
			continue
		}
		t := now
		m.DeletedAt = &t
		m.AuthCodeCiphertext = nil
		m.EmailCiphertext = nil
	}
}

func (r privacyRepo) eraseExports(userID string) {
	for id, j := range r.s.dataExports {
		if j.UserID != userID {
			continue
		}
		delete(r.s.dataExports, id)
		delete(r.s.dataExportByDay, exportDayKey(j.UserID, j.Day))
	}
}
