package risk

import (
	"context"
	"errors"
	"net"
	"sort"
	"time"

	"github.com/google/uuid"
)

type Engine struct {
	Repo Repo
	Now  func() time.Time
}

func NewEngine(repo Repo, now func() time.Time) *Engine {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Engine{Repo: repo, Now: now}
}

func (e *Engine) now() time.Time {
	if e == nil || e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// Evaluate records a checkout observation, applies every enabled rule, and
// persists the decision. Callers must hold a subject lock (user row and/or
// public-link row) so concurrent checkouts cannot slip past a velocity cap.
// After payment (req.Paid) a DENY is coerced to REVIEW.
func (e *Engine) Evaluate(ctx context.Context, req Request) (Decision, error) {
	now := e.now()
	obs := observationFrom(req, KindCheckout, now)
	if req.Paid {
		obs.Kind = KindPaymentOK
	}
	if err := e.Repo.InsertObservation(ctx, obs); err != nil {
		return Decision{}, err
	}
	if req.CardSignature != "" {
		card := obs
		card.ID = uuid.New()
		card.Kind = KindCardSeen
		if err := e.Repo.InsertObservation(ctx, card); err != nil {
			return Decision{}, err
		}
	}

	rules, err := e.Repo.ListRules(ctx)
	if err != nil {
		return Decision{}, err
	}
	if len(rules) == 0 {
		rules = DefaultRules()
	}

	outcome, code, err := e.decide(ctx, req, rules, now)
	if err != nil {
		return Decision{}, err
	}
	outcome = coerce(req, outcome)
	d := Decision{
		ID: uuid.New(), CreatedAt: now, Outcome: outcome, RuleCode: code,
		Surface: req.Surface, UserID: req.UserID, MeterID: req.MeterID, LinkID: req.LinkID,
		TransactionID: req.TransactionID, AmountMinor: req.AmountMinor,
		EmailHMAC: req.EmailHMAC, IPHMAC: req.IPHMAC, DeviceHMAC: req.DeviceHMAC,
		CardSignature: req.CardSignature,
	}
	if err := e.Repo.InsertDecision(ctx, d); err != nil {
		return Decision{}, err
	}
	if outcome == Deny {
		return d, ErrDenied
	}
	return d, nil
}

// RecordFailure stores a failed-payment observation and pauses a public
// link once the failure threshold is crossed.
func (e *Engine) RecordFailure(ctx context.Context, req Request) error {
	now := e.now()
	obs := observationFrom(req, KindPaymentFailed, now)
	if err := e.Repo.InsertObservation(ctx, obs); err != nil {
		return err
	}
	if req.LinkID == uuid.Nil {
		return nil
	}
	rules, err := e.Repo.ListRules(ctx)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		rules = DefaultRules()
	}
	rule, ok := ruleByCode(rules, RulePublicLinkFailures)
	if !ok || !rule.Enabled {
		return nil
	}
	since := now.Add(-window(rule))
	n, err := e.Repo.CountObservations(ctx, ObservationFilter{Kind: KindPaymentFailed, LinkID: req.LinkID, Since: since})
	if err != nil {
		return err
	}
	if int64(n) >= max(1, rule.Threshold) {
		return e.Repo.PauseLink(ctx, LinkPause{
			LinkID: req.LinkID, RuleCode: RulePublicLinkFailures,
			Reason: "too many failed payments", PausedAt: now,
		})
	}
	return nil
}

