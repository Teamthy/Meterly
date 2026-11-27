package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
)

type privacyRepo struct{ *repos }

func skipHeld(held map[string]bool, userID string) bool {
	return userID != "" && held[userID]
}

func capBatch[T any](ids []T, batch int) []T {
	if batch <= 0 || len(ids) <= batch {
		return ids
	}
	return ids[:batch]
}

func (r privacyRepo) ListConsents(_ context.Context, userID string) ([]privacy.Record, error) {
	defer r.lock()()
	var out []privacy.Record
	for _, rec := range r.s.consents {
		if rec.UserID == userID {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r privacyRepo) AppendConsent(_ context.Context, rec privacy.Record) error {
	defer r.lock()()
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	r.s.consents = append(r.s.consents, rec)
	n := len(r.s.consents)
	r.onUndo(func() { r.s.consents = r.s.consents[:n-1] })
	return nil
}

func (r privacyRepo) AddLegalHold(_ context.Context, h privacy.Hold) error {
	defer r.lock()()
	if h.ID == uuid.Nil {
		h.ID = uuid.New()
	}
	prev, had := r.s.legalHolds[h.ID]
	r.s.legalHolds[h.ID] = h
	r.onUndo(func() {
		if had {
			r.s.legalHolds[h.ID] = prev
		} else {
			delete(r.s.legalHolds, h.ID)
		}
	})
	return nil
}

func (r privacyRepo) ActiveHeldUserIDs(_ context.Context) (map[string]bool, error) {
	defer r.lock()()
	out := map[string]bool{}
	for _, h := range r.s.legalHolds {
		if h.ReleasedAt == nil && h.UserID != "" {
			out[h.UserID] = true
		}
	}
	return out, nil
}

func (r privacyRepo) RecordRetentionRun(_ context.Context, run privacy.RetentionRun) error {
	defer r.lock()()
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	if run.Counts == nil {
		run.Counts = map[string]int{}
	}
	r.s.retentionRuns = append(r.s.retentionRuns, run)
	n := len(r.s.retentionRuns)
	meta := map[string]any{"dry_run": run.DryRun, "counts": run.Counts}
	if run.Error != "" {
		meta["error"] = run.Error
	}
	at := run.StartedAt
	if run.FinishedAt != nil {
		at = *run.FinishedAt
	}
	id := run.ID
	r.s.audit = append(r.s.audit, audit.Event{
		ID:           int64(len(r.s.audit) + 1),
		ActorType:    audit.ActorSystem,
		Action:       "retention.purge",
		ResourceType: "retention_run",
		ResourceID:   &id,
		Metadata:     meta,
		CreatedAt:    at,
	})
	an := len(r.s.audit)
	r.onUndo(func() {
		r.s.retentionRuns = r.s.retentionRuns[:n-1]
		r.s.audit = r.s.audit[:an-1]
	})
	return nil
}

func (r privacyRepo) PurgeChannelOTPs(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var ids []uuid.UUID
	for id, o := range r.s.channelOTPs {
		if o.CreatedAt.Before(cutoff) && !skipHeld(held, o.TargetUserID) {
			ids = append(ids, id)
		}
	}
	ids = capBatch(ids, batch)
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	prev := map[uuid.UUID]*conversation.ChannelOTP{}
	for _, id := range ids {
		if o, ok := r.s.channelOTPs[id]; ok {
			prev[id] = copyOTP(o)
			delete(r.s.channelOTPs, id)
		}
	}
	r.onUndo(func() {
		for id, o := range prev {
			r.s.channelOTPs[id] = o
		}
	})
	return len(prev), nil
}

func (r privacyRepo) PurgeAuthChallenges(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var keys []string
	for k, c := range r.s.authChallenges {
		if c.ExpiresAt.Before(cutoff) && !skipHeld(held, c.UserID) {
			keys = append(keys, k)
		}
	}
	keys = capBatch(keys, batch)
	if dryRun || len(keys) == 0 {
		return len(keys), nil
	}
	prev := map[string]identity.Challenge{}
	for _, k := range keys {
		prev[k] = r.s.authChallenges[k]
		delete(r.s.authChallenges, k)
	}
	r.onUndo(func() {
		for k, c := range prev {
			r.s.authChallenges[k] = c
		}
	})
	return len(prev), nil
}

func (r privacyRepo) PurgeAuthSessions(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var keys []string
	for k, s := range r.s.authSessions {
		end := s.ExpiresAt
		if s.RevokedAt != nil {
			end = *s.RevokedAt
		}
		if end.Before(cutoff) && !skipHeld(held, s.UserID) {
			keys = append(keys, k)
		}
	}
	keys = capBatch(keys, batch)
	if dryRun || len(keys) == 0 {
		return len(keys), nil
	}
	prev := map[string]identity.Session{}
	for _, k := range keys {
		prev[k] = r.s.authSessions[k]
		delete(r.s.authSessions, k)
	}
	r.onUndo(func() {
		for k, s := range prev {
			r.s.authSessions[k] = s
		}
	})
	return len(prev), nil
}

func (r privacyRepo) PurgeIdempotencyKeys(_ context.Context, cutoff time.Time, batch int, dryRun bool) (int, error) {
	defer r.lock()()
	var keys []string
	for k, rec := range r.s.idem {
		if rec != nil && rec.ResultRef != "" && rec.CreatedAt.Before(cutoff) {
			keys = append(keys, k)
		}
	}
	keys = capBatch(keys, batch)
	if dryRun || len(keys) == 0 {
		return len(keys), nil
	}
	prev := map[string]*idempotency.Record{}
	for _, k := range keys {
		if rec, ok := r.s.idem[k]; ok {
			cp := *rec
			prev[k] = &cp
			delete(r.s.idem, k)
		}
	}
	r.onUndo(func() {
		for k, rec := range prev {
			r.s.idem[k] = rec
		}
	})
	return len(prev), nil
}

func (r privacyRepo) PurgeRiskObservations(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var idx []int
	for i, o := range r.s.riskObs {
		if o.ObservedAt.Before(cutoff) && !skipHeld(held, o.UserID) {
			idx = append(idx, i)
		}
	}
	if batch > 0 && len(idx) > batch {
		idx = idx[:batch]
	}
	if dryRun || len(idx) == 0 {
		return len(idx), nil
	}
	drop := map[int]bool{}
	for _, i := range idx {
		drop[i] = true
	}
	prev := append([]risk.Observation(nil), r.s.riskObs...)
	var next []risk.Observation
	for i, o := range r.s.riskObs {
		if !drop[i] {
			next = append(next, o)
		}
	}
	r.s.riskObs = next
	r.onUndo(func() { r.s.riskObs = prev })
	return len(idx), nil
}

func (r privacyRepo) PurgeClosedSupportThreads(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var ids []uuid.UUID
	for id, th := range r.s.supportThreads {
		if th != nil && th.Closed() && th.UpdatedAt.Before(cutoff) && !skipHeld(held, th.CustomerID) {
			ids = append(ids, id)
		}
	}
	ids = capBatch(ids, batch)
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	type snap struct {
		th   *support.Thread
		msgs []*support.Message
		wrap map[string]*support.WrappedKey
	}
	prev := map[uuid.UUID]snap{}
	for _, id := range ids {
		s := snap{wrap: map[string]*support.WrappedKey{}}
		if th, ok := r.s.supportThreads[id]; ok {
			cp := *th
			s.th = &cp
			delete(r.s.supportThreads, id)
		}
		for mid, m := range r.s.supportMessages {
			if m != nil && m.ThreadID == id {
				cp := *m
				s.msgs = append(s.msgs, &cp)
				delete(r.s.supportMessages, mid)
			}
		}
		prefix := id.String() + ":"
		for k, w := range r.s.supportWraps {
			if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
				cp := *w
				s.wrap[k] = &cp
				delete(r.s.supportWraps, k)
			}
		}
		prev[id] = s
	}
	r.onUndo(func() {
		for id, s := range prev {
			if s.th != nil {
				r.s.supportThreads[id] = s.th
			}
			for _, m := range s.msgs {
				r.s.supportMessages[m.ID] = m
			}
			for k, w := range s.wrap {
				r.s.supportWraps[k] = w
			}
		}
	})
	return len(prev), nil
}

func (r privacyRepo) CountPurgeableDeliveryHMACs(_ context.Context, cutoff time.Time) (int, error) {
	defer r.lock()()
	n := 0
	for _, d := range r.s.deliveries {
		if d != nil && d.DestinationHMAC != "" && d.Status.Terminal() && d.CreatedAt.Before(cutoff) {
			n++
		}
	}
	return n, nil
}

func (r privacyRepo) PurgeDeliveryHMACs(_ context.Context, cutoff time.Time) (int, error) {
	defer r.lock()()
	n := 0
	type prev struct {
		id   uuid.UUID
		hmac string
		upd  time.Time
	}
	var prevs []prev
	now := time.Now().UTC()
	for id, d := range r.s.deliveries {
		if d == nil || d.DestinationHMAC == "" || !d.Status.Terminal() || !d.CreatedAt.Before(cutoff) {
			continue
		}
		prevs = append(prevs, prev{id: id, hmac: d.DestinationHMAC, upd: d.UpdatedAt})
		cp := cloneDelivery(d)
		cp.DestinationHMAC = ""
		cp.UpdatedAt = now
		r.s.deliveries[id] = cp
		n++
	}
	r.onUndo(func() {
		for _, p := range prevs {
			if d, ok := r.s.deliveries[p.id]; ok {
				d.DestinationHMAC = p.hmac
				d.UpdatedAt = p.upd
			}
		}
	})
	return n, nil
}

func exportDayKey(userID, day string) string { return userID + "|" + day }

func (r privacyRepo) CreateExport(_ context.Context, job privacy.ExportJob) error {
	defer r.lock()()
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
	}
	k := exportDayKey(job.UserID, job.Day)
	if _, ok := r.s.dataExportByDay[k]; ok {
		return store.ErrDuplicate
	}
	if _, ok := r.s.dataExports[job.ID]; ok {
		return store.ErrDuplicate
	}
	r.s.dataExports[job.ID] = privacy.CloneExport(job)
	r.s.dataExportByDay[k] = job.ID
	r.onUndo(func() {
		delete(r.s.dataExports, job.ID)
		delete(r.s.dataExportByDay, k)
	})
	return nil
}

