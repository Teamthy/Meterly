// Package memstore is an in-memory store.Store for unit tests and local
// development. InTx is serialisable (one global lock) and rolls back every
// write on error via an undo log, matching PostgreSQL semantics closely
// enough for the orchestrator's guarantees to be tested against it.
package memstore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notificationintents"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Store struct {
	mu sync.Mutex

	users             map[string]string
	userIDs           map[string]bool
	userContacts      map[string]notifications.UserContacts
	meters            map[uuid.UUID]*meters.Meter
	meterByHash       map[string]uuid.UUID
	grants            []*meters.Grant
	quotes            map[uuid.UUID]*tx.Quote
	txns              map[uuid.UUID]*tx.Transaction
	txnByPayRef       map[string]uuid.UUID
	ledger            []ledger.Transaction
	ledgerByRef       map[string]int
	outbox            []*events.Event
	idem              map[string]*idempotency.Record
	audit             []audit.Event
	providerOff       map[string]bool
	tokenShares       []tokenshares.Share
	gifts             map[uuid.UUID]gifts.Gift
	candidates        map[uuid.UUID]notificationintents.Intent
	notifPrefs        map[string]notifications.Preferences
	optOuts           map[string]notifications.OptOut
	waWindows         map[string]time.Time
	deliveries        map[uuid.UUID]*notifications.Delivery
	deliveryByKey     map[string]uuid.UUID
	payReqs           map[uuid.UUID]*payrequests.PaymentRequest
	payReqByCodeHMAC  map[string]uuid.UUID
	payers            map[uuid.UUID]payrequests.Payer
	payerByStatusHMAC map[string]uuid.UUID
	pots              map[uuid.UUID]*pots.Pot
	potByCodeHMAC     map[string]uuid.UUID
	contribs          map[uuid.UUID]*pots.Contribution
	contribByPayRef   map[string]uuid.UUID
	allocs            map[uuid.UUID]*pots.Allocation
	allocByTxn        map[uuid.UUID]uuid.UUID
	payMethods        map[uuid.UUID]*autopower.SavedPaymentMethod
	apRules           map[uuid.UUID]*autopower.Rule
	apRuns            map[uuid.UUID]*autopower.Run
	apRunBySlot       map[string]uuid.UUID
	apRunByTxn        map[uuid.UUID]uuid.UUID
	orgs              map[uuid.UUID]*powerdesk.Organisation
	orgMembers        map[string]*powerdesk.Member // "<org_id>:<user_id>"
	properties        map[uuid.UUID]*powerdesk.Property
	buildings         map[uuid.UUID]*powerdesk.Building
	units             map[uuid.UUID]*powerdesk.Unit
	unitMeters        map[uuid.UUID]*powerdesk.UnitMeter
	bulkTopups        map[uuid.UUID]*powerdesk.BulkTopup
	bulkTopupByIdem   map[string]uuid.UUID // "<org_id>:<key>"
	bulkItems         map[uuid.UUID]*powerdesk.BulkTopupItem
	phoneLinks        map[string]*conversation.PhoneLink
	channelOTPs       map[uuid.UUID]*conversation.ChannelOTP
	convSessions      map[uuid.UUID]*conversation.Session
	convSessionByProv map[string]uuid.UUID // "<channel>:<provider_session_id>"
	processedMsgs     map[string]time.Time // "<channel>:<message_id>"
	offlinePurchases  map[uuid.UUID]*conversation.OfflinePurchase
	tokenResends      []*conversation.TokenResend
	supportKeys       map[string]*support.IdentityKey
	supportThreads    map[uuid.UUID]*support.Thread
	supportMessages   map[uuid.UUID]*support.Message
	supportWraps      map[string]*support.WrappedKey // threadID + ":" + holder
	households        map[uuid.UUID]households.Household
	hhMembers         map[string]households.Member    // householdID:userID
	hhMeters          map[string]households.MeterLink // householdID:meterID
	ownership         map[string]households.Ownership // meterID:userID
}

