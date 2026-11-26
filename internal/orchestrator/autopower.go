package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const (
	autoPowerChargeRefPrefix = "apchg:"
	autoPowerIdemPrefix      = "autopower:"
)

// SavePaymentMethodParams captures explicit user consent to store a reusable
// card authorization from one of the caller's settled transactions.
type SavePaymentMethodParams struct {
	UserID        string
	TransactionID uuid.UUID
	Consent       bool
	ConsentText   string
}

// CreateRuleParams defines inputs for POST /v1/autopower/rules.
type CreateRuleParams struct {
	UserID                 string
	MeterID                uuid.UUID
	PaymentMethodID        uuid.UUID
	TriggerType            autopower.TriggerType
	AmountMinor            int64
	MonthlyCapMinor        int64
	DeliveryPreference     DeliveryPreference
	Frequency              autopower.Frequency
	DaysOfWeek             []int
	DaysOfMonth            []int
	TimeOfDay              string
	Timezone               string
	EndsAt                 *time.Time
	LowBalanceThresholdKWh *float64
}

// UpdateRuleParams defines mutable fields for PATCH /v1/autopower/rules/{id}.
type UpdateRuleParams struct {
	UserID                 string
	RuleID                 uuid.UUID
	PaymentMethodID        *uuid.UUID
	TriggerType            *autopower.TriggerType
	AmountMinor            *int64
	MonthlyCapMinor        *int64
	DeliveryPreference     *DeliveryPreference
	Frequency              *autopower.Frequency
	DaysOfWeek             *[]int
	DaysOfMonth            *[]int
	TimeOfDay              *string
	EndsAt                 *time.Time
	ClearEndsAt            bool
	LowBalanceThresholdKWh *float64
	Status                 *autopower.RuleStatus
}

// RuleView is the redacted user-facing representation of an AutoPower rule and
// its upcoming/recent run state.
type RuleView struct {
	RuleID                    uuid.UUID             `json:"rule_id"`
	MeterID                   uuid.UUID             `json:"meter_id"`
	MaskedMeterNumber         string                `json:"masked_meter_number,omitempty"`
	PaymentMethodID           uuid.UUID             `json:"payment_method_id"`
	TriggerType               autopower.TriggerType `json:"trigger_type"`
	AmountMinor               int64                 `json:"amount_minor"`
	MonthlyCapMinor           int64                 `json:"monthly_cap_minor"`
	Currency                  string                `json:"currency"`
	DeliveryPreference        string                `json:"delivery_preference"`
	Frequency                 autopower.Frequency   `json:"frequency,omitempty"`
	DaysOfWeek                []int                 `json:"days_of_week,omitempty"`
	DaysOfMonth               []int                 `json:"days_of_month,omitempty"`
	TimeOfDay                 string                `json:"time_of_day,omitempty"`
	Timezone                  string                `json:"timezone"`
	EndsAt                    *time.Time            `json:"ends_at,omitempty"`
	LowBalanceThresholdKWh    *float64              `json:"low_balance_threshold_kwh,omitempty"`
	Status                    autopower.RuleStatus  `json:"status"`
	PauseReason               string                `json:"pause_reason,omitempty"`
	ConsecutiveFailures       int                   `json:"consecutive_failures"`
	NextScheduledAt           *time.Time            `json:"next_scheduled_at,omitempty"`
	LastLowBalanceTriggeredAt *time.Time            `json:"last_low_balance_triggered_at,omitempty"`
	LastReadingKWh            *float64              `json:"last_reading_kwh,omitempty"`
	LastReadingAt             *time.Time            `json:"last_reading_at,omitempty"`
	UpcomingRun               *autopower.Run        `json:"upcoming_run,omitempty"`
	RecentRuns                []*autopower.Run      `json:"recent_runs,omitempty"`
	CreatedAt                 time.Time             `json:"created_at"`
	UpdatedAt                 time.Time             `json:"updated_at"`
}

// ---- Saved payment methods -------------------------------------------------

// SavePaymentMethod verifies the caller's settled card transaction with the
// PSP, validates reusability and card expiry, and stores the bound AEAD-encrypted
// authorization code alongside explicit consent metadata.
func (o *Orchestrator) SavePaymentMethod(ctx context.Context, p SavePaymentMethodParams) (*autopower.SavedPaymentMethod, error) {
	if !p.Consent {
		return nil, autopower.ErrConsentRequired
	}
	if existing, err := o.st.Repos().AutoPower().GetActivePaymentMethodBySourceTxn(ctx, p.UserID, p.TransactionID); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	t, err := o.Get(ctx, p.TransactionID)
	if err != nil {
		return nil, err
	}
	if t.CustomerID != p.UserID {
		return nil, tx.ErrNotFound
	}
	if t.EffectiveFundingSource() != tx.FundingPSP || t.Payment != tx.PayConfirmed || t.PaymentProviderRef == "" {
		return nil, autopower.ErrPaymentMethodNotReusable
	}

	pctx, cancel := o.pctx(ctx)
	v, err := o.payments.VerifyPayment(pctx, t.PaymentProviderRef)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("verify payment for save: %w", err)
	}
	if v.Status != providers.PaymentSucceeded || v.Authorization == nil || !v.Authorization.Reusable || strings.TrimSpace(v.Authorization.AuthorizationCode) == "" {
		return nil, autopower.ErrPaymentMethodNotReusable
	}
	auth := v.Authorization
	if auth.Channel != "" && !strings.EqualFold(auth.Channel, "card") {
		return nil, autopower.ErrPaymentMethodNotReusable
	}
	expYear := autopower.NormalizeExpYear(auth.ExpYear)
	if err := autopower.ValidateCardExpiry(auth.ExpMonth, expYear); err != nil {
		return nil, fmt.Errorf("%w: %v", autopower.ErrPaymentMethodNotReusable, err)
	}

	now := o.Now().UTC()
	loc := autopower.LagosLocation()
	probe := &autopower.SavedPaymentMethod{ExpMonth: auth.ExpMonth, ExpYear: expYear}
	if probe.IsExpiredAt(now, loc) {
		return nil, autopower.ErrCardExpired
	}

	chargeEmail := strings.TrimSpace(auth.CustomerEmail)
	if chargeEmail == "" {
		if contacts, cErr := o.st.Repos().Users().Contacts(ctx, p.UserID); cErr == nil && len(contacts.EmailCiphertext) > 0 {
			if dec, dErr := notifications.OpenUserEmail(o.box, p.UserID, contacts.EmailCiphertext); dErr == nil {
				chargeEmail = strings.TrimSpace(dec)
			}
		}
	}
	if chargeEmail == "" {
		return nil, fmt.Errorf("%w: verified customer email required for recurring card charges", autopower.ErrPaymentMethodNotReusable)
	}

	consentText := strings.TrimSpace(p.ConsentText)
	if consentText == "" {
		consentText = autopower.DefaultConsentTextV1
	}
	brand := strings.TrimSpace(auth.CardBrand)
	if brand == "" {
		brand = "card"
	}
	last4 := strings.TrimSpace(auth.Last4)
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	if len(last4) != 4 {
		return nil, autopower.ErrPaymentMethodNotReusable
	}

	var saved *autopower.SavedPaymentMethod
	err = o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, p.UserID); err != nil {
			return err
		}
		if existing, gErr := r.AutoPower().GetActivePaymentMethodBySourceTxn(ctx, p.UserID, p.TransactionID); gErr == nil {
			saved = existing
			return nil
		} else if !errors.Is(gErr, store.ErrNotFound) {
			return gErr
		}

		pmID := uuid.New()
		authCT, err := autopower.SealAuthCode(o.box, p.UserID, pmID, auth.AuthorizationCode)
		if err != nil {
			return err
		}
		emailCT, err := autopower.SealAuthEmail(o.box, p.UserID, pmID, chargeEmail)
		if err != nil {
			return err
		}
		pm := &autopower.SavedPaymentMethod{
			ID:                  pmID,
			UserID:              p.UserID,
			Provider:            o.payments.Name(),
			SourceTransactionID: p.TransactionID,
			AuthCodeCiphertext:  authCT,
			EmailCiphertext:     emailCT,
			KeyVersion:          o.box.KeyVersion,
			CardBrand:           brand,
			Last4:               last4,
			ExpMonth:            auth.ExpMonth,
			ExpYear:             expYear,
			Signature:           auth.Signature,
			ConsentVersion:      autopower.DefaultConsentVersion,
			ConsentText:         consentText,
			ConsentedAt:         now,
			CreatedAt:           now,
		}
		if err := r.AutoPower().CreatePaymentMethod(ctx, pm); err != nil {
			return err
		}
		actor := audit.Actor{UserID: p.UserID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("payment_method.saved", "payment_method", pm.ID, map[string]any{
			"provider":              pm.Provider,
			"card_brand":            pm.CardBrand,
			"last4":                 pm.Last4,
			"exp_month":             pm.ExpMonth,
			"exp_year":              pm.ExpYear,
			"consent_version":       pm.ConsentVersion,
			"source_transaction_id": p.TransactionID.String(),
		})); err != nil {
			return err
		}
		saved = pm
		return nil
	})
	return saved, err
}

