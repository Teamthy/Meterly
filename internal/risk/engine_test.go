package risk_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/risk"
)

type fakeRepo struct {
	mu     sync.Mutex
	rules  []risk.Rule
	obs    []risk.Observation
	dec    []risk.Decision
	deny   []risk.DenyEntry
	pauses map[uuid.UUID]risk.LinkPause
}

func newFake(rules ...risk.Rule) *fakeRepo {
	if len(rules) == 0 {
		rules = risk.DefaultRules()
	}
	return &fakeRepo{rules: append([]risk.Rule(nil), rules...), pauses: map[uuid.UUID]risk.LinkPause{}}
}

func (f *fakeRepo) ListRules(context.Context) ([]risk.Rule, error) {
	return append([]risk.Rule(nil), f.rules...), nil
}
func (f *fakeRepo) GetRule(_ context.Context, code string) (risk.Rule, error) {
	for _, r := range f.rules {
		if r.Code == code {
			return r, nil
		}
	}
	return risk.Rule{}, risk.ErrNotFound
}
func (f *fakeRepo) UpdateRule(_ context.Context, rule risk.Rule) error {
	for i, r := range f.rules {
		if r.Code == rule.Code {
			f.rules[i] = rule
			return nil
		}
	}
	f.rules = append(f.rules, rule)
	return nil
}
func (f *fakeRepo) InsertObservation(_ context.Context, o risk.Observation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.obs = append(f.obs, o)
	return nil
}
func (f *fakeRepo) matches(o risk.Observation, filt risk.ObservationFilter) bool {
	if filt.Kind != "" && o.Kind != filt.Kind {
		return false
	}
	if filt.UserID != "" && o.UserID != filt.UserID {
		return false
	}
	if filt.MeterID != uuid.Nil && o.MeterID != filt.MeterID {
		return false
	}
	if filt.LinkID != uuid.Nil && o.LinkID != filt.LinkID {
		return false
	}
	if filt.EmailHMAC != "" && o.EmailHMAC != filt.EmailHMAC {
		return false
	}
	if filt.IPHMAC != "" && o.IPHMAC != filt.IPHMAC {
		return false
	}
	if filt.DeviceHMAC != "" && o.DeviceHMAC != filt.DeviceHMAC {
		return false
	}
	if filt.CardSignature != "" && o.CardSignature != filt.CardSignature {
		return false
	}
	if !filt.Since.IsZero() && o.ObservedAt.Before(filt.Since) {
		return false
	}
	return true
}
func (f *fakeRepo) CountObservations(_ context.Context, filt risk.ObservationFilter) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.obs {
		if f.matches(o, filt) {
			n++
		}
	}
	return n, nil
}
func (f *fakeRepo) DistinctCards(_ context.Context, filt risk.ObservationFilter) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	for _, o := range f.obs {
		if o.CardSignature == "" {
			continue
		}
		cf := filt
		cf.Kind = ""
		if !f.matches(o, cf) {
			continue
		}
		seen[o.CardSignature] = struct{}{}
	}
	return len(seen), nil
}
func (f *fakeRepo) RecentAmounts(_ context.Context, userID string, limit int) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for i := len(f.obs) - 1; i >= 0 && len(out) < limit; i-- {
		o := f.obs[i]
		if o.Kind == risk.KindPaymentOK && o.UserID == userID && o.AmountMinor > 0 {
			out = append(out, o.AmountMinor)
		}
	}
	return out, nil
}
func (f *fakeRepo) InsertDecision(_ context.Context, d risk.Decision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dec = append(f.dec, d)
	return nil
}
func (f *fakeRepo) BindDecisionTxn(_ context.Context, id, txnID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.dec {
		if f.dec[i].ID == id {
			f.dec[i].TransactionID = txnID
			return nil
		}
	}
	return risk.ErrNotFound
}
func (f *fakeRepo) ListDecisionsByTxn(_ context.Context, txnID uuid.UUID) ([]risk.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []risk.Decision
	for _, d := range f.dec {
		if d.TransactionID == txnID {
			out = append(out, d)
		}
	}
	return out, nil
}
func (f *fakeRepo) ListDenylist(context.Context) ([]risk.DenyEntry, error) {
	return append([]risk.DenyEntry(nil), f.deny...), nil
}
func (f *fakeRepo) AddDenylist(_ context.Context, e risk.DenyEntry) error {
	f.deny = append(f.deny, e)
	return nil
}
func (f *fakeRepo) RemoveDenylist(_ context.Context, id uuid.UUID) error {
	for i, e := range f.deny {
		if e.ID == id {
			f.deny = append(f.deny[:i], f.deny[i+1:]...)
			return nil
		}
	}
	return risk.ErrNotFound
}
func (f *fakeRepo) MatchDenylist(_ context.Context, kind risk.DenyKind, value string) (risk.DenyEntry, error) {
	for _, e := range f.deny {
		if e.Kind == kind && e.Value == value {
			return e, nil
		}
	}
	return risk.DenyEntry{}, risk.ErrNotFound
}
func (f *fakeRepo) ListIPRanges(context.Context) ([]risk.DenyEntry, error) {
	var out []risk.DenyEntry
	for _, e := range f.deny {
		if e.Kind == risk.DenyIPRange {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeRepo) PauseLink(_ context.Context, p risk.LinkPause) error {
	f.pauses[p.LinkID] = p
	return nil
}
func (f *fakeRepo) LinkPause(_ context.Context, linkID uuid.UUID) (risk.LinkPause, error) {
	p, ok := f.pauses[linkID]
	if !ok {
		return risk.LinkPause{}, risk.ErrNotFound
	}
	return p, nil
}
func (f *fakeRepo) ClearLinkPause(_ context.Context, linkID uuid.UUID) error {
	delete(f.pauses, linkID)
	return nil
}

func only(code string, action risk.Outcome, window int, threshold int64) []risk.Rule {
	return []risk.Rule{{Code: code, Enabled: true, Action: action, WindowSeconds: window, Threshold: threshold}}
}

func TestVelocityThresholdEdge(t *testing.T) {
	ctx := context.Background()
	repo := newFake(only(risk.RuleVelocityUser, risk.Deny, 3600, 2)...)
	eng := risk.NewEngine(repo, nil)
	uid := uuid.NewString()
	req := risk.Request{Surface: risk.SurfacePurchase, UserID: uid, AmountMinor: 1000}
	if _, err := eng.Evaluate(ctx, req); err != nil {
		t.Fatalf("1st: %v", err)
	}
	if _, err := eng.Evaluate(ctx, req); err != nil {
		t.Fatalf("2nd at cap: %v", err)
	}
	if _, err := eng.Evaluate(ctx, req); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("3rd want denied, got %v", err)
	}
}

func TestVelocityWindowExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	repo := newFake(only(risk.RuleVelocityUser, risk.Deny, 60, 1)...)
	eng := risk.NewEngine(repo, func() time.Time { return now })
	uid := uuid.NewString()
	req := risk.Request{Surface: risk.SurfacePurchase, UserID: uid}
	if _, err := eng.Evaluate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Evaluate(ctx, req); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("inside window: %v", err)
	}
	now = now.Add(61 * time.Second)
	if _, err := eng.Evaluate(ctx, req); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestFirstPurchaseCap(t *testing.T) {
	ctx := context.Background()
	repo := newFake(only(risk.RuleFirstPurchaseCap, risk.Deny, 0, 1000)...)
	eng := risk.NewEngine(repo, nil)
	uid := uuid.NewString()
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, UserID: uid, AmountMinor: 1000}); err != nil {
		t.Fatalf("at cap: %v", err)
	}
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, UserID: uid, AmountMinor: 1001}); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("over cap: %v", err)
	}
}

