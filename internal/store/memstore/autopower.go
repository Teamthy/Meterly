package memstore

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/store"
)

type autoPowerRepo struct{ *repos }

func clonePaymentMethod(m *autopower.SavedPaymentMethod) *autopower.SavedPaymentMethod {
	cp := *m
	cp.AuthCodeCiphertext = append([]byte(nil), m.AuthCodeCiphertext...)
	cp.EmailCiphertext = append([]byte(nil), m.EmailCiphertext...)
	cp.ConsentedAt = m.ConsentedAt.UTC()
	cp.CreatedAt = m.CreatedAt.UTC()
	if m.DeletedAt != nil {
		t := m.DeletedAt.UTC()
		cp.DeletedAt = &t
	}
	return &cp
}

func cloneRule(rl *autopower.Rule) *autopower.Rule {
	cp := *rl
	cp.DaysOfWeek = append([]int(nil), rl.DaysOfWeek...)
	cp.DaysOfMonth = append([]int(nil), rl.DaysOfMonth...)
	cp.LeaseUntil = rl.LeaseUntil.UTC()
	cp.CreatedAt = rl.CreatedAt.UTC()
	cp.UpdatedAt = rl.UpdatedAt.UTC()
	if rl.EndsAt != nil {
		t := rl.EndsAt.UTC()
		cp.EndsAt = &t
	}
	if rl.NextScheduledAt != nil {
		t := rl.NextScheduledAt.UTC()
		cp.NextScheduledAt = &t
	}
	if rl.NextPollAt != nil {
		t := rl.NextPollAt.UTC()
		cp.NextPollAt = &t
	}
	if rl.LastLowBalanceTriggered != nil {
		t := rl.LastLowBalanceTriggered.UTC()
		cp.LastLowBalanceTriggered = &t
	}
	if rl.LastReadingMilliKWh != nil {
		v := *rl.LastReadingMilliKWh
		cp.LastReadingMilliKWh = &v
	}
	if rl.LastReadingAt != nil {
		t := rl.LastReadingAt.UTC()
		cp.LastReadingAt = &t
	}
	return &cp
}

func cloneRun(rn *autopower.Run) *autopower.Run {
	cp := *rn
	cp.ScheduledFor = rn.ScheduledFor.UTC()
	cp.LeaseUntil = rn.LeaseUntil.UTC()
	cp.CreatedAt = rn.CreatedAt.UTC()
	cp.UpdatedAt = rn.UpdatedAt.UTC()
	if rn.ExecutedAt != nil {
		t := rn.ExecutedAt.UTC()
		cp.ExecutedAt = &t
	}
	if rn.HeadsUpSentAt != nil {
		t := rn.HeadsUpSentAt.UTC()
		cp.HeadsUpSentAt = &t
	}
	if rn.TransactionID != nil {
		id := *rn.TransactionID
		cp.TransactionID = &id
	}
	if rn.ChargeDispatchedAt != nil {
		t := rn.ChargeDispatchedAt.UTC()
		cp.ChargeDispatchedAt = &t
	}
	if rn.CompletedAt != nil {
		t := rn.CompletedAt.UTC()
		cp.CompletedAt = &t
	}
	return &cp
}

func runSlotKey(ruleID uuid.UUID, scheduledFor time.Time) string {
	return ruleID.String() + "|" + scheduledFor.UTC().Format(time.RFC3339Nano)
}