func (o *Orchestrator) ListPaymentMethods(ctx context.Context, userID string) ([]*autopower.SavedPaymentMethod, error) {
	return o.st.Repos().AutoPower().ListPaymentMethods(ctx, userID)
}

// DeletePaymentMethod soft-deletes a saved payment method, atomically stops
// every AutoPower rule referencing it, cancels any unstarted scheduled runs,
// and records audit events.
func (o *Orchestrator) DeletePaymentMethod(ctx context.Context, userID string, pmID uuid.UUID) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, userID); err != nil {
			return err
		}
		pm, err := r.AutoPower().GetPaymentMethodForUpdate(ctx, pmID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrPaymentMethodNotFound
		}
		if err != nil {
			return err
		}
		if pm.UserID != userID || pm.DeletedAt != nil {
			return autopower.ErrPaymentMethodNotFound
		}
		if err := r.AutoPower().DeletePaymentMethod(ctx, pmID, now); err != nil {
			return err
		}

		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		rules, err := r.AutoPower().ListRulesByPaymentMethodForUpdate(ctx, pmID)
		if err != nil {
			return err
		}
		stopped := 0
		for _, rl := range rules {
			wasActive := rl.Status == autopower.RuleActive
			rl.Status = autopower.RulePaused
			rl.PauseReason = autopower.PausePaymentMethodDeleted
			rl.NextScheduledAt = nil
			rl.NextPollAt = nil
			rl.LeaseUntil = time.Time{}
			rl.UpdatedAt = now
			if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
				return err
			}
			if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
				return err
			}
			stopped++
			if err := r.Audit().Add(ctx, actor.Event("autopower.rule.stopped", "autopower_rule", rl.ID, map[string]any{
				"reason": autopower.PausePaymentMethodDeleted,
			})); err != nil {
				return err
			}
			if wasActive {
				ev := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{
					"reason": autopower.PausePaymentMethodDeleted,
				})
				ev.CreatedAt = now
				if err := r.Outbox().Add(ctx, ev); err != nil {
					return err
				}
			}
		}
		return r.Audit().Add(ctx, actor.Event("payment_method.deleted", "payment_method", pmID, map[string]any{
			"stopped_rules": stopped,
		}))
	})
}

// ---- Rules & skip ----------------------------------------------------------