func TestAmountAnomaly(t *testing.T) {
	ctx := context.Background()
	repo := newFake([]risk.Rule{{
		Code: risk.RuleAmountAnomaly, Enabled: true, Action: risk.Review,
		Params: risk.RuleParams{Multiplier: 3, MinHistory: 5},
	}}...)
	eng := risk.NewEngine(repo, nil)
	uid := uuid.NewString()
	for i := 0; i < 5; i++ {
		if err := repo.InsertObservation(ctx, risk.Observation{
			ID: uuid.New(), ObservedAt: time.Now(), Kind: risk.KindPaymentOK,
			UserID: uid, AmountMinor: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, UserID: uid, AmountMinor: 1000})
	if err != nil || d.Outcome != risk.Allow {
		t.Fatalf("normal %+v %v", d, err)
	}
	d, err = eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, UserID: uid, AmountMinor: 4000})
	if err != nil || d.Outcome != risk.Review || d.RuleCode != risk.RuleAmountAnomaly {
		t.Fatalf("anomaly %+v %v", d, err)
	}
}

func TestCardTestingPausesLink(t *testing.T) {
	ctx := context.Background()
	repo := newFake(only(risk.RuleCardTesting, risk.Review, 3600, 2)...)
	eng := risk.NewEngine(repo, nil)
	link := uuid.New()
	for i, sig := range []string{"sig-a", "sig-b", "sig-c"} {
		d, err := eng.Evaluate(ctx, risk.Request{
			Surface: risk.SurfacePayForMe, LinkID: link, CardSignature: sig, Paid: true,
		})
		if i < 2 {
			if err != nil {
				t.Fatalf("%d: %v", i, err)
			}
			continue
		}
		if err != nil || d.Outcome != risk.Review {
			t.Fatalf("3rd card %+v %v", d, err)
		}
	}
	if _, err := repo.LinkPause(ctx, link); err != nil {
		t.Fatalf("link should be paused: %v", err)
	}
}