func New() *Store {
	return &Store{
		users: map[string]string{}, userIDs: map[string]bool{}, userContacts: map[string]notifications.UserContacts{},
		meters: map[uuid.UUID]*meters.Meter{}, meterByHash: map[string]uuid.UUID{},
		quotes: map[uuid.UUID]*tx.Quote{}, txns: map[uuid.UUID]*tx.Transaction{}, txnByPayRef: map[string]uuid.UUID{},
		ledgerByRef: map[string]int{}, idem: map[string]*idempotency.Record{}, providerOff: map[string]bool{},
		gifts: map[uuid.UUID]gifts.Gift{}, candidates: map[uuid.UUID]notificationintents.Intent{},
		notifPrefs: map[string]notifications.Preferences{}, optOuts: map[string]notifications.OptOut{},
		waWindows: map[string]time.Time{}, deliveries: map[uuid.UUID]*notifications.Delivery{},
		deliveryByKey:     map[string]uuid.UUID{},
		payReqs:           map[uuid.UUID]*payrequests.PaymentRequest{},
		payReqByCodeHMAC:  map[string]uuid.UUID{},
		payers:            map[uuid.UUID]payrequests.Payer{},
		payerByStatusHMAC: map[string]uuid.UUID{},
		pots:              map[uuid.UUID]*pots.Pot{},
		potByCodeHMAC:     map[string]uuid.UUID{},
		contribs:          map[uuid.UUID]*pots.Contribution{},
		contribByPayRef:   map[string]uuid.UUID{},
		allocs:            map[uuid.UUID]*pots.Allocation{},
		allocByTxn:        map[uuid.UUID]uuid.UUID{},
		payMethods:        map[uuid.UUID]*autopower.SavedPaymentMethod{},
		apRules:           map[uuid.UUID]*autopower.Rule{},
		apRuns:            map[uuid.UUID]*autopower.Run{},
		apRunBySlot:       map[string]uuid.UUID{},
		apRunByTxn:        map[uuid.UUID]uuid.UUID{},
		orgs:              map[uuid.UUID]*powerdesk.Organisation{},
		orgMembers:        map[string]*powerdesk.Member{},
		properties:        map[uuid.UUID]*powerdesk.Property{},
		buildings:         map[uuid.UUID]*powerdesk.Building{},
		units:             map[uuid.UUID]*powerdesk.Unit{},
		unitMeters:        map[uuid.UUID]*powerdesk.UnitMeter{},
		bulkTopups:        map[uuid.UUID]*powerdesk.BulkTopup{},
		bulkTopupByIdem:   map[string]uuid.UUID{},
		bulkItems:         map[uuid.UUID]*powerdesk.BulkTopupItem{},
		phoneLinks:        map[string]*conversation.PhoneLink{},
		channelOTPs:       map[uuid.UUID]*conversation.ChannelOTP{},
		convSessions:      map[uuid.UUID]*conversation.Session{},
		convSessionByProv: map[string]uuid.UUID{},
		processedMsgs:     map[string]time.Time{},
		offlinePurchases:  map[uuid.UUID]*conversation.OfflinePurchase{},
		supportKeys:       map[string]*support.IdentityKey{},
		supportThreads:    map[uuid.UUID]*support.Thread{},
		supportMessages:   map[uuid.UUID]*support.Message{},
		supportWraps:      map[string]*support.WrappedKey{},
		households:        map[uuid.UUID]households.Household{},
		hhMembers:         map[string]households.Member{},
		hhMeters:          map[string]households.MeterLink{},
		ownership:         map[string]households.Ownership{},
	}
}

func (s *Store) Close() {}

func (s *Store) MetricsSnapshot(_ context.Context, now time.Time) (store.MetricsSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byStatus := make(map[tx.Overall]int64)
	for _, t := range s.txns {
		st := t.Status()
		byStatus[st]++
	}
	var oldest time.Time
	for _, e := range s.outbox {
		if e.PublishedAt != nil {
			continue
		}
		if oldest.IsZero() || e.CreatedAt.Before(oldest) {
			oldest = e.CreatedAt
		}
	}
	var lag float64
	if !oldest.IsZero() {
		if d := now.Sub(oldest).Seconds(); d > 0 {
			lag = d
		}
	}
	return store.MetricsSnapshot{
		TransactionsByStatus: byStatus,
		ManualReviewOpen:     byStatus[tx.OverallManualReview],
		OutboxLagSeconds:     lag,
	}, nil
}