func (o *Orchestrator) CreateAutoPowerRule(ctx context.Context, p CreateRuleParams) (*RuleView, error) {
	if p.AmountMinor < o.cfg.MinAmountMinor || p.AmountMinor > o.cfg.MaxAmountMinor {
		return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
	}
	if p.MonthlyCapMinor <= 0 || p.MonthlyCapMinor < p.AmountMinor {
		return nil, autopower.ErrInvalidCap
	}
	pref := p.DeliveryPreference
	if pref == "" {
		pref = PrefAuto
	}
	switch pref {
	case PrefAuto, PrefRemote, PrefToken:
	default:
		return nil, fmt.Errorf("%w %q", ErrInvalidPreference, pref)
	}
	trig := p.TriggerType
	if trig == "" {
		trig = autopower.TriggerSchedule
	}
	if !trig.UsesSchedule() && !trig.UsesLowBalance() {
		return nil, fmt.Errorf("%w: %q", autopower.ErrInvalidTrigger, trig)
	}
	tz := strings.TrimSpace(p.Timezone)
	if tz == "" {
		tz = autopower.DefaultTimezone
	}
	if tz != autopower.DefaultTimezone {
		return nil, autopower.ErrInvalidTimezone
	}

	now := o.Now().UTC()
	loc := autopower.LagosLocation()

	var sched autopower.ScheduleSpec
	var firstSlot time.Time
	if trig.UsesSchedule() {
		sched = autopower.ScheduleSpec{
			Frequency:   p.Frequency,
			DaysOfWeek:  p.DaysOfWeek,
			DaysOfMonth: p.DaysOfMonth,
			TimeOfDay:   strings.TrimSpace(p.TimeOfDay),
			EndsAt:      p.EndsAt,
		}
		if err := sched.Validate(); err != nil {
			return nil, err
		}
		var ok bool
		firstSlot, ok = autopower.NextSlotAfter(now, sched, loc)
		if !ok {
			return nil, fmt.Errorf("%w: ends_at precedes the first scheduled run", autopower.ErrInvalidSchedule)
		}
	}

	var lowBalMilli int64
	if trig.UsesLowBalance() {
		if p.LowBalanceThresholdKWh == nil {
			return nil, autopower.ErrLowBalanceThreshold
		}
		var err error
		lowBalMilli, err = autopower.MilliKWhFromFloat(*p.LowBalanceThresholdKWh)
		if err != nil {
			return nil, err
		}
	}

	var out *RuleView
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, p.UserID); err != nil {
			return err
		}
		m, err := registry.AuthorizeForUpdateIn(ctx, r, p.UserID, p.MeterID, meters.PermPurchase)
		if err != nil {
			return err
		}
		if pref == PrefRemote && !(o.remote != nil && m.Capabilities.RemoteCreditUsable()) {
			return ErrNotEligible
		}
		if trig.UsesLowBalance() {
			if o.readings == nil || !m.Capabilities.RemoteReadingConfirmed() {
				return autopower.ErrLowBalanceIneligible
			}
		}

		pm, err := r.AutoPower().GetPaymentMethodForUpdate(ctx, p.PaymentMethodID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrPaymentMethodNotFound
		}
		if err != nil {
			return err
		}
		if pm.UserID != p.UserID || pm.DeletedAt != nil {
			return autopower.ErrPaymentMethodNotFound
		}
		if pm.IsExpiredAt(now, loc) {
			return autopower.ErrCardExpired
		}

		rl := &autopower.Rule{
			ID:                 uuid.New(),
			UserID:             p.UserID,
			MeterID:            p.MeterID,
			PaymentMethodID:    p.PaymentMethodID,
			TriggerType:        trig,
			AmountMinor:        p.AmountMinor,
			MonthlyCapMinor:    p.MonthlyCapMinor,
			Currency:           string(money.NGN),
			DeliveryPreference: string(pref),
			Timezone:           tz,
			LowBalanceMilliKWh: lowBalMilli,
			Status:             autopower.RuleActive,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		if trig.UsesSchedule() {
			rl.Frequency = sched.Frequency
			rl.DaysOfWeek = sched.DaysOfWeek
			rl.DaysOfMonth = sched.DaysOfMonth
			rl.TimeOfDay = sched.TimeOfDay
			if sched.EndsAt != nil {
				e := sched.EndsAt.UTC()
				rl.EndsAt = &e
			}
			if nextAfterFirst, ok := autopower.NextSlotAfter(firstSlot, autopower.ScheduleSpecFromRule(rl), loc); ok {
				rl.NextScheduledAt = &nextAfterFirst
			}
		}
		if trig.UsesLowBalance() {
			pollAt := now
			rl.NextPollAt = &pollAt
		}
		if err := r.AutoPower().CreateRule(ctx, rl); err != nil {
			return err
		}

		if trig.UsesSchedule() {
			initialRun := &autopower.Run{
				ID:            uuid.New(),
				RuleID:        rl.ID,
				UserID:        rl.UserID,
				MeterID:       rl.MeterID,
				TriggerSource: autopower.SourceSchedule,
				ScheduledFor:  firstSlot,
				AmountMinor:   rl.AmountMinor,
				TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
				Currency:      rl.Currency,
				Status:        autopower.RunScheduled,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := r.AutoPower().CreateRun(ctx, initialRun); err != nil {
				return err
			}
		}

		actor := audit.Actor{UserID: p.UserID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("autopower.rule.created", "autopower_rule", rl.ID, map[string]any{
			"meter_id":          rl.MeterID.String(),
			"payment_method_id": rl.PaymentMethodID.String(),
			"trigger_type":      string(rl.TriggerType),
			"amount_minor":      rl.AmountMinor,
			"monthly_cap_minor": rl.MonthlyCapMinor,
		})); err != nil {
			return err
		}

		view, err := buildRuleViewInRepos(ctx, r, rl, m.Masked)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	return out, err
}

func (o *Orchestrator) ListAutoPowerRules(ctx context.Context, userID string) ([]*RuleView, error) {
	r := o.st.Repos()
	rules, err := r.AutoPower().ListRulesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*RuleView, 0, len(rules))
	for _, rl := range rules {
		masked := ""
		if m, mErr := r.Meters().Get(ctx, rl.MeterID); mErr == nil {
			masked = m.Masked
		}
		v, err := buildRuleViewInRepos(ctx, r, rl, masked)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (o *Orchestrator) UpdateAutoPowerRule(ctx context.Context, p UpdateRuleParams) (*RuleView, error) {
	now := o.Now().UTC()
	loc := autopower.LagosLocation()

	var out *RuleView
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, p.UserID); err != nil {
			return err
		}
		rl0, err := r.AutoPower().GetRule(ctx, p.RuleID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrRuleNotFound
		}
		if err != nil {
			return err
		}
		if rl0.UserID != p.UserID || rl0.Status == autopower.RuleCancelled {
			return autopower.ErrRuleNotFound
		}

		// Lock meter -> payment method -> rule in canonical order.
		m, mErr := r.Meters().GetForUpdate(ctx, rl0.MeterID)
		if errors.Is(mErr, store.ErrNotFound) {
			return meters.ErrNotFound
		}
		if mErr != nil {
			return mErr
		}

		pmID := rl0.PaymentMethodID
		if p.PaymentMethodID != nil {
			pmID = *p.PaymentMethodID
		}
		pm, err := r.AutoPower().GetPaymentMethodForUpdate(ctx, pmID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrPaymentMethodNotFound
		}
		if err != nil {
			return err
		}
		if pm.UserID != p.UserID || pm.DeletedAt != nil {
			return autopower.ErrPaymentMethodNotFound
		}

		rl, err := r.AutoPower().GetRuleForUpdate(ctx, p.RuleID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrRuleNotFound
		}
		if err != nil {
			return err
		}
		if rl.UserID != p.UserID || rl.Status == autopower.RuleCancelled {
			return autopower.ErrRuleNotFound
		}

		targetStatus := rl.Status
		if p.Status != nil {
			switch *p.Status {
			case autopower.RuleActive, autopower.RulePaused:
				targetStatus = *p.Status
			default:
				return fmt.Errorf("%w: status must be ACTIVE or PAUSED", autopower.ErrInvalidSchedule)
			}
		}

		scheduleTouched := false
		if p.PaymentMethodID != nil && *p.PaymentMethodID != rl.PaymentMethodID {
			rl.PaymentMethodID = *p.PaymentMethodID
		}
		if p.AmountMinor != nil {
			if *p.AmountMinor < o.cfg.MinAmountMinor || *p.AmountMinor > o.cfg.MaxAmountMinor {
				return fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
			}
			if *p.AmountMinor != rl.AmountMinor {
				rl.AmountMinor = *p.AmountMinor
				scheduleTouched = true
			}
		}
		if p.MonthlyCapMinor != nil {
			rl.MonthlyCapMinor = *p.MonthlyCapMinor
		}
		if rl.MonthlyCapMinor <= 0 || rl.MonthlyCapMinor < rl.AmountMinor {
			return autopower.ErrInvalidCap
		}
		if p.DeliveryPreference != nil {
			switch *p.DeliveryPreference {
			case PrefAuto, PrefRemote, PrefToken:
				rl.DeliveryPreference = string(*p.DeliveryPreference)
			default:
				return fmt.Errorf("%w %q", ErrInvalidPreference, *p.DeliveryPreference)
			}
		}
		if p.TriggerType != nil {
			if !p.TriggerType.UsesSchedule() && !p.TriggerType.UsesLowBalance() {
				return fmt.Errorf("%w: %q", autopower.ErrInvalidTrigger, *p.TriggerType)
			}
			if *p.TriggerType != rl.TriggerType {
				rl.TriggerType = *p.TriggerType
				scheduleTouched = true
			}
		}
		if p.Frequency != nil {
			rl.Frequency = *p.Frequency
			scheduleTouched = true
		}
		if p.DaysOfWeek != nil {
			rl.DaysOfWeek = append([]int(nil), (*p.DaysOfWeek)...)
			scheduleTouched = true
		}
		if p.DaysOfMonth != nil {
			rl.DaysOfMonth = append([]int(nil), (*p.DaysOfMonth)...)
			scheduleTouched = true
		}
		if p.TimeOfDay != nil {
			rl.TimeOfDay = strings.TrimSpace(*p.TimeOfDay)
			scheduleTouched = true
		}
		if p.ClearEndsAt {
			rl.EndsAt = nil
			scheduleTouched = true
		} else if p.EndsAt != nil {
			e := p.EndsAt.UTC()
			rl.EndsAt = &e
			scheduleTouched = true
		}
		if p.LowBalanceThresholdKWh != nil {
			milli, mErr := autopower.MilliKWhFromFloat(*p.LowBalanceThresholdKWh)
			if mErr != nil {
				return mErr
			}
			rl.LowBalanceMilliKWh = milli
		}
		if !rl.TriggerType.UsesLowBalance() {
			rl.LowBalanceMilliKWh = 0
			rl.NextPollAt = nil
		}
		if !rl.TriggerType.UsesSchedule() {
			rl.Frequency = autopower.FreqNone
			rl.DaysOfWeek = nil
			rl.DaysOfMonth = nil
			rl.TimeOfDay = ""
			rl.NextScheduledAt = nil
		}

		// When active (or being resumed), re-verify grant, card expiry, delivery
		// preference and remote-reading capability.
		if targetStatus == autopower.RuleActive {
			gr, gErr := r.Meters().ActiveGrant(ctx, rl.MeterID, p.UserID)
			if errors.Is(gErr, store.ErrNotFound) || (gErr == nil && !gr.Permission.Allows(meters.PermPurchase)) {
				return meters.ErrForbidden
			}
			if gErr != nil {
				return gErr
			}
			if pm.IsExpiredAt(now, loc) {
				return autopower.ErrCardExpired
			}
			if DeliveryPreference(rl.DeliveryPreference) == PrefRemote && !(o.remote != nil && m.Capabilities.RemoteCreditUsable()) {
				return ErrNotEligible
			}
			if rl.TriggerType.UsesLowBalance() {
				if rl.LowBalanceMilliKWh <= 0 {
					return autopower.ErrLowBalanceThreshold
				}
				if o.readings == nil || !m.Capabilities.RemoteReadingConfirmed() {
					return autopower.ErrLowBalanceIneligible
				}
				if rl.NextPollAt == nil {
					pollAt := now
					rl.NextPollAt = &pollAt
				}
			}
			if rl.TriggerType.UsesSchedule() {
				spec := autopower.ScheduleSpecFromRule(rl)
				if err := spec.Validate(); err != nil {
					return err
				}
				rl.DaysOfWeek = spec.DaysOfWeek
				rl.DaysOfMonth = spec.DaysOfMonth
				rl.TimeOfDay = spec.TimeOfDay
			}
		}

		wasPaused := rl.Status == autopower.RulePaused
		rl.Status = targetStatus
		rl.LeaseUntil = time.Time{}
		rl.UpdatedAt = now

		if targetStatus == autopower.RulePaused {
			if rl.PauseReason == "" || p.Status != nil {
				rl.PauseReason = autopower.PauseUser
			}
			rl.NextScheduledAt = nil
			rl.NextPollAt = nil
			if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
				return err
			}
		} else {
			if wasPaused {
				rl.PauseReason = ""
				rl.ConsecutiveFailures = 0
			}
			if !rl.TriggerType.UsesSchedule() {
				if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
					return err
				}
			} else if wasPaused || scheduleTouched {
				if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
					return err
				}
				spec := autopower.ScheduleSpecFromRule(rl)
				firstSlot, ok := autopower.NextSlotAfter(now, spec, loc)
				if !ok {
					return fmt.Errorf("%w: ends_at precedes the next scheduled run", autopower.ErrInvalidSchedule)
				}
				if nextSlot, nOK := autopower.NextSlotAfter(firstSlot, spec, loc); nOK {
					rl.NextScheduledAt = &nextSlot
				} else {
					rl.NextScheduledAt = nil
				}
				initialRun := &autopower.Run{
					ID:            uuid.New(),
					RuleID:        rl.ID,
					UserID:        rl.UserID,
					MeterID:       rl.MeterID,
					TriggerSource: autopower.SourceSchedule,
					ScheduledFor:  firstSlot,
					AmountMinor:   rl.AmountMinor,
					TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
					Currency:      rl.Currency,
					Status:        autopower.RunScheduled,
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if err := r.AutoPower().CreateRun(ctx, initialRun); err != nil && !errors.Is(err, store.ErrDuplicate) {
					return err
				}
			}
		}

		if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
			return err
		}

		actor := audit.Actor{UserID: p.UserID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("autopower.rule.updated", "autopower_rule", rl.ID, map[string]any{
			"status":            string(rl.Status),
			"amount_minor":      rl.AmountMinor,
			"monthly_cap_minor": rl.MonthlyCapMinor,
			"trigger_type":      string(rl.TriggerType),
		})); err != nil {
			return err
		}

		view, err := buildRuleViewInRepos(ctx, r, rl, m.Masked)
		if err != nil {
			return err
		}
		out = view
		return nil
	})
	return out, err
}