func TestPublicLinkFailuresPauseAndDeny(t *testing.T) {
	ctx := context.Background()
	repo := newFake(only(risk.RulePublicLinkFailures, risk.Deny, 900, 2)...)
	eng := risk.NewEngine(repo, nil)
	link := uuid.New()
	req := risk.Request{Surface: risk.SurfacePayForMe, LinkID: link}
	if err := eng.RecordFailure(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordFailure(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LinkPause(ctx, link); err != nil {
		t.Fatalf("expected pause: %v", err)
	}
	if _, err := eng.Evaluate(ctx, req); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("paused link: %v", err)
	}
}

func TestDenylistCardEmailIP(t *testing.T) {
	ctx := context.Background()
	repo := newFake()
	eng := risk.NewEngine(repo, nil)
	_ = repo.AddDenylist(ctx, risk.DenyEntry{ID: uuid.New(), Kind: risk.DenyEmail, Value: "email-hmac"})
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, EmailHMAC: "email-hmac"}); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("email: %v", err)
	}
	_ = repo.AddDenylist(ctx, risk.DenyEntry{ID: uuid.New(), Kind: risk.DenyCard, Value: "sig-bad"})
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfaceSaveCard, CardSignature: "sig-bad"}); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("card: %v", err)
	}
	_ = repo.AddDenylist(ctx, risk.DenyEntry{ID: uuid.New(), Kind: risk.DenyIPRange, Value: "10.1.2.0/24"})
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, IP: "10.1.2.9"}); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("cidr: %v", err)
	}
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, IP: "10.9.9.9"}); err != nil {
		t.Fatalf("outside cidr: %v", err)
	}
}

func TestNeverDenyAfterPayment(t *testing.T) {
	ctx := context.Background()
	repo := newFake()
	eng := risk.NewEngine(repo, nil)
	_ = repo.AddDenylist(ctx, risk.DenyEntry{ID: uuid.New(), Kind: risk.DenyEmail, Value: "hmac"})
	d, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, EmailHMAC: "hmac", Paid: true})
	if err != nil || d.Outcome != risk.Review || d.RuleCode != risk.RuleDenylist {
		t.Fatalf("paid deny must become review: %+v %v", d, err)
	}
	if _, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfaceSaveCard, EmailHMAC: "hmac"}); !errors.Is(err, risk.ErrDenied) {
		t.Fatalf("save-card may still deny: %v", err)
	}
}

func TestDisabledRuleIgnored(t *testing.T) {
	ctx := context.Background()
	repo := newFake([]risk.Rule{{Code: risk.RuleVelocityUser, Enabled: false, Action: risk.Deny, Threshold: 0, WindowSeconds: 3600}}...)
	eng := risk.NewEngine(repo, nil)
	d, err := eng.Evaluate(ctx, risk.Request{Surface: risk.SurfacePurchase, UserID: uuid.NewString()})
	if err != nil || d.Outcome != risk.Allow {
		t.Fatalf("%+v %v", d, err)
	}
}