func (s *Store) InTx(_ context.Context, fn func(store.Repos) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &repos{s: s, inTx: true}
	if err := fn(r); err != nil {
		for i := len(r.undo) - 1; i >= 0; i-- {
			r.undo[i]()
		}
		return err
	}
	return nil
}

func (s *Store) Repos() store.Repos { return &repos{s: s} }

type repos struct {
	s    *Store
	inTx bool
	undo []func()
}

func (r *repos) lock() func() {
	if r.inTx {
		return func() {}
	}
	r.s.mu.Lock()
	return r.s.mu.Unlock
}

func (r *repos) onUndo(f func()) {
	if r.inTx {
		r.undo = append(r.undo, f)
	}
}

func (r *repos) Users() store.UserRepo                                   { return r }
func (r *repos) Meters() store.MeterRepo                                 { return meterRepo{r} }
func (r *repos) Quotes() store.QuoteRepo                                 { return quoteRepo{r} }
func (r *repos) Transactions() store.TransactionRepo                     { return txnRepo{r} }
func (r *repos) Ledger() store.LedgerRepo                                { return ledgerRepo{r} }
func (r *repos) Outbox() store.OutboxRepo                                { return outboxRepo{r} }
func (r *repos) Idempotency() store.IdempotencyRepo                      { return idemRepo{r} }
func (r *repos) Audit() store.AuditRepo                                  { return auditRepo{r} }
func (r *repos) Providers() store.ProviderRepo                           { return providerRepo{r} }
func (r *repos) TokenShares() store.TokenShareRepo                       { return shareRepo{r} }
func (r *repos) Gifts() store.GiftRepo                                   { return giftRepo{r} }
func (r *repos) NotificationCandidates() store.NotificationCandidateRepo { return candidateRepo{r} }
func (r *repos) Notifications() store.NotificationRepo                   { return notifRepo{r} }
func (r *repos) PayRequests() store.PayRequestRepo                       { return payReqRepo{r} }
func (r *repos) Pots() store.PotRepo                                     { return potRepo{r} }
func (r *repos) AutoPower() store.AutoPowerRepo                          { return autoPowerRepo{r} }
func (r *repos) PowerDesk() store.PowerDeskRepo                          { return powerDeskRepo{r} }
func (r *repos) Offline() store.OfflineRepo                              { return offlineRepo{r} }
func (r *repos) Support() store.SupportRepo                              { return supportRepo{r} }
func (r *repos) Households() store.HouseholdRepo                         { return householdRepo{r} }

// ---- users -------------------------------------------------------------------

func (r *repos) EnsureByExternalID(_ context.Context, ext string) (string, error) {
	defer r.lock()()
	if id, ok := r.s.users[ext]; ok {
		return id, nil
	}
	id := uuid.NewString()
	r.s.users[ext] = id
	r.s.userIDs[id] = true
	r.onUndo(func() {
		delete(r.s.users, ext)
		delete(r.s.userIDs, id)
	})
	return id, nil
}

func (r *repos) LockForUpdate(_ context.Context, userID string) error {
	if !r.inTx {
		return errors.New("user lock requires transaction")
	}
	if userID == "" || !r.s.userIDs[userID] {
		return store.ErrNotFound
	}
	return nil
}

// ---- meters -------------------------------------------------------------------

type meterRepo struct{ *repos }

func cloneMeter(m *meters.Meter) *meters.Meter {
	c := *m
	c.NumberEncrypted = append([]byte(nil), m.NumberEncrypted...)
	return &c
}