func (o *Orchestrator) DeleteAutoPowerRule(ctx context.Context, userID string, ruleID uuid.UUID) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, userID); err != nil {
			return err
		}
		rl, err := r.AutoPower().GetRuleForUpdate(ctx, ruleID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrRuleNotFound
		}
		if err != nil {
			return err
		}
		if rl.UserID != userID || rl.Status == autopower.RuleCancelled {
			return autopower.ErrRuleNotFound
		}
		rl.Status = autopower.RuleCancelled
		rl.PauseReason = ""
		rl.NextScheduledAt = nil
		rl.NextPollAt = nil
		rl.LeaseUntil = time.Time{}
		rl.UpdatedAt = now
		if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
			return err
		}
		if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("autopower.rule.cancelled", "autopower_rule", rl.ID, nil))
	})
}

// SkipAutoPowerRun marks a future unstarted scheduled run SKIPPED_USER and
// materialises the subsequent cycle's run so upcoming_run advances cleanly.
func (o *Orchestrator) SkipAutoPowerRun(ctx context.Context, userID string, runID uuid.UUID) (*autopower.Run, error) {
	now := o.Now().UTC()
	var out *autopower.Run
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Users().LockForUpdate(ctx, userID); err != nil {
			return err
		}
		existing, err := r.AutoPower().GetRun(ctx, runID)
		if errors.Is(err, store.ErrNotFound) {
			return autopower.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		if existing.UserID != userID {
			return autopower.ErrRunNotFound
		}
		if existing.Status == autopower.RunSkippedUser {
			out = existing
			return nil
		}

		rl, err := r.AutoPower().GetRuleForUpdate(ctx, existing.RuleID)
		if err != nil {
			return err
		}
		rn, err := r.AutoPower().GetRunForUpdate(ctx, runID)
		if err != nil {
			return err
		}
		if rn.Status == autopower.RunSkippedUser {
			out = rn
			return nil
		}
		if rn.Status != autopower.RunScheduled {
			return autopower.ErrRunNotSkippable
		}

		rn.Status = autopower.RunSkippedUser
		rn.CompletedAt = &now
		rn.LeaseUntil = time.Time{}
		rn.UpdatedAt = now
		if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
			return err
		}
		if err := o.ensureNextScheduledRunInTx(ctx, r, rl, rn.ScheduledFor, now); err != nil {
			return err
		}
		actor := audit.Actor{UserID: userID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("autopower.run.skipped", "autopower_run", rn.ID, map[string]any{
			"rule_id":       rl.ID.String(),
			"scheduled_for": rn.ScheduledFor.Format(time.RFC3339),
		})); err != nil {
			return err
		}
		out = rn
		return nil
	})
	return out, err
}

func buildRuleViewInRepos(ctx context.Context, r store.Repos, rl *autopower.Rule, maskedMeter string) (*RuleView, error) {
	runs, err := r.AutoPower().ListRunsForRule(ctx, rl.ID, 10)
	if err != nil {
		return nil, err
	}
	var upcoming *autopower.Run
	for _, rn := range runs {
		if rn.Status == autopower.RunScheduled {
			if upcoming == nil || rn.ScheduledFor.Before(upcoming.ScheduledFor) {
				upcoming = rn
			}
		}
	}
	v := &RuleView{
		RuleID:                    rl.ID,
		MeterID:                   rl.MeterID,
		MaskedMeterNumber:         maskedMeter,
		PaymentMethodID:           rl.PaymentMethodID,
		TriggerType:               rl.TriggerType,
		AmountMinor:               rl.AmountMinor,
		MonthlyCapMinor:           rl.MonthlyCapMinor,
		Currency:                  rl.Currency,
		DeliveryPreference:        rl.DeliveryPreference,
		Frequency:                 rl.Frequency,
		DaysOfWeek:                append([]int(nil), rl.DaysOfWeek...),
		DaysOfMonth:               append([]int(nil), rl.DaysOfMonth...),
		TimeOfDay:                 rl.TimeOfDay,
		Timezone:                  rl.Timezone,
		EndsAt:                    rl.EndsAt,
		Status:                    rl.Status,
		PauseReason:               rl.PauseReason,
		ConsecutiveFailures:       rl.ConsecutiveFailures,
		NextScheduledAt:           rl.NextScheduledAt,
		LastLowBalanceTriggeredAt: rl.LastLowBalanceTriggered,
		LastReadingAt:             rl.LastReadingAt,
		UpcomingRun:               upcoming,
		RecentRuns:                runs,
		CreatedAt:                 rl.CreatedAt,
		UpdatedAt:                 rl.UpdatedAt,
	}
	if upcoming != nil {
		ts := upcoming.ScheduledFor
		v.NextScheduledAt = &ts
	}
	if rl.LowBalanceMilliKWh > 0 {
		kwh := autopower.FloatFromMilliKWh(rl.LowBalanceMilliKWh)
		v.LowBalanceThresholdKWh = &kwh
	}
	if rl.LastReadingMilliKWh != nil {
		kwh := autopower.FloatFromMilliKWh(*rl.LastReadingMilliKWh)
		v.LastReadingKWh = &kwh
	}
	return v, nil
}