func (r autoPowerRepo) CreatePaymentMethod(_ context.Context, m *autopower.SavedPaymentMethod) error {
	defer r.lock()()
	if m.UserID == "" || !r.s.userIDs[m.UserID] {
		return store.ErrNotFound
	}
	if _, ok := r.s.txns[m.SourceTransactionID]; !ok {
		return store.ErrNotFound
	}
	for _, existing := range r.s.payMethods {
		if existing.UserID == m.UserID && existing.SourceTransactionID == m.SourceTransactionID && existing.DeletedAt == nil {
			return store.ErrDuplicate
		}
	}
	cp := clonePaymentMethod(m)
	if cp.ID == uuid.Nil {
		cp.ID = uuid.New()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	r.s.payMethods[cp.ID] = cp
	id := cp.ID
	r.onUndo(func() { delete(r.s.payMethods, id) })
	*m = *clonePaymentMethod(cp)
	return nil
}

func (r autoPowerRepo) GetPaymentMethod(_ context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	defer r.lock()()
	m, ok := r.s.payMethods[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return clonePaymentMethod(m), nil
}

func (r autoPowerRepo) GetPaymentMethodForUpdate(ctx context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	if !r.inTx {
		return nil, errors.New("payment method lock requires transaction")
	}
	return r.GetPaymentMethod(ctx, id)
}

func (r autoPowerRepo) GetActivePaymentMethodBySourceTxn(_ context.Context, userID string, txnID uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	defer r.lock()()
	for _, m := range r.s.payMethods {
		if m.UserID == userID && m.SourceTransactionID == txnID && m.DeletedAt == nil {
			return clonePaymentMethod(m), nil
		}
	}
	return nil, store.ErrNotFound
}

func (r autoPowerRepo) ListPaymentMethods(_ context.Context, userID string) ([]*autopower.SavedPaymentMethod, error) {
	defer r.lock()()
	var out []*autopower.SavedPaymentMethod
	for _, m := range r.s.payMethods {
		if m.UserID == userID && m.DeletedAt == nil {
			out = append(out, clonePaymentMethod(m))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (r autoPowerRepo) DeletePaymentMethod(_ context.Context, id uuid.UUID, at time.Time) error {
	defer r.lock()()
	prev, ok := r.s.payMethods[id]
	if !ok || prev.DeletedAt != nil {
		return store.ErrNotFound
	}
	cp := clonePaymentMethod(prev)
	ts := at.UTC()
	cp.DeletedAt = &ts
	r.s.payMethods[id] = cp
	r.onUndo(func() { r.s.payMethods[id] = prev })
	return nil
}

func (r autoPowerRepo) CreateRule(_ context.Context, rl *autopower.Rule) error {
	defer r.lock()()
	if rl.UserID == "" || !r.s.userIDs[rl.UserID] {
		return store.ErrNotFound
	}
	if _, ok := r.s.meters[rl.MeterID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := r.s.payMethods[rl.PaymentMethodID]; !ok {
		return store.ErrNotFound
	}
	cp := cloneRule(rl)
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
	if cp.Currency == "" {
		cp.Currency = "NGN"
	}
	if cp.Timezone == "" {
		cp.Timezone = autopower.DefaultTimezone
	}
	if _, dup := r.s.apRules[cp.ID]; dup {
		return store.ErrDuplicate
	}
	r.s.apRules[cp.ID] = cp
	id := cp.ID
	r.onUndo(func() { delete(r.s.apRules, id) })
	*rl = *cloneRule(cp)
	return nil
}

func (r autoPowerRepo) GetRule(_ context.Context, id uuid.UUID) (*autopower.Rule, error) {
	defer r.lock()()
	rl, ok := r.s.apRules[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRule(rl), nil
}

func (r autoPowerRepo) GetRuleForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Rule, error) {
	if !r.inTx {
		return nil, errors.New("autopower rule lock requires transaction")
	}
	return r.GetRule(ctx, id)
}

func (r autoPowerRepo) UpdateRule(_ context.Context, rl *autopower.Rule) error {
	defer r.lock()()
	prev, ok := r.s.apRules[rl.ID]
	if !ok {
		return store.ErrNotFound
	}
	cp := cloneRule(rl)
	r.s.apRules[rl.ID] = cp
	id := rl.ID
	r.onUndo(func() { r.s.apRules[id] = prev })
	return nil
}

func (r autoPowerRepo) ListRulesForUser(_ context.Context, userID string) ([]*autopower.Rule, error) {
	defer r.lock()()
	var out []*autopower.Rule
	for _, rl := range r.s.apRules {
		if rl.UserID == userID && rl.Status != autopower.RuleCancelled {
			out = append(out, cloneRule(rl))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (r autoPowerRepo) ListRulesByPaymentMethodForUpdate(_ context.Context, paymentMethodID uuid.UUID) ([]*autopower.Rule, error) {
	if !r.inTx {
		return nil, errors.New("autopower rule lock requires transaction")
	}
	var out []*autopower.Rule
	for _, rl := range r.s.apRules {
		if rl.PaymentMethodID == paymentMethodID && rl.Status != autopower.RuleCancelled {
			out = append(out, cloneRule(rl))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

func (r autoPowerRepo) ClaimDueRulesForMaterialize(_ context.Context, horizon, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	horizon = horizon.UTC()
	now = now.UTC()
	var due []*autopower.Rule
	for _, rl := range r.s.apRules {
		if rl.Status != autopower.RuleActive || rl.NextScheduledAt == nil {
			continue
		}
		if !rl.NextScheduledAt.After(horizon) && !rl.LeaseUntil.After(now) {
			due = append(due, rl)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextScheduledAt.Equal(*due[j].NextScheduledAt) {
			return due[i].ID.String() < due[j].ID.String()
		}
		return due[i].NextScheduledAt.Before(*due[j].NextScheduledAt)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	var ids []uuid.UUID
	leaseUntil := now.Add(lease)
	for _, rl := range due {
		prev := rl.LeaseUntil
		target := rl
		target.LeaseUntil = leaseUntil
		r.onUndo(func() { target.LeaseUntil = prev })
		ids = append(ids, rl.ID)
	}
	return ids, nil
}

func (r autoPowerRepo) ClaimDueLowBalanceRules(_ context.Context, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	now = now.UTC()
	var due []*autopower.Rule
	for _, rl := range r.s.apRules {
		if rl.Status != autopower.RuleActive || rl.NextPollAt == nil || !rl.TriggerType.UsesLowBalance() {
			continue
		}
		if !rl.NextPollAt.After(now) && !rl.LeaseUntil.After(now) {
			due = append(due, rl)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextPollAt.Equal(*due[j].NextPollAt) {
			return due[i].ID.String() < due[j].ID.String()
		}
		return due[i].NextPollAt.Before(*due[j].NextPollAt)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	var ids []uuid.UUID
	leaseUntil := now.Add(lease)
	for _, rl := range due {
		prev := rl.LeaseUntil
		target := rl
		target.LeaseUntil = leaseUntil
		r.onUndo(func() { target.LeaseUntil = prev })
		ids = append(ids, rl.ID)
	}
	return ids, nil
}

func (r autoPowerRepo) CreateRun(_ context.Context, rn *autopower.Run) error {
	defer r.lock()()
	if _, ok := r.s.apRules[rn.RuleID]; !ok {
		return store.ErrNotFound
	}
	sk := runSlotKey(rn.RuleID, rn.ScheduledFor)
	if _, dup := r.s.apRunBySlot[sk]; dup {
		return store.ErrDuplicate
	}
	cp := cloneRun(rn)
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
	if cp.Currency == "" {
		cp.Currency = "NGN"
	}
	r.s.apRuns[cp.ID] = cp
	r.s.apRunBySlot[sk] = cp.ID
	var txnID uuid.UUID
	if cp.TransactionID != nil {
		txnID = *cp.TransactionID
		r.s.apRunByTxn[txnID] = cp.ID
	}
	id := cp.ID
	r.onUndo(func() {
		delete(r.s.apRuns, id)
		delete(r.s.apRunBySlot, sk)
		if txnID != uuid.Nil {
			delete(r.s.apRunByTxn, txnID)
		}
	})
	*rn = *cloneRun(cp)
	return nil
}

func (r autoPowerRepo) GetRun(_ context.Context, id uuid.UUID) (*autopower.Run, error) {
	defer r.lock()()
	rn, ok := r.s.apRuns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRun(rn), nil
}

func (r autoPowerRepo) GetRunForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Run, error) {
	if !r.inTx {
		return nil, errors.New("autopower run lock requires transaction")
	}
	return r.GetRun(ctx, id)
}

func (r autoPowerRepo) GetRunBySlot(_ context.Context, ruleID uuid.UUID, scheduledFor time.Time) (*autopower.Run, error) {
	defer r.lock()()
	id, ok := r.s.apRunBySlot[runSlotKey(ruleID, scheduledFor)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRun(r.s.apRuns[id]), nil
}

func (r autoPowerRepo) GetRunByTxn(_ context.Context, txnID uuid.UUID) (*autopower.Run, error) {
	defer r.lock()()
	id, ok := r.s.apRunByTxn[txnID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRun(r.s.apRuns[id]), nil
}

func (r autoPowerRepo) UpdateRun(_ context.Context, rn *autopower.Run) error {
	defer r.lock()()
	prev, ok := r.s.apRuns[rn.ID]
	if !ok {
		return store.ErrNotFound
	}
	cp := cloneRun(rn)
	r.s.apRuns[rn.ID] = cp
	var addedTxn uuid.UUID
	if cp.TransactionID != nil && (prev.TransactionID == nil || *prev.TransactionID != *cp.TransactionID) {
		addedTxn = *cp.TransactionID
		r.s.apRunByTxn[addedTxn] = cp.ID
	}
	id := rn.ID
	r.onUndo(func() {
		r.s.apRuns[id] = prev
		if addedTxn != uuid.Nil {
			delete(r.s.apRunByTxn, addedTxn)
		}
	})
	return nil
}

func (r autoPowerRepo) ListRunsForRule(_ context.Context, ruleID uuid.UUID, limit int) ([]*autopower.Run, error) {
	defer r.lock()()
	var out []*autopower.Run
	for _, rn := range r.s.apRuns {
		if rn.RuleID == ruleID {
			out = append(out, cloneRun(rn))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ScheduledFor.Equal(out[j].ScheduledFor) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].ScheduledFor.After(out[j].ScheduledFor)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r autoPowerRepo) CancelScheduledRunsForRule(_ context.Context, ruleID uuid.UUID, now time.Time) error {
	defer r.lock()()
	ts := now.UTC()
	type prevEntry struct {
		id   uuid.UUID
		prev *autopower.Run
	}
	var prevs []prevEntry
	for id, rn := range r.s.apRuns {
		if rn.RuleID == ruleID && rn.Status == autopower.RunScheduled {
			prevs = append(prevs, prevEntry{id: id, prev: rn})
			cp := cloneRun(rn)
			cp.Status = autopower.RunCancelled
			cp.CompletedAt = &ts
			cp.UpdatedAt = ts
			r.s.apRuns[id] = cp
		}
	}
	if len(prevs) > 0 {
		r.onUndo(func() {
			for _, p := range prevs {
				r.s.apRuns[p.id] = p.prev
			}
		})
	}
	return nil
}

func (r autoPowerRepo) ClaimDueRuns(_ context.Context, headsUpHorizon, now, staleChargingBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	if limit <= 0 {
		limit = 50
	}
	headsUpHorizon = headsUpHorizon.UTC()
	now = now.UTC()
	staleChargingBefore = staleChargingBefore.UTC()
	var due []*autopower.Run
	for _, rn := range r.s.apRuns {
		if rn.LeaseUntil.After(now) {
			continue
		}
		needsHeadsUp := rn.Status == autopower.RunScheduled && rn.HeadsUpSentAt == nil && !rn.ScheduledFor.After(headsUpHorizon)
		needsExec := rn.Status == autopower.RunScheduled && !rn.ScheduledFor.After(now)
		needsRecon := rn.Status == autopower.RunCharging && !rn.UpdatedAt.After(staleChargingBefore)
		if needsHeadsUp || needsExec || needsRecon {
			due = append(due, rn)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].ScheduledFor.Equal(due[j].ScheduledFor) {
			return due[i].ID.String() < due[j].ID.String()
		}
		return due[i].ScheduledFor.Before(due[j].ScheduledFor)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	var ids []uuid.UUID
	leaseUntil := now.Add(lease)
	for _, rn := range due {
		prev := rn.LeaseUntil
		target := rn
		target.LeaseUntil = leaseUntil
		r.onUndo(func() { target.LeaseUntil = prev })
		ids = append(ids, rn.ID)
	}
	return ids, nil
}

func effectiveRunTime(rn *autopower.Run) time.Time {
	if rn.ExecutedAt != nil {
		return rn.ExecutedAt.UTC()
	}
	return rn.ScheduledFor.UTC()
}

func (r autoPowerRepo) SumRuleSpendMinor(_ context.Context, ruleID uuid.UUID, startUTC, endUTC time.Time) (int64, error) {
	defer r.lock()()
	startUTC = startUTC.UTC()
	endUTC = endUTC.UTC()
	var sum int64
	for _, rn := range r.s.apRuns {
		if rn.RuleID != ruleID || !rn.Status.CountsTowardCap() {
			continue
		}
		t := effectiveRunTime(rn)
		if !t.Before(startUTC) && t.Before(endUTC) {
			sum += rn.AmountMinor
		}
	}
	return sum, nil
}

func (r autoPowerRepo) SumUserSpendMinor(_ context.Context, userID string, startUTC, endUTC time.Time) (int64, error) {
	defer r.lock()()
	startUTC = startUTC.UTC()
	endUTC = endUTC.UTC()
	var sum int64
	for _, rn := range r.s.apRuns {
		if rn.UserID != userID || !rn.Status.CountsTowardCap() {
			continue
		}
		t := effectiveRunTime(rn)
		if !t.Before(startUTC) && t.Before(endUTC) {
			sum += rn.AmountMinor
		}
	}
	return sum, nil
}