func (r meterRepo) Get(_ context.Context, id uuid.UUID) (*meters.Meter, error) {
	defer r.lock()()
	m, ok := r.s.meters[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneMeter(m), nil
}

func (r meterRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*meters.Meter, error) {
	if !r.inTx {
		return nil, errors.New("meter lock requires transaction")
	}
	return r.Get(ctx, id)
}

func (r meterRepo) GetByHash(_ context.Context, hash string) (*meters.Meter, error) {
	defer r.lock()()
	id, ok := r.s.meterByHash[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneMeter(r.s.meters[id]), nil
}

func (r meterRepo) Insert(_ context.Context, m *meters.Meter) error {
	defer r.lock()()
	if _, ok := r.s.meterByHash[m.NumberHash]; ok {
		return store.ErrDuplicate
	}
	r.s.meters[m.ID] = cloneMeter(m)
	r.s.meterByHash[m.NumberHash] = m.ID
	r.onUndo(func() { delete(r.s.meters, m.ID); delete(r.s.meterByHash, m.NumberHash) })
	return nil
}

func (r meterRepo) Update(_ context.Context, m *meters.Meter) error {
	defer r.lock()()
	prev, ok := r.s.meters[m.ID]
	if !ok {
		return store.ErrNotFound
	}
	r.s.meters[m.ID] = cloneMeter(m)
	r.onUndo(func() { r.s.meters[m.ID] = prev })
	return nil
}

func (r meterRepo) activeGrant(meterID uuid.UUID, userID string) *meters.Grant {
	for _, g := range r.s.grants {
		if g.MeterID == meterID && g.UserID == userID && g.RevokedAt == nil {
			return g
		}
	}
	return nil
}

func (r meterRepo) UpsertGrant(_ context.Context, g meters.Grant) error {
	defer r.lock()()
	if cur := r.activeGrant(g.MeterID, g.UserID); cur != nil {
		prev := *cur
		if g.Permission.Allows(cur.Permission) {
			cur.Permission = g.Permission
		}
		if g.Label != "" {
			cur.Label = g.Label
		}
		r.onUndo(func() { *cur = prev })
		return nil
	}
	ng := g
	ng.RevokedAt = nil
	r.s.grants = append(r.s.grants, &ng)
	n := len(r.s.grants)
	r.onUndo(func() { r.s.grants = r.s.grants[:n-1] })
	return nil
}

func (r meterRepo) ActiveGrant(_ context.Context, meterID uuid.UUID, userID string) (*meters.Grant, error) {
	defer r.lock()()
	if g := r.activeGrant(meterID, userID); g != nil {
		c := *g
		return &c, nil
	}
	return nil, store.ErrNotFound
}

func (r meterRepo) ListForUser(_ context.Context, userID string) ([]meters.Listed, error) {
	defer r.lock()()
	var out []meters.Listed
	for _, g := range r.s.grants {
		if g.UserID == userID && g.RevokedAt == nil {
			out = append(out, meters.Listed{Meter: *cloneMeter(r.s.meters[g.MeterID]), Label: g.Label, Permission: g.Permission})
		}
	}
	return out, nil
}

func (r meterRepo) Revoke(_ context.Context, meterID uuid.UUID, userID string, at time.Time) error {
	defer r.lock()()
	if g := r.activeGrant(meterID, userID); g != nil {
		g.RevokedAt = &at
		r.onUndo(func() { g.RevokedAt = nil })
	}
	return nil
}

// ---- quotes -------------------------------------------------------------------

type quoteRepo struct{ *repos }

func cloneQuote(q *tx.Quote) *tx.Quote {
	c := *q
	c.DeliveryOptions = append([]tx.DeliveryMethod(nil), q.DeliveryOptions...)
	return &c
}

func (r quoteRepo) Create(_ context.Context, q *tx.Quote) error {
	defer r.lock()()
	r.s.quotes[q.ID] = cloneQuote(q)
	r.onUndo(func() { delete(r.s.quotes, q.ID) })
	return nil
}

func (r quoteRepo) Get(_ context.Context, id uuid.UUID) (*tx.Quote, error) {
	defer r.lock()()
	q, ok := r.s.quotes[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneQuote(q), nil
}

func (r quoteRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Quote, error) {
	return r.Get(ctx, id)
}

func (r quoteRepo) MarkUsed(_ context.Context, id uuid.UUID) error {
	defer r.lock()()
	q, ok := r.s.quotes[id]
	if !ok {
		return store.ErrNotFound
	}
	if q.Used {
		return store.ErrConflict
	}
	q.Used = true
	r.onUndo(func() { q.Used = false })
	return nil
}

// ---- transactions ---------------------------------------------------------------

type txnRepo struct{ *repos }

func cloneTxn(t *tx.Transaction) *tx.Transaction {
	c := *t
	c.History = append([]tx.Event(nil), t.History...)
	c.TokenCiphertext = append([]byte(nil), t.TokenCiphertext...)
	return &c
}

func (r txnRepo) Create(_ context.Context, t *tx.Transaction) error {
	defer r.lock()()
	if _, ok := r.s.txns[t.ID]; ok {
		return store.ErrDuplicate
	}
	t.Version = 1
	t.PersistedEvents = len(t.History)
	r.s.txns[t.ID] = cloneTxn(t)
	r.onUndo(func() { delete(r.s.txns, t.ID) })
	if t.PaymentProviderRef != "" {
		ref := t.PaymentProviderRef
		r.s.txnByPayRef[ref] = t.ID
		r.onUndo(func() { delete(r.s.txnByPayRef, ref) })
	}
	return nil
}

func (r txnRepo) Get(_ context.Context, id uuid.UUID) (*tx.Transaction, error) {
	defer r.lock()()
	t, ok := r.s.txns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneTxn(t), nil
}

func (r txnRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	return r.Get(ctx, id)
}

func (r txnRepo) ByPaymentRef(ctx context.Context, ref string) (*tx.Transaction, error) {
	unlock := r.lock()
	id, ok := r.s.txnByPayRef[ref]
	unlock()
	if !ok {
		return nil, store.ErrNotFound
	}
	return r.Get(ctx, id)
}

func (r txnRepo) Save(_ context.Context, t *tx.Transaction) error {
	defer r.lock()()
	cur, ok := r.s.txns[t.ID]
	if !ok {
		return store.ErrNotFound
	}
	if cur.Version != t.Version {
		return store.ErrConflict
	}
	t.Version++
	t.PersistedEvents = len(t.History)
	r.s.txns[t.ID] = cloneTxn(t)
	r.onUndo(func() { r.s.txns[t.ID] = cur })
	if t.PaymentProviderRef != "" {
		if _, had := r.s.txnByPayRef[t.PaymentProviderRef]; !had {
			r.s.txnByPayRef[t.PaymentProviderRef] = t.ID
			ref := t.PaymentProviderRef
			r.onUndo(func() { delete(r.s.txnByPayRef, ref) })
		}
	}
	return nil
}

func (r txnRepo) ListForCustomer(_ context.Context, customerID string) ([]*tx.Transaction, error) {
	defer r.lock()()
	var out []*tx.Transaction
	for _, t := range r.s.txns {
		if t.CustomerID == customerID {
			out = append(out, cloneTxn(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r txnRepo) Search(_ context.Context, f store.TxnFilter) ([]*tx.Transaction, error) {
	defer r.lock()()
	var out []*tx.Transaction
	for _, t := range r.s.txns {
		switch {
		case f.ReviewOnly && t.ReviewReason == "",
			f.Payment != "" && t.Payment != f.Payment,
			f.Vending != "" && t.Vending != f.Vending,
			f.Delivery != "" && t.Delivery != f.Delivery,
			f.CustomerID != "" && t.CustomerID != f.CustomerID,
			f.MeterID != uuid.Nil && t.MeterID != f.MeterID,
			!f.Before.IsZero() && !t.CreatedAt.Before(f.Before):
			continue
		}
		out = append(out, cloneTxn(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (r txnRepo) ClaimDue(_ context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	defer r.lock()()
	var due []*tx.Transaction
	for _, t := range r.s.txns {
		if !t.Settled() && !t.NextCheckAt.After(now) && !t.UpdatedAt.After(staleBefore) {
			due = append(due, t)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextCheckAt.Before(due[j].NextCheckAt) })
	var out []uuid.UUID
	for i, t := range due {
		if i == limit {
			break
		}
		prev := t.NextCheckAt
		t.NextCheckAt = now.Add(lease) // lease; does not bump Version
		r.onUndo(func() { t.NextCheckAt = prev })
		out = append(out, t.ID)
	}
	return out, nil
}

// ---- ledger --------------------------------------------------------------------

type ledgerRepo struct{ *repos }

func (r ledgerRepo) Post(_ context.Context, t ledger.Transaction) (ledger.Transaction, error) {
	if err := ledger.Validate(t.Entries); err != nil {
		return ledger.Transaction{}, err
	}
	defer r.lock()()
	if i, ok := r.s.ledgerByRef[t.Reference]; ok {
		return r.s.ledger[i], nil
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.PostedAt.IsZero() {
		t.PostedAt = time.Now().UTC()
	}
	t.Entries = append([]ledger.Entry(nil), t.Entries...)
	r.s.ledgerByRef[t.Reference] = len(r.s.ledger)
	r.s.ledger = append(r.s.ledger, t)
	n := len(r.s.ledger)
	r.onUndo(func() { r.s.ledger = r.s.ledger[:n-1]; delete(r.s.ledgerByRef, t.Reference) })
	return t, nil
}

func (r ledgerRepo) BySource(_ context.Context, id uuid.UUID) ([]ledger.Transaction, error) {
	defer r.lock()()
	var out []ledger.Transaction
	for _, t := range r.s.ledger {
		if t.SourceID == id {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r ledgerRepo) sum(match func(ledger.Entry) bool) int64 {
	var v int64
	for _, t := range r.s.ledger {
		for _, e := range t.Entries {
			if !match(e) {
				continue
			}
			if e.Direction == ledger.Debit {
				v += e.Amount.Minor
			} else {
				v -= e.Amount.Minor
			}
		}
	}
	return v
}

func (r ledgerRepo) Balance(_ context.Context, acct ledger.AccountCode, c money.Currency) (int64, error) {
	defer r.lock()()
	return r.sum(func(e ledger.Entry) bool { return e.Account == acct && e.Amount.Currency == c }), nil
}

func (r ledgerRepo) TrialBalance(context.Context) (int64, error) {
	defer r.lock()()
	return r.sum(func(ledger.Entry) bool { return true }), nil
}

// ---- outbox --------------------------------------------------------------------

type outboxRepo struct{ *repos }

func (r outboxRepo) Add(_ context.Context, e events.Event) error {
	defer r.lock()()
	c := e
	r.s.outbox = append(r.s.outbox, &c)
	n := len(r.s.outbox)
	r.onUndo(func() { r.s.outbox = r.s.outbox[:n-1] })
	return nil
}

func (r outboxRepo) ClaimUnpublished(_ context.Context, limit int) ([]events.Event, error) {
	defer r.lock()()
	var out []events.Event
	for _, e := range r.s.outbox {
		if e.PublishedAt == nil {
			out = append(out, *e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (r outboxRepo) MarkPublished(_ context.Context, id uuid.UUID, at time.Time) error {
	defer r.lock()()
	for _, e := range r.s.outbox {
		if e.ID == id && e.PublishedAt == nil {
			e.PublishedAt = &at
			ev := e
			r.onUndo(func() { ev.PublishedAt = nil })
		}
	}
	return nil
}

func (r outboxRepo) ForAggregate(_ context.Context, id uuid.UUID) ([]events.Event, error) {
	defer r.lock()()
	var out []events.Event
	for _, e := range r.s.outbox {
		if e.AggregateID == id {
			out = append(out, *e)
		}
	}
	return out, nil
}

// ---- idempotency ---------------------------------------------------------------

type idemRepo struct{ *repos }

func idemKey(scope, op, key string) string { return scope + "\x00" + op + "\x00" + key }

func (r idemRepo) Reserve(_ context.Context, rec idempotency.Record) (*idempotency.Record, error) {
	defer r.lock()()
	k := idemKey(rec.Scope, rec.Operation, rec.Key)
	prev, had := r.s.idem[k]
	if had && time.Now().Before(prev.ExpiresAt) {
		c := *prev
		return &c, nil
	}
	c := rec
	r.s.idem[k] = &c
	r.onUndo(func() {
		if had {
			r.s.idem[k] = prev
		} else {
			delete(r.s.idem, k)
		}
	})
	return nil, nil
}

func (r idemRepo) Complete(_ context.Context, scope, op, key, resultRef string) error {
	defer r.lock()()
	rec, ok := r.s.idem[idemKey(scope, op, key)]
	if !ok {
		return store.ErrNotFound
	}
	prev := rec.ResultRef
	rec.ResultRef = resultRef
	r.onUndo(func() { rec.ResultRef = prev })
	return nil
}

// ---- audit ---------------------------------------------------------------------

type auditRepo struct{ *repos }

func (r auditRepo) Add(_ context.Context, e audit.Event) error {
	defer r.lock()()
	e.ID = int64(len(r.s.audit) + 1)
	r.s.audit = append(r.s.audit, e)
	n := len(r.s.audit)
	r.onUndo(func() { r.s.audit = r.s.audit[:n-1] })
	return nil
}

func (r auditRepo) ForResource(_ context.Context, typ string, id uuid.UUID) ([]audit.Event, error) {
	defer r.lock()()
	var out []audit.Event
	for _, e := range r.s.audit {
		if e.ResourceType == typ && e.ResourceID != nil && *e.ResourceID == id {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---- providers -----------------------------------------------------------------

type providerRepo struct{ *repos }

func (r providerRepo) Enabled(_ context.Context, name string) (bool, error) {
	defer r.lock()()
	return !r.s.providerOff[name], nil
}

func (r providerRepo) SetEnabled(_ context.Context, name, _ string, enabled bool) error {
	defer r.lock()()
	prev := r.s.providerOff[name]
	r.s.providerOff[name] = !enabled
	r.onUndo(func() { r.s.providerOff[name] = prev })
	return nil
}

type shareRepo struct{ *repos }

func cloneShare(s tokenshares.Share) tokenshares.Share {
	if len(s.DestCipher) > 0 {
		s.DestCipher = append([]byte(nil), s.DestCipher...)
	}
	return s
}

func (r shareRepo) AddCopy(_ context.Context, share tokenshares.Share) error {
	defer r.lock()()
	r.s.tokenShares = append(r.s.tokenShares, cloneShare(share))
	r.onUndo(func() { r.s.tokenShares = r.s.tokenShares[:len(r.s.tokenShares)-1] })
	return nil
}
func (r shareRepo) ListCopies(_ context.Context, id uuid.UUID, user string) ([]tokenshares.Share, error) {
	defer r.lock()()
	result := []tokenshares.Share{}
	for i := len(r.s.tokenShares) - 1; i >= 0 && len(result) < 200; i-- {
		s := r.s.tokenShares[i]
		if s.TransactionID == id && s.UserID == user && s.Channel == tokenshares.Copy {
			result = append(result, cloneShare(s))
		}
	}
	return result, nil
}

func (r shareRepo) GetCopy(_ context.Context, shareID, txnID uuid.UUID, user string) (tokenshares.Share, error) {
	defer r.lock()()
	for _, s := range r.s.tokenShares {
		if s.ID == shareID && s.TransactionID == txnID && s.UserID == user && s.Channel == tokenshares.Copy {
			return cloneShare(s), nil
		}
	}
	return tokenshares.Share{}, store.ErrNotFound
}

func (r shareRepo) AddShare(_ context.Context, share tokenshares.Share) error {
	defer r.lock()()
	r.s.tokenShares = append(r.s.tokenShares, cloneShare(share))
	r.onUndo(func() { r.s.tokenShares = r.s.tokenShares[:len(r.s.tokenShares)-1] })
	return nil
}

func (r shareRepo) GetShare(_ context.Context, shareID uuid.UUID) (tokenshares.Share, error) {
	defer r.lock()()
	for _, s := range r.s.tokenShares {
		if s.ID == shareID {
			return cloneShare(s), nil
		}
	}
	return tokenshares.Share{}, store.ErrNotFound
}

func (r shareRepo) ListShares(_ context.Context, id uuid.UUID, user string) ([]tokenshares.Share, error) {
	defer r.lock()()
	result := []tokenshares.Share{}
	for i := len(r.s.tokenShares) - 1; i >= 0 && len(result) < 200; i-- {
		s := r.s.tokenShares[i]
		if s.TransactionID == id && s.UserID == user {
			out := cloneShare(s)
			out.DestCipher = nil
			result = append(result, out)
		}
	}
	return result, nil
}

func (r shareRepo) CountSends(_ context.Context, userID string, start, end time.Time) (int, error) {
	defer r.lock()()
	n := 0
	for _, s := range r.s.tokenShares {
		if s.UserID == userID && tokenshares.IsSendChannel(s.Channel) && !s.SharedAt.Before(start) && s.SharedAt.Before(end) {
			n++
		}
	}
	return n, nil
}

type giftRepo struct{ *repos }

func (r giftRepo) Create(_ context.Context, g gifts.Gift) error {
	defer r.lock()()
	if _, ok := r.s.gifts[g.TransactionID]; ok {
		return store.ErrDuplicate
	}
	g.ContactCiphertext = append([]byte(nil), g.ContactCiphertext...)
	r.s.gifts[g.TransactionID] = g
	r.onUndo(func() { delete(r.s.gifts, g.TransactionID) })
	return nil
}
func (r giftRepo) Get(_ context.Context, id uuid.UUID) (gifts.Gift, error) {
	defer r.lock()()
	g, ok := r.s.gifts[id]
	if !ok {
		return gifts.Gift{}, store.ErrNotFound
	}
	g.ContactCiphertext = append([]byte(nil), g.ContactCiphertext...)
	return g, nil
}

type candidateRepo struct{ *repos }

func (r candidateRepo) Claim(_ context.Context, since time.Time, limit int) ([]notificationintents.Intent, error) {
	if !r.inTx {
		return nil, errors.New("candidate claim requires transaction")
	}
	if limit <= 0 || since.IsZero() {
		return nil, errors.New("invalid candidate cutoff or limit")
	}
	allowed := map[string]bool{}
	for _, typ := range notificationintents.EventTypes() {
		allowed[typ] = true
	}
	var out []notificationintents.Intent
	for _, e := range r.s.outbox {
		if e.CreatedAt.Before(since) || e.AggregateType != "electricity_transaction" || !allowed[e.Type] {
			continue
		}
		if _, ok := r.s.candidates[e.ID]; ok {
			continue
		}
		out = append(out, notificationintents.Intent{EventID: e.ID, TransactionID: e.AggregateID, EventType: e.Type})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (r candidateRepo) Record(_ context.Context, c notificationintents.Intent) error {
	if !r.inTx {
		return errors.New("candidate record requires transaction")
	}
	if old, ok := r.s.candidates[c.EventID]; ok {
		if old.TransactionID != c.TransactionID || old.EventType != c.EventType {
			return store.ErrConflict
		}
		return nil
	}
	// Claim is the source of candidate metadata; reject unmatched events.
	found := false
	for _, e := range r.s.outbox {
		if e.ID == c.EventID && e.AggregateType == "electricity_transaction" && e.AggregateID == c.TransactionID && e.Type == c.EventType {
			found = true
			break
		}
	}
	allowed := false
	for _, v := range notificationintents.EventTypes() {
		if v == c.EventType {
			allowed = true
		}
	}
	if !found || !allowed {
		return store.ErrNotFound
	}
	r.s.candidates[c.EventID] = c
	r.onUndo(func() { delete(r.s.candidates, c.EventID) })
	return nil
}
func (r candidateRepo) Get(_ context.Context, id uuid.UUID) (notificationintents.Intent, error) {
	defer r.lock()()
	c, ok := r.s.candidates[id]
	if !ok {
		return c, store.ErrNotFound
	}
	return c, nil
}