// ensureNextScheduledRunInTx guarantees that if rl is ACTIVE and schedule-based,
// at least one future SCHEDULED run is materialised (advancing past any slots
// older than the 6-hour catch-up window as MISSED).
func (o *Orchestrator) ensureNextScheduledRunInTx(ctx context.Context, r store.Repos, rl *autopower.Rule, afterSlot, now time.Time) error {
	if rl.Status != autopower.RuleActive || !rl.TriggerType.UsesSchedule() {
		return nil
	}
	runs, err := r.AutoPower().ListRunsForRule(ctx, rl.ID, 5)
	if err != nil {
		return err
	}
	for _, rn := range runs {
		if rn.Status == autopower.RunScheduled {
			return nil
		}
	}

	loc, err := autopower.ResolveLocation(rl.Timezone)
	if err != nil {
		loc = autopower.LagosLocation()
	}
	spec := autopower.ScheduleSpecFromRule(rl)
	cursor := afterSlot
	if cursor.IsZero() {
		cursor = now
	}
	slot, ok := autopower.NextSlotAfter(cursor, spec, loc)
	if !ok {
		rl.NextScheduledAt = nil
		rl.UpdatedAt = now
		return r.AutoPower().UpdateRule(ctx, rl)
	}

	missedCutoff := now.Add(-autopower.CatchUpWindow)
	for slot.Before(missedCutoff) {
		missedRun := &autopower.Run{
			ID:            uuid.New(),
			RuleID:        rl.ID,
			UserID:        rl.UserID,
			MeterID:       rl.MeterID,
			TriggerSource: autopower.SourceSchedule,
			ScheduledFor:  slot,
			AmountMinor:   rl.AmountMinor,
			TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
			Currency:      rl.Currency,
			Status:        autopower.RunMissed,
			FailureReason: "MISSED_WINDOW",
			CompletedAt:   &now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := r.AutoPower().CreateRun(ctx, missedRun); err == nil {
			ev := events.NewForAggregate("autopower_run", "autopower.run.missed", missedRun.ID, map[string]any{
				"rule_id":       rl.ID.String(),
				"scheduled_for": slot.Format(time.RFC3339),
			})
			ev.CreatedAt = now
			if err := r.Outbox().Add(ctx, ev); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrDuplicate) {
			return err
		}
		next, nOK := autopower.NextSlotAfter(slot, spec, loc)
		if !nOK {
			rl.NextScheduledAt = nil
			rl.UpdatedAt = now
			return r.AutoPower().UpdateRule(ctx, rl)
		}
		slot = next
	}

	nextRun := &autopower.Run{
		ID:            uuid.New(),
		RuleID:        rl.ID,
		UserID:        rl.UserID,
		MeterID:       rl.MeterID,
		TriggerSource: autopower.SourceSchedule,
		ScheduledFor:  slot,
		AmountMinor:   rl.AmountMinor,
		TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
		Currency:      rl.Currency,
		Status:        autopower.RunScheduled,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := r.AutoPower().CreateRun(ctx, nextRun); err != nil && !errors.Is(err, store.ErrDuplicate) {
		return err
	}
	if following, fOK := autopower.NextSlotAfter(slot, spec, loc); fOK {
		rl.NextScheduledAt = &following
	} else {
		rl.NextScheduledAt = nil
	}
	rl.UpdatedAt = now
	return r.AutoPower().UpdateRule(ctx, rl)
}

// ---- Scheduler, spend guard, ChargeSaved & reconciliation -------------------

// TickAutoPower executes one scheduler pass: materialising runs entering the
// 12-hour heads-up horizon, dispatching 12-hour heads-ups, marking >6h overdue
// runs MISSED, executing due scheduled runs under spend-guard locks, and
// reconciling stale CHARGING runs via VerifyPayment without double-charging.
func (o *Orchestrator) TickAutoPower(ctx context.Context) (int, error) {
	now := o.Now().UTC()
	lease := o.cfg.ResumeLease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	staleAfter := o.cfg.ResumeStaleAfter
	if staleAfter <= 0 {
		staleAfter = 2 * time.Minute
	}
	batch := o.cfg.ResumeBatch
	if batch <= 0 {
		batch = 50
	}
	processed := 0

	// 1. Materialise any next-cycle slots within the 12h heads-up window.
	horizon := now.Add(autopower.HeadsUpLead)
	dueRuleIDs, err := o.st.Repos().AutoPower().ClaimDueRulesForMaterialize(ctx, horizon, now, lease, batch)
	if err != nil {
		return processed, err
	}
	for _, ruleID := range dueRuleIDs {
		if err := o.materializeClaimedRule(ctx, ruleID); err != nil {
			o.log.Warn("autopower materialize failed", "rule", ruleID, "err", err)
			continue
		}
		processed++
	}

	// 2. Claim & process due runs (12h heads-up, >6h MISSED, due charge, or stale CHARGING).
	dueRunIDs, err := o.st.Repos().AutoPower().ClaimDueRuns(ctx, horizon, now, now.Add(-staleAfter), lease, batch)
	if err != nil {
		return processed, err
	}
	for _, runID := range dueRunIDs {
		if err := o.processClaimedRun(ctx, runID); err != nil {
			if !errors.Is(err, ErrProviderDisabled) {
				o.log.Warn("autopower run processing failed", "run", runID, "err", err)
			}
			continue
		}
		processed++
	}

	return processed, nil
}

func (o *Orchestrator) materializeClaimedRule(ctx context.Context, ruleID uuid.UUID) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		rl, err := r.AutoPower().GetRuleForUpdate(ctx, ruleID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		rl.LeaseUntil = time.Time{}
		if rl.Status != autopower.RuleActive || !rl.TriggerType.UsesSchedule() || rl.NextScheduledAt == nil {
			rl.UpdatedAt = now
			return r.AutoPower().UpdateRule(ctx, rl)
		}

		loc, lErr := autopower.ResolveLocation(rl.Timezone)
		if lErr != nil {
			loc = autopower.LagosLocation()
		}
		slot := *rl.NextScheduledAt
		spec := autopower.ScheduleSpecFromRule(rl)
		missedCutoff := now.Add(-autopower.CatchUpWindow)
		for slot.Before(missedCutoff) {
			missedRun := &autopower.Run{
				ID:            uuid.New(),
				RuleID:        rl.ID,
				UserID:        rl.UserID,
				MeterID:       rl.MeterID,
				TriggerSource: autopower.SourceSchedule,
				ScheduledFor:  slot,
				AmountMinor:   rl.AmountMinor,
				TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
				Currency:      rl.Currency,
				Status:        autopower.RunMissed,
				FailureReason: "MISSED_WINDOW",
				CompletedAt:   &now,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := r.AutoPower().CreateRun(ctx, missedRun); err == nil {
				ev := events.NewForAggregate("autopower_run", "autopower.run.missed", missedRun.ID, map[string]any{
					"rule_id":       rl.ID.String(),
					"scheduled_for": slot.Format(time.RFC3339),
				})
				ev.CreatedAt = now
				if err := r.Outbox().Add(ctx, ev); err != nil {
					return err
				}
			} else if !errors.Is(err, store.ErrDuplicate) {
				return err
			}
			next, ok := autopower.NextSlotAfter(slot, spec, loc)
			if !ok {
				rl.NextScheduledAt = nil
				rl.UpdatedAt = now
				return r.AutoPower().UpdateRule(ctx, rl)
			}
			slot = next
		}

		if !slot.After(now.Add(autopower.HeadsUpLead)) {
			rn := &autopower.Run{
				ID:            uuid.New(),
				RuleID:        rl.ID,
				UserID:        rl.UserID,
				MeterID:       rl.MeterID,
				TriggerSource: autopower.SourceSchedule,
				ScheduledFor:  slot,
				AmountMinor:   rl.AmountMinor,
				TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
				Currency:      rl.Currency,
				Status:        autopower.RunScheduled,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := r.AutoPower().CreateRun(ctx, rn); err != nil && !errors.Is(err, store.ErrDuplicate) {
				return err
			}
			if next, ok := autopower.NextSlotAfter(slot, spec, loc); ok {
				rl.NextScheduledAt = &next
			} else {
				rl.NextScheduledAt = nil
			}
		} else {
			rl.NextScheduledAt = &slot
		}
		rl.UpdatedAt = now
		return r.AutoPower().UpdateRule(ctx, rl)
	})
}

func (o *Orchestrator) processClaimedRun(ctx context.Context, runID uuid.UUID) error {
	now := o.Now().UTC()
	rn0, err := o.st.Repos().AutoPower().GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if rn0.Status == autopower.RunCharging {
		return o.reconcileStaleChargingRun(ctx, rn0)
	}
	if rn0.Status != autopower.RunScheduled {
		return nil
	}

	// Case A: future run inside [scheduled_for - 12h, scheduled_for) -> send 12h heads-up.
	if now.Before(rn0.ScheduledFor) {
		return o.st.InTx(ctx, func(r store.Repos) error {
			rl, err := r.AutoPower().GetRuleForUpdate(ctx, rn0.RuleID)
			if err != nil {
				return err
			}
			rn, err := r.AutoPower().GetRunForUpdate(ctx, runID)
			if err != nil {
				return err
			}
			if rn.Status != autopower.RunScheduled {
				return nil
			}
			rn.LeaseUntil = time.Time{}
			if rl.Status != autopower.RuleActive {
				rn.Status = autopower.RunCancelled
				rn.CompletedAt = &now
				rn.UpdatedAt = now
				return r.AutoPower().UpdateRun(ctx, rn)
			}
			if rn.HeadsUpSentAt == nil && !rn.ScheduledFor.After(now.Add(autopower.HeadsUpLead)) {
				rn.HeadsUpSentAt = &now
				rn.UpdatedAt = now
				if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
					return err
				}
				ev := events.NewForAggregate("autopower_run", "autopower.run.heads_up", rn.ID, map[string]any{
					"rule_id":       rl.ID.String(),
					"scheduled_for": rn.ScheduledFor.Format(time.RFC3339),
					"amount_minor":  rn.AmountMinor,
				})
				ev.ID = rn.ID
				ev.CreatedAt = now
				return r.Outbox().Add(ctx, ev)
			}
			return r.AutoPower().UpdateRun(ctx, rn)
		})
	}

	// Case B: scheduled slot overdue beyond the 6-hour catch-up window -> MISSED.
	if now.Sub(rn0.ScheduledFor) > autopower.CatchUpWindow {
		return o.st.InTx(ctx, func(r store.Repos) error {
			rl, err := r.AutoPower().GetRuleForUpdate(ctx, rn0.RuleID)
			if err != nil {
				return err
			}
			rn, err := r.AutoPower().GetRunForUpdate(ctx, runID)
			if err != nil {
				return err
			}
			if rn.Status != autopower.RunScheduled {
				return nil
			}
			rn.Status = autopower.RunMissed
			rn.FailureReason = "MISSED_WINDOW"
			rn.CompletedAt = &now
			rn.LeaseUntil = time.Time{}
			rn.UpdatedAt = now
			if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
				return err
			}
			ev := events.NewForAggregate("autopower_run", "autopower.run.missed", rn.ID, map[string]any{
				"rule_id":       rl.ID.String(),
				"scheduled_for": rn.ScheduledFor.Format(time.RFC3339),
			})
			ev.CreatedAt = now
			if err := r.Outbox().Add(ctx, ev); err != nil {
				return err
			}
			return o.ensureNextScheduledRunInTx(ctx, r, rl, rn.ScheduledFor, now)
		})
	}

	// Case C: due within [scheduled_for, scheduled_for + 6h] -> execute purchase.
	return o.executeRun(ctx, runID)
}

type preparedAutoPowerCharge struct {
	txnID       uuid.UUID
	chargeRef   string
	authCode    string
	chargeEmail string
	total       money.Amount
}