func (e *Engine) decide(ctx context.Context, req Request, rules []Rule, now time.Time) (Outcome, string, error) {
	if hit, err := e.denylist(ctx, req); err != nil {
		return Allow, "", err
	} else if hit {
		return Deny, RuleDenylist, nil
	}
	if req.LinkID != uuid.Nil {
		if _, err := e.Repo.LinkPause(ctx, req.LinkID); err == nil {
			return Deny, RuleLinkPaused, nil
		} else if !isNotFound(err) {
			return Allow, "", err
		}
	}

	var reviewCode string
	check := func(code string, fn func(Rule) (bool, error)) error {
		rule, ok := ruleByCode(rules, code)
		if !ok || !rule.Enabled {
			return nil
		}
		hit, err := fn(rule)
		if err != nil || !hit {
			return err
		}
		if rule.Action == Deny {
			return denySentinel{code}
		}
		if reviewCode == "" {
			reviewCode = code
		}
		return nil
	}

	if err := check(RulePublicLinkFailures, func(rule Rule) (bool, error) {
		if req.LinkID == uuid.Nil {
			return false, nil
		}
		n, err := e.Repo.CountObservations(ctx, ObservationFilter{
			Kind: KindPaymentFailed, LinkID: req.LinkID, Since: now.Add(-window(rule)),
		})
		if err != nil {
			return false, err
		}
		if int64(n) >= max(1, rule.Threshold) {
			_ = e.Repo.PauseLink(ctx, LinkPause{
				LinkID: req.LinkID, RuleCode: RulePublicLinkFailures,
				Reason: "too many failed payments", PausedAt: now,
			})
			return true, nil
		}
		return false, nil
	}); err != nil {
		if d, ok := err.(denySentinel); ok {
			return Deny, d.code, nil
		}
		return Allow, "", err
	}

	if err := check(RuleFirstPurchaseCap, func(rule Rule) (bool, error) {
		if req.UserID == "" || rule.Threshold <= 0 || req.AmountMinor <= rule.Threshold {
			return false, nil
		}
		n, err := e.Repo.CountObservations(ctx, ObservationFilter{Kind: KindPaymentOK, UserID: req.UserID})
		if err != nil {
			return false, err
		}
		return n == 0, nil
	}); err != nil {
		if d, ok := err.(denySentinel); ok {
			return Deny, d.code, nil
		}
		return Allow, "", err
	}

	velocities := []struct {
		code string
		filt func(Rule) ObservationFilter
		skip bool
	}{
		{RuleVelocityUser, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCheckout, UserID: req.UserID, Since: now.Add(-window(rule))}
		}, req.UserID == ""},
		{RuleVelocityEmail, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCheckout, EmailHMAC: req.EmailHMAC, Since: now.Add(-window(rule))}
		}, req.EmailHMAC == ""},
		{RuleVelocityIP, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCheckout, IPHMAC: req.IPHMAC, Since: now.Add(-window(rule))}
		}, req.IPHMAC == ""},
		{RuleVelocityMeter, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCheckout, MeterID: req.MeterID, Since: now.Add(-window(rule))}
		}, req.MeterID == uuid.Nil},
		{RuleVelocityDevice, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCheckout, DeviceHMAC: req.DeviceHMAC, Since: now.Add(-window(rule))}
		}, req.DeviceHMAC == ""},
		{RuleVelocityCard, func(rule Rule) ObservationFilter {
			return ObservationFilter{Kind: KindCardSeen, CardSignature: req.CardSignature, Since: now.Add(-window(rule))}
		}, req.CardSignature == ""},
	}
	for _, v := range velocities {
		if v.skip {
			continue
		}
		filt := v.filt
		if err := check(v.code, func(rule Rule) (bool, error) {
			n, err := e.Repo.CountObservations(ctx, filt(rule))
			if err != nil {
				return false, err
			}
			return int64(n) > rule.Threshold, nil
		}); err != nil {
			if d, ok := err.(denySentinel); ok {
				return Deny, d.code, nil
			}
			return Allow, "", err
		}
	}

	if err := check(RuleCardTesting, func(rule Rule) (bool, error) {
		f := ObservationFilter{Since: now.Add(-window(rule))}
		if req.LinkID != uuid.Nil {
			f.LinkID = req.LinkID
		} else if req.UserID != "" {
			f.UserID = req.UserID
		} else {
			return false, nil
		}
		n, err := e.Repo.DistinctCards(ctx, f)
		if err != nil {
			return false, err
		}
		if int64(n) > max(1, rule.Threshold) {
			if req.LinkID != uuid.Nil {
				_ = e.Repo.PauseLink(ctx, LinkPause{
					LinkID: req.LinkID, RuleCode: RuleCardTesting,
					Reason: "too many distinct cards", PausedAt: now,
				})
			}
			return true, nil
		}
		return false, nil
	}); err != nil {
		if d, ok := err.(denySentinel); ok {
			return Deny, d.code, nil
		}
		return Allow, "", err
	}

	if err := check(RuleAmountAnomaly, func(rule Rule) (bool, error) {
		if req.UserID == "" || req.AmountMinor <= 0 {
			return false, nil
		}
		mult := rule.Params.Multiplier
		if mult <= 0 {
			mult = 8
		}
		minH := rule.Params.MinHistory
		if minH <= 0 {
			minH = 5
		}
		amts, err := e.Repo.RecentAmounts(ctx, req.UserID, 20)
		if err != nil {
			return false, err
		}
		if int64(len(amts)) < minH {
			return false, nil
		}
		med := median(amts)
		return med > 0 && req.AmountMinor > med*mult, nil
	}); err != nil {
		if d, ok := err.(denySentinel); ok {
			return Deny, d.code, nil
		}
		return Allow, "", err
	}

	if reviewCode != "" {
		return Review, reviewCode, nil
	}
	return Allow, "", nil
}

func (e *Engine) denylist(ctx context.Context, req Request) (bool, error) {
	pairs := []struct {
		kind DenyKind
		val  string
	}{
		{DenyCard, req.CardSignature},
		{DenyEmail, req.EmailHMAC},
		{DenyPhone, req.PhoneHMAC},
	}
	for _, p := range pairs {
		if p.val == "" {
			continue
		}
		if _, err := e.Repo.MatchDenylist(ctx, p.kind, p.val); err == nil {
			return true, nil
		} else if !isNotFound(err) {
			return false, err
		}
	}
	if req.IP == "" {
		return false, nil
	}
	ip := net.ParseIP(req.IP)
	if ip == nil {
		return false, nil
	}
	ranges, err := e.Repo.ListIPRanges(ctx)
	if err != nil {
		return false, err
	}
	for _, e := range ranges {
		_, cidr, err := net.ParseCIDR(e.Value)
		if err != nil || cidr == nil {
			continue
		}
		if cidr.Contains(ip) {
			return true, nil
		}
	}
	return false, nil
}

func coerce(req Request, o Outcome) Outcome {
	if o != Deny {
		return o
	}
	if !req.Paid {
		return o
	}
	switch req.Surface {
	case SurfacePurchase, SurfacePayForMe, SurfacePot:
		return Review
	}
	return o
}

func observationFrom(req Request, kind Kind, now time.Time) Observation {
	return Observation{
		ID: uuid.New(), ObservedAt: now, Kind: kind, Surface: req.Surface,
		UserID: req.UserID, MeterID: req.MeterID, LinkID: req.LinkID,
		TransactionID: req.TransactionID, EmailHMAC: req.EmailHMAC, IPHMAC: req.IPHMAC,
		DeviceHMAC: req.DeviceHMAC, PhoneHMAC: req.PhoneHMAC, CardSignature: req.CardSignature,
		AmountMinor: req.AmountMinor,
	}
}

func window(r Rule) time.Duration {
	if r.WindowSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(r.WindowSeconds) * time.Second
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

type denySentinel struct{ code string }

func (d denySentinel) Error() string { return d.code }

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