func (r privacyRepo) GetExport(_ context.Context, id uuid.UUID) (privacy.ExportJob, error) {
	defer r.lock()()
	j, ok := r.s.dataExports[id]
	if !ok {
		return privacy.ExportJob{}, store.ErrNotFound
	}
	return privacy.CloneExport(j), nil
}

func (r privacyRepo) GetExportForUserDay(_ context.Context, userID, day string) (privacy.ExportJob, error) {
	defer r.lock()()
	id, ok := r.s.dataExportByDay[exportDayKey(userID, day)]
	if !ok {
		return privacy.ExportJob{}, store.ErrNotFound
	}
	j, ok := r.s.dataExports[id]
	if !ok {
		return privacy.ExportJob{}, store.ErrNotFound
	}
	return privacy.CloneExport(j), nil
}

func (r privacyRepo) UpdateExport(_ context.Context, job privacy.ExportJob) error {
	defer r.lock()()
	prev, ok := r.s.dataExports[job.ID]
	if !ok {
		return store.ErrNotFound
	}
	r.s.dataExports[job.ID] = privacy.CloneExport(job)
	r.onUndo(func() { r.s.dataExports[job.ID] = prev })
	return nil
}

func (r privacyRepo) ListPendingExports(_ context.Context, limit int) ([]privacy.ExportJob, error) {
	defer r.lock()()
	var out []privacy.ExportJob
	for _, j := range r.s.dataExports {
		if j.Status == privacy.ExportPending {
			out = append(out, privacy.CloneExport(j))
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r privacyRepo) ExpireDueExports(_ context.Context, now time.Time, batch int) (int, error) {
	defer r.lock()()
	var ids []uuid.UUID
	for id, j := range r.s.dataExports {
		if (j.Status == privacy.ExportPending || j.Status == privacy.ExportReady) && !j.ExpiresAt.After(now) {
			ids = append(ids, id)
		}
	}
	ids = capBatch(ids, batch)
	if len(ids) == 0 {
		return 0, nil
	}
	prev := map[uuid.UUID]privacy.ExportJob{}
	for _, id := range ids {
		j := r.s.dataExports[id]
		prev[id] = privacy.CloneExport(j)
		j.Status = privacy.ExportExpired
		j.ArchiveCiphertext = nil
		j.TokenCiphertext = nil
		j.UpdatedAt = now.UTC()
		r.s.dataExports[id] = j
	}
	r.onUndo(func() {
		for id, j := range prev {
			r.s.dataExports[id] = j
		}
	})
	return len(ids), nil
}

func (r privacyRepo) PurgeDataExports(_ context.Context, cutoff time.Time, batch int, dryRun bool, held map[string]bool) (int, error) {
	defer r.lock()()
	var ids []uuid.UUID
	for id, j := range r.s.dataExports {
		if j.CreatedAt.Before(cutoff) && !skipHeld(held, j.UserID) {
			ids = append(ids, id)
		}
	}
	ids = capBatch(ids, batch)
	if dryRun || len(ids) == 0 {
		return len(ids), nil
	}
	type snap struct {
		job privacy.ExportJob
		day string
	}
	prev := map[uuid.UUID]snap{}
	for _, id := range ids {
		j := r.s.dataExports[id]
		k := exportDayKey(j.UserID, j.Day)
		prev[id] = snap{job: privacy.CloneExport(j), day: k}
		delete(r.s.dataExports, id)
		delete(r.s.dataExportByDay, k)
	}
	r.onUndo(func() {
		for id, s := range prev {
			r.s.dataExports[id] = s.job
			r.s.dataExportByDay[s.day] = id
		}
	})
	return len(prev), nil
}