func (o *Orchestrator) executeRun(ctx context.Context, runID uuid.UUID) error {
	if err := o.requireEnabled(ctx, o.payments.Name()); err != nil {
		return err
	}
	if err := o.requireEnabled(ctx, o.vending.Name()); err != nil {
		return err
	}

	rn0, err := o.st.Repos().AutoPower().GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rl0, err := o.st.Repos().AutoPower().GetRule(ctx, rn0.RuleID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	now := o.Now().UTC()
	loc := autopower.LagosLocation()
	lease := o.cfg.ResumeLease
	if lease <= 0 {
		lease = 2 * time.Minute
	}

	var prep *preparedAutoPowerCharge
	err = o.st.InTx(ctx, func(r store.Repos) error {
		// Canonical lock order: User -> Meter -> PaymentMethod -> Rule -> Run.
		if err := r.Users().LockForUpdate(ctx, rl0.UserID); err != nil {
			return err
		}
		m, mErr := r.Meters().GetForUpdate(ctx, rl0.MeterID)
		if mErr != nil && !errors.Is(mErr, store.ErrNotFound) {
			return mErr
		}

		curRule, err := r.AutoPower().GetRule(ctx, rl0.ID)
		if err != nil {
			return err
		}
		pm, pmErr := r.AutoPower().GetPaymentMethodForUpdate(ctx, curRule.PaymentMethodID)
		if pmErr != nil && !errors.Is(pmErr, store.ErrNotFound) {
			return pmErr
		}

		rl, err := r.AutoPower().GetRuleForUpdate(ctx, rl0.ID)
		if err != nil {
			return err
		}
		rn, err := r.AutoPower().GetRunForUpdate(ctx, runID)
		if err != nil {
			return err
		}
		if rn.Status != autopower.RunScheduled {
			return nil
		}
		if rl.Status != autopower.RuleActive {
			rn.Status = autopower.RunCancelled
			rn.CompletedAt = &now
			rn.LeaseUntil = time.Time{}
			rn.UpdatedAt = now
			return r.AutoPower().UpdateRun(ctx, rn)
		}

		// Verify meter existence and active PURCHASE grant.
		meterRevoked := errors.Is(mErr, store.ErrNotFound) || m == nil
		if !meterRevoked {
			gr, gErr := r.Meters().ActiveGrant(ctx, rl.MeterID, rl.UserID)
			if errors.Is(gErr, store.ErrNotFound) || (gErr == nil && !gr.Permission.Allows(meters.PermPurchase)) {
				meterRevoked = true
			} else if gErr != nil {
				return gErr
			}
		}
		if meterRevoked {
			return o.failRunAndPauseRuleInTx(ctx, r, rl, rn, autopower.PauseMeterAccessRevoked, autopower.PauseMeterAccessRevoked, now)
		}

		// Verify saved payment method is still active and un-expired.
		if errors.Is(pmErr, store.ErrNotFound) || pm == nil || pm.DeletedAt != nil || pm.UserID != rl.UserID {
			return o.failRunAndPauseRuleInTx(ctx, r, rl, rn, autopower.PausePaymentMethodDeleted, autopower.PausePaymentMethodDeleted, now)
		}
		if pm.IsExpiredAt(now, loc) {
			return o.failRunAndPauseRuleInTx(ctx, r, rl, rn, autopower.PauseCardExpired, autopower.PauseCardExpired, now)
		}

		// Spend guard: check Africa/Lagos monthly rule cap AND per-user daily cap.
		mStart, mEnd := autopower.MonthBoundsUTC(now, loc)
		monthSpent, err := r.AutoPower().SumRuleSpendMinor(ctx, rl.ID, mStart, mEnd)
		if err != nil {
			return err
		}
		dStart, dEnd := autopower.DayBoundsUTC(now, loc)
		daySpent, err := r.AutoPower().SumUserSpendMinor(ctx, rl.UserID, dStart, dEnd)
		if err != nil {
			return err
		}
		monthOver := monthSpent+rn.AmountMinor > rl.MonthlyCapMinor
		dayOver := o.cfg.AutoPowerDailyMaxMinor > 0 && daySpent+rn.AmountMinor > o.cfg.AutoPowerDailyMaxMinor
		if monthOver || dayOver {
			reason := "MONTHLY_CAP_EXCEEDED"
			if dayOver && !monthOver {
				reason = "DAILY_CAP_EXCEEDED"
			}
			rn.Status = autopower.RunSkippedCap
			rn.FailureReason = reason
			rn.CompletedAt = &now
			rn.LeaseUntil = time.Time{}
			rn.UpdatedAt = now
			if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
				return err
			}
			if rn.TriggerSource == autopower.SourceSchedule {
				if err := o.ensureNextScheduledRunInTx(ctx, r, rl, rn.ScheduledFor, now); err != nil {
					return err
				}
			}
			ev := events.NewForAggregate("autopower_run", "autopower.run.skipped_cap", rn.ID, map[string]any{
				"rule_id":      rl.ID.String(),
				"reason":       reason,
				"amount_minor": rn.AmountMinor,
			})
			ev.CreatedAt = now
			if err := r.Outbox().Add(ctx, ev); err != nil {
				return err
			}
			actor := audit.Actor{UserID: rl.UserID, Type: audit.ActorSystem}
			return r.Audit().Add(ctx, actor.Event("autopower.run.skipped_cap", "autopower_run", rn.ID, map[string]any{
				"rule_id": rl.ID.String(),
				"reason":  reason,
			}))
		}

		// Resolve current delivery method & current fee ("tariff/quote change between runs").
		remoteOK := o.remote != nil && m.Capabilities.RemoteCreditUsable()
		method := tx.MethodToken
		switch DeliveryPreference(rl.DeliveryPreference) {
		case PrefRemote:
			if !remoteOK {
				return o.recordRunFailureInTx(ctx, r, rl, rn, "REMOTE_DELIVERY_INELIGIBLE", now)
			}
			method = tx.MethodRemote
		case PrefAuto, "":
			if remoteOK {
				method = tx.MethodRemote
			}
		case PrefToken:
			method = tx.MethodToken
		}

		rawAuth, err := autopower.OpenAuthCode(o.box, rl.UserID, pm.ID, pm.AuthCodeCiphertext)
		if err != nil {
			return o.failRunAndPauseRuleInTx(ctx, r, rl, rn, "PAYMENT_AUTH_DECRYPT_FAILED", "PAYMENT_AUTH_INVALID", now)
		}
		rawEmail, err := autopower.OpenAuthEmail(o.box, rl.UserID, pm.ID, pm.EmailCiphertext)
		if err != nil {
			return o.failRunAndPauseRuleInTx(ctx, r, rl, rn, "PAYMENT_EMAIL_DECRYPT_FAILED", "PAYMENT_AUTH_INVALID", now)
		}

		idemKey := autoPowerIdemPrefix + rn.ID.String()
		chargeRef := autoPowerChargeRefPrefix + rn.ID.String()
		amt := money.New(rn.AmountMinor, money.NGN)
		fee := money.New(o.cfg.FeeMinor, money.NGN)
		total, _ := amt.Add(fee)

		options := []tx.DeliveryMethod{tx.MethodToken}
		if remoteOK {
			options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
		}
		q := &tx.Quote{
			ID:              uuid.New(),
			CustomerID:      rl.UserID,
			MeterID:         rl.MeterID,
			Amount:          amt,
			Fee:             fee,
			Total:           total,
			DeliveryMethod:  method,
			DeliveryOptions: options,
			Used:            true,
			CreatedAt:       now,
			ExpiresAt:       now.Add(o.cfg.QuoteTTL),
		}
		if err := r.Quotes().Create(ctx, q); err != nil {
			return err
		}

		reqHash := o.box.Hash(fmt.Sprintf("%s|%s", rl.ID, rn.ID))
		if existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
			Scope: rl.UserID, Operation: opCreateTransaction, Key: idemKey,
			RequestHash: reqHash, CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL),
		}); err != nil {
			return err
		} else if existing != nil && existing.ResultRef != "" {
			if existingTxnID, pErr := uuid.Parse(existing.ResultRef); pErr == nil {
				rn.Status = autopower.RunCharging
				rn.ExecutedAt = &now
				rn.TotalMinor = total.Minor
				rn.TransactionID = &existingTxnID
				rn.PaymentProviderRef = chargeRef
				rn.ChargeDispatchedAt = &now
				rn.LeaseUntil = now.Add(lease)
				rn.UpdatedAt = now
				return r.AutoPower().UpdateRun(ctx, rn)
			}
		}

		elecTxn := tx.New(rl.UserID, q, false, "", now)
		elecTxn.PaymentProviderName = o.payments.Name()
		elecTxn.PaymentProviderRef = chargeRef
		if err := elecTxn.SetPayment(tx.PayPending, "autopower saved card charge via "+o.payments.Name()); err != nil {
			return err
		}
		if err := r.Transactions().Create(ctx, elecTxn); err != nil {
			return err
		}
		if err := r.Idempotency().Complete(ctx, rl.UserID, opCreateTransaction, idemKey, elecTxn.ID.String()); err != nil {
			return err
		}
		if err := emit(ctx, r, "electricity.transaction.created", elecTxn.ID, map[string]any{
			"gift": false, "source": "autopower",
		}); err != nil {
			return err
		}
		if err := emit(ctx, r, "payment.pending", elecTxn.ID, map[string]any{
			"provider": o.payments.Name(), "provider_ref": chargeRef,
		}); err != nil {
			return err
		}

		rn.Status = autopower.RunCharging
		rn.ExecutedAt = &now
		rn.TotalMinor = total.Minor
		rn.TransactionID = &elecTxn.ID
		rn.PaymentProviderRef = chargeRef
		rn.ChargeDispatchedAt = &now
		rn.LeaseUntil = now.Add(lease)
		rn.UpdatedAt = now
		if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
			return err
		}
		if rn.TriggerSource == autopower.SourceSchedule {
			if err := o.ensureNextScheduledRunInTx(ctx, r, rl, rn.ScheduledFor, now); err != nil {
				return err
			}
		}

		prep = &preparedAutoPowerCharge{
			txnID:       elecTxn.ID,
			chargeRef:   chargeRef,
			authCode:    rawAuth,
			chargeEmail: rawEmail,
			total:       total,
		}
		return nil
	})
	if err != nil || prep == nil {
		return err
	}

	// Outbound PSP charge outside DB transaction. PaymentProviderRef is already
	// persisted on both electricity_transactions and autopower_runs so a crash or
	// lost response can NEVER cause a second ChargeSaved call for this run.
	pctx, cancel := o.pctx(ctx)
	_, chargeErr := o.payments.ChargeSaved(pctx, providers.ChargeRequest{
		AuthorizationRef: prep.authCode,
		Email:            prep.chargeEmail,
		Amount:           prep.total,
		Reference:        prep.chargeRef,
	})
	cancel()

	if chargeErr != nil {
		// Probe VerifyPayment immediately: if the PSP actually processed the
		// charge before dropping the response (FailAfterApply), VerifyPayment
		// settles it cleanly via OnPaymentEvent without double-charging.
		vctx, vcancel := o.pctx(ctx)
		_, vErr := o.payments.VerifyPayment(vctx, prep.chargeRef)
		vcancel()
		if vErr == nil {
			_, _ = o.HandlePaymentNotification(ctx, prep.chargeRef)
			o.trySyncAutoPowerTxn(ctx, prep.txnID)
			return nil
		}
		if !providers.IsAmbiguous(chargeErr) && errors.Is(vErr, providers.ErrPaymentNotFound) {
			// Definitive synchronous rejection before PSP recorded a transaction.
			_, _ = o.mutate(ctx, prep.txnID, func(t *tx.Transaction, r store.Repos) error {
				if t.Payment != tx.PayPending || t.ReviewReason != "" {
					return errSkip
				}
				if err := t.SetPayment(tx.PayFailed, "saved card charge rejected by provider"); err != nil {
					return err
				}
				return emit(ctx, r, "payment.failed", t.ID, map[string]any{"reason": "charge_authorization_rejected"})
			})
			o.trySyncAutoPowerTxn(ctx, prep.txnID)
			return nil
		}
		o.log.Warn("autopower ChargeSaved outcome ambiguous; awaiting VerifyPayment reconciliation",
			"run", runID, "txn", prep.txnID, "err", chargeErr)
		return nil
	}

	// Invariant 1: never trust ChargeSaved response alone; verify & advance via HandlePaymentNotification.
	_, _ = o.HandlePaymentNotification(ctx, prep.chargeRef)
	o.trySyncAutoPowerTxn(ctx, prep.txnID)
	return nil
}

func (o *Orchestrator) reconcileStaleChargingRun(ctx context.Context, rn *autopower.Run) error {
	if rn.TransactionID == nil {
		return nil
	}
	if rn.PaymentProviderRef != "" {
		if _, err := o.HandlePaymentNotification(ctx, rn.PaymentProviderRef); err != nil && !errors.Is(err, ErrProviderDisabled) {
			o.log.Warn("autopower reconcile HandlePaymentNotification failed", "run", rn.ID, "err", err)
		}
	}
	o.trySyncAutoPowerTxn(ctx, *rn.TransactionID)
	return nil
}

// trySyncAutoPowerTxn reconciles any autopower_run linked to txnID whenever the
// underlying electricity transaction settles (VendConfirmed -> SUCCEEDED,
// PayFailed / PayRefunded / VendFailed -> FAILED / REFUNDED + 3-strike pause).
func (o *Orchestrator) trySyncAutoPowerTxn(ctx context.Context, txnID uuid.UUID) {
	rn0, err := o.st.Repos().AutoPower().GetRunByTxn(ctx, txnID)
	if err != nil {
		return
	}
	now := o.Now().UTC()

	if err := o.st.InTx(ctx, func(r store.Repos) error {
		rl, err := r.AutoPower().GetRuleForUpdate(ctx, rn0.RuleID)
		if err != nil {
			return err
		}
		rn, err := r.AutoPower().GetRunForUpdate(ctx, rn0.ID)
		if err != nil {
			return err
		}
		t, err := r.Transactions().Get(ctx, txnID)
		if err != nil {
			return err
		}

		switch {
		case t.Vending == tx.VendConfirmed:
			if rn.Status == autopower.RunSucceeded {
				return nil
			}
			rn.Status = autopower.RunSucceeded
			rn.FailureReason = ""
			rn.CompletedAt = &now
			rn.LeaseUntil = time.Time{}
			rn.UpdatedAt = now
			if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
				return err
			}
			if rl.ConsecutiveFailures != 0 {
				rl.ConsecutiveFailures = 0
				rl.UpdatedAt = now
				return r.AutoPower().UpdateRule(ctx, rl)
			}
			return nil

		case t.Payment == tx.PayFailed:
			if rn.Status != autopower.RunCharging && rn.Status != autopower.RunScheduled {
				return nil
			}
			return o.recordRunFailureInTx(ctx, r, rl, rn, "PAYMENT_FAILED", now)

		case t.Payment == tx.PayRefunded:
			if rn.Status == autopower.RunRefunded {
				return nil
			}
			if rn.Status == autopower.RunFailed {
				rn.Status = autopower.RunRefunded
				rn.LeaseUntil = time.Time{}
				rn.UpdatedAt = now
				return r.AutoPower().UpdateRun(ctx, rn)
			}
			if rn.Status == autopower.RunCharging || rn.Status == autopower.RunScheduled {
				if err := o.recordRunFailureInTx(ctx, r, rl, rn, "VENDING_FAILED_REFUNDED", now); err != nil {
					return err
				}
				rn.Status = autopower.RunRefunded
				return r.AutoPower().UpdateRun(ctx, rn)
			}
			return nil

		case t.Payment == tx.PayRefundPending && t.Vending == tx.VendFailed:
			if rn.Status != autopower.RunCharging && rn.Status != autopower.RunScheduled {
				return nil
			}
			return o.recordRunFailureInTx(ctx, r, rl, rn, "VENDING_FAILED_REFUND_PENDING", now)
		}
		return nil
	}); err != nil {
		o.log.Warn("autopower sync run failed", "txn", txnID, "err", err)
	}
}

func (o *Orchestrator) recordRunFailureInTx(ctx context.Context, r store.Repos, rl *autopower.Rule, rn *autopower.Run, reason string, now time.Time) error {
	rn.Status = autopower.RunFailed
	rn.FailureReason = reason
	rn.CompletedAt = &now
	rn.LeaseUntil = time.Time{}
	rn.UpdatedAt = now
	if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
		return err
	}
	if rn.TriggerSource == autopower.SourceSchedule && rl.Status == autopower.RuleActive {
		if err := o.ensureNextScheduledRunInTx(ctx, r, rl, rn.ScheduledFor, now); err != nil {
			return err
		}
	}

	failEv := events.NewForAggregate("autopower_run", "autopower.run.failed", rn.ID, map[string]any{
		"rule_id": rl.ID.String(),
		"reason":  reason,
	})
	failEv.CreatedAt = now
	if err := r.Outbox().Add(ctx, failEv); err != nil {
		return err
	}

	rl.ConsecutiveFailures++
	rl.UpdatedAt = now
	actor := audit.Actor{UserID: rl.UserID, Type: audit.ActorSystem}

	if rl.ConsecutiveFailures >= autopower.MaxConsecutiveFailures && rl.Status == autopower.RuleActive {
		rl.Status = autopower.RulePaused
		rl.PauseReason = autopower.PauseConsecutiveFailures
		rl.NextScheduledAt = nil
		rl.NextPollAt = nil
		if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
			return err
		}
		if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
			return err
		}
		pauseEv := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{
			"reason":               autopower.PauseConsecutiveFailures,
			"consecutive_failures": rl.ConsecutiveFailures,
		})
		pauseEv.CreatedAt = now
		if err := r.Outbox().Add(ctx, pauseEv); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event("autopower.rule.paused", "autopower_rule", rl.ID, map[string]any{
			"reason":               autopower.PauseConsecutiveFailures,
			"consecutive_failures": rl.ConsecutiveFailures,
		}))
	}
	return r.AutoPower().UpdateRule(ctx, rl)
}

func (o *Orchestrator) failRunAndPauseRuleInTx(ctx context.Context, r store.Repos, rl *autopower.Rule, rn *autopower.Run, runReason, pauseReason string, now time.Time) error {
	rn.Status = autopower.RunFailed
	rn.FailureReason = runReason
	rn.CompletedAt = &now
	rn.LeaseUntil = time.Time{}
	rn.UpdatedAt = now
	if err := r.AutoPower().UpdateRun(ctx, rn); err != nil {
		return err
	}

	rl.ConsecutiveFailures++
	wasActive := rl.Status == autopower.RuleActive
	rl.Status = autopower.RulePaused
	rl.PauseReason = pauseReason
	rl.NextScheduledAt = nil
	rl.NextPollAt = nil
	rl.LeaseUntil = time.Time{}
	rl.UpdatedAt = now
	if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
		return err
	}
	if err := r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now); err != nil {
		return err
	}

	failEv := events.NewForAggregate("autopower_run", "autopower.run.failed", rn.ID, map[string]any{
		"rule_id": rl.ID.String(),
		"reason":  runReason,
	})
	failEv.CreatedAt = now
	if err := r.Outbox().Add(ctx, failEv); err != nil {
		return err
	}
	if wasActive {
		pauseEv := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{
			"reason": pauseReason,
		})
		pauseEv.CreatedAt = now
		if err := r.Outbox().Add(ctx, pauseEv); err != nil {
			return err
		}
	}
	actor := audit.Actor{UserID: rl.UserID, Type: audit.ActorSystem}
	return r.Audit().Add(ctx, actor.Event("autopower.rule.paused", "autopower_rule", rl.ID, map[string]any{
		"reason": pauseReason,
	}))
}

// ---- Low-balance polling ---------------------------------------------------

// PollLowBalance polls remote meter readings for active LOW_BALANCE / BOTH
// AutoPower rules and triggers at most one recharge per 24 hours per rule when
// the observed balance drops at or below LowBalanceMilliKWh.
func (o *Orchestrator) PollLowBalance(ctx context.Context) (int, error) {
	if o.readings == nil {
		return 0, nil
	}
	if err := o.requireEnabled(ctx, o.readings.Name()); err != nil {
		return 0, nil
	}

	now := o.Now().UTC()
	lease := o.cfg.ResumeLease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	batch := o.cfg.ResumeBatch
	if batch <= 0 {
		batch = 50
	}
	pollInterval := o.cfg.AutoPowerPollInterval
	if pollInterval <= 0 {
		pollInterval = 4 * time.Hour
	}

	dueIDs, err := o.st.Repos().AutoPower().ClaimDueLowBalanceRules(ctx, now, lease, batch)
	if err != nil {
		return 0, err
	}

	polled := 0
	for _, ruleID := range dueIDs {
		triggeredRunID, err := o.pollSingleRule(ctx, ruleID, pollInterval)
		if err != nil {
			o.log.Warn("autopower low-balance poll failed", "rule", ruleID, "err", err)
			continue
		}
		polled++
		if triggeredRunID != uuid.Nil {
			if err := o.executeRun(ctx, triggeredRunID); err != nil && !errors.Is(err, ErrProviderDisabled) {
				o.log.Warn("autopower low-balance run execution failed", "rule", ruleID, "run", triggeredRunID, "err", err)
			}
		}
	}
	return polled, nil
}

func (o *Orchestrator) pollSingleRule(ctx context.Context, ruleID uuid.UUID, pollInterval time.Duration) (uuid.UUID, error) {
	now := o.Now().UTC()
	rl0, err := o.st.Repos().AutoPower().GetRule(ctx, ruleID)
	if errors.Is(err, store.ErrNotFound) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	if rl0.Status != autopower.RuleActive || !rl0.TriggerType.UsesLowBalance() {
		return uuid.Nil, nil
	}

	m, err := o.Meters.Authorize(ctx, rl0.UserID, rl0.MeterID, meters.PermPurchase)
	if errors.Is(err, meters.ErrForbidden) || errors.Is(err, meters.ErrNotFound) {
		_ = o.st.InTx(ctx, func(r store.Repos) error {
			rl, gErr := r.AutoPower().GetRuleForUpdate(ctx, ruleID)
			if gErr != nil || rl.Status != autopower.RuleActive {
				return gErr
			}
			rl.Status = autopower.RulePaused
			rl.PauseReason = autopower.PauseMeterAccessRevoked
			rl.NextScheduledAt = nil
			rl.NextPollAt = nil
			rl.LeaseUntil = time.Time{}
			rl.UpdatedAt = now
			if err := r.AutoPower().UpdateRule(ctx, rl); err != nil {
				return err
			}
			_ = r.AutoPower().CancelScheduledRunsForRule(ctx, rl.ID, now)
			ev := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{"reason": autopower.PauseMeterAccessRevoked})
			ev.CreatedAt = now
			return r.Outbox().Add(ctx, ev)
		})
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !m.Capabilities.RemoteReadingConfirmed() {
		_ = o.st.InTx(ctx, func(r store.Repos) error {
			rl, gErr := r.AutoPower().GetRuleForUpdate(ctx, ruleID)
			if gErr != nil || rl.Status != autopower.RuleActive {
				return gErr
			}
			rl.LeaseUntil = time.Time{}
			rl.NextPollAt = nil
			rl.UpdatedAt = now
			if rl.TriggerType == autopower.TriggerLowBalance {
				rl.Status = autopower.RulePaused
				rl.PauseReason = autopower.PauseIneligibleReading
				ev := events.NewForAggregate("autopower_rule", "autopower.rule.paused", rl.ID, map[string]any{"reason": autopower.PauseIneligibleReading})
				ev.CreatedAt = now
				if err := r.Outbox().Add(ctx, ev); err != nil {
					return err
				}
			}
			return r.AutoPower().UpdateRule(ctx, rl)
		})
		return uuid.Nil, nil
	}

	number, err := o.Meters.Number(m)
	if err != nil {
		return uuid.Nil, err
	}

	pctx, cancel := o.pctx(ctx)
	balKWh, readAt, readErr := o.readings.LatestReading(pctx, number)
	cancel()

	var triggeredRunID uuid.UUID
	err = o.st.InTx(ctx, func(r store.Repos) error {
		rl, err := r.AutoPower().GetRuleForUpdate(ctx, ruleID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		rl.LeaseUntil = time.Time{}
		nextPoll := now.Add(pollInterval)
		rl.NextPollAt = &nextPoll
		rl.UpdatedAt = now

		if rl.Status != autopower.RuleActive || !rl.TriggerType.UsesLowBalance() {
			rl.NextPollAt = nil
			return r.AutoPower().UpdateRule(ctx, rl)
		}
		if readErr != nil {
			return r.AutoPower().UpdateRule(ctx, rl)
		}

		milliKWh := int64(math.Round(balKWh * 1000))
		obsAt := readAt.UTC()
		if obsAt.IsZero() {
			obsAt = now
		}
		rl.LastReadingMilliKWh = &milliKWh
		rl.LastReadingAt = &obsAt
		_ = r.Readings().InsertSample(ctx, meters.ReadingSample{
			MeterID: rl.MeterID, MilliKWh: milliKWh, ReadAt: obsAt, Provider: o.readings.Name(),
		})

		staleReading := now.Sub(obsAt) > autopower.LowBalanceMaxReadingAge
		cooldownOK := rl.LastLowBalanceTriggered == nil || !now.Before(rl.LastLowBalanceTriggered.Add(autopower.LowBalanceCooldown))
		if milliKWh <= rl.LowBalanceMilliKWh && cooldownOK && !staleReading {
			recent, err := r.AutoPower().ListRunsForRule(ctx, rl.ID, 5)
			if err != nil {
				return err
			}
			inFlight := false
			for _, ex := range recent {
				if ex.Status == autopower.RunCharging {
					inFlight = true
					break
				}
			}
			if !inFlight {
				run := &autopower.Run{
					ID:            uuid.New(),
					RuleID:        rl.ID,
					UserID:        rl.UserID,
					MeterID:       rl.MeterID,
					TriggerSource: autopower.SourceLowBalance,
					ScheduledFor:  now,
					AmountMinor:   rl.AmountMinor,
					TotalMinor:    rl.AmountMinor + o.cfg.FeeMinor,
					Currency:      rl.Currency,
					Status:        autopower.RunScheduled,
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if err := r.AutoPower().CreateRun(ctx, run); err != nil {
					if !errors.Is(err, store.ErrDuplicate) {
						return err
					}
				} else {
					rl.LastLowBalanceTriggered = &now
					triggeredRunID = run.ID
				}
			}
		}
		return r.AutoPower().UpdateRule(ctx, rl)
	})
	return triggeredRunID, err
}
