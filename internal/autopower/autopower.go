// Package autopower holds the domain types, encryption bindings, validation
// rules and Africa/Lagos schedule calculator for M9 AutoPower (scheduled and
// capability-gated low-balance auto-recharge with spend guards).
package autopower

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

const (
	DefaultTimezone          = "Africa/Lagos"
	DefaultConsentVersion    = "v1"
	DefaultConsentTextV1     = "I authorise Meterly to save this card and charge scheduled or low-balance AutoPower electricity purchases up to my configured spend limits until I pause or cancel."
	HeadsUpLead              = 12 * time.Hour
	CatchUpWindow            = 6 * time.Hour
	LowBalanceCooldown       = 24 * time.Hour
	MaxConsecutiveFailures   = 3
	DefaultDailyMaxMinor     = 10_000_000 // ₦100,000 in kobo
	DefaultReadingPollEvery  = 4 * time.Hour
	DefaultSchedulerInterval = 30 * time.Second
)

var (
	ErrConsentRequired      = errors.New("autopower: explicit consent is required to save a payment method")
	ErrCardExpired          = errors.New("autopower: card is expired")
	ErrInvalidCard          = errors.New("autopower: invalid card metadata")
	ErrInvalidSchedule      = errors.New("autopower: invalid schedule configuration")
	ErrInvalidTrigger       = errors.New("autopower: invalid trigger configuration")
	ErrInvalidCap           = errors.New("autopower: monthly_cap_minor must be at least amount_minor")
	ErrLowBalanceThreshold  = errors.New("autopower: low_balance_threshold_kwh must be between 0.1 and 10000")
	ErrInvalidTimezone      = errors.New("autopower: unsupported timezone")
	ErrRuleNotEditable      = errors.New("autopower: cancelled rule cannot be modified")
	ErrRunNotSkippable      = errors.New("autopower: only a scheduled future run can be skipped")
	ErrPaymentMethodDeleted = errors.New("autopower: payment method has been deleted")
)

var lagosLoc = func() *time.Location {
	loc, err := time.LoadLocation(DefaultTimezone)
	if err != nil {
		// Embedded time/tzdata guarantees Africa/Lagos is present; fixed WAT
		// (UTC+1) is an unreachable defensive fallback.
		return time.FixedZone("WAT", 3600)
	}
	return loc
}()

// LagosLocation returns the canonical IANA Africa/Lagos location.
func LagosLocation() *time.Location { return lagosLoc }

// ---- Saved payment methods -------------------------------------------------

type SavedPaymentMethod struct {
	ID                  uuid.UUID  `json:"payment_method_id"`
	UserID              string     `json:"-"`
	Provider            string     `json:"provider"`
	SourceTransactionID uuid.UUID  `json:"source_transaction_id"`
	AuthCodeCiphertext  []byte     `json:"-"`
	EmailCiphertext     []byte     `json:"-"`
	KeyVersion          string     `json:"-"`
	CardBrand           string     `json:"card_brand"`
	Last4               string     `json:"last4"`
	ExpMonth            int        `json:"exp_month"`
	ExpYear             int        `json:"exp_year"`
	Signature           string     `json:"-"`
	ConsentVersion      string     `json:"consent_version"`
	ConsentText         string     `json:"consent_text"`
	ConsentedAt         time.Time  `json:"consented_at"`
	CreatedAt           time.Time  `json:"created_at"`
	DeletedAt           *time.Time `json:"deleted_at,omitempty"`
}

// NormalizeExpYear converts a 2-digit card expiry year (e.g. 30) to 4 digits (2030).
func NormalizeExpYear(y int) int {
	if y > 0 && y < 100 {
		return 2000 + y
	}
	return y
}

// ValidateCardExpiry checks 1 <= expMonth <= 12 and reasonable 4-digit year.
func ValidateCardExpiry(expMonth, expYear int) error {
	expYear = NormalizeExpYear(expYear)
	if expMonth < 1 || expMonth > 12 || expYear < 2000 || expYear > 2200 {
		return ErrInvalidCard
	}
	return nil
}

// IsExpiredAt reports whether the card has expired at instant now in loc.
// A card expiring in MM/YYYY is valid through the final instant of month MM in loc.
func (m *SavedPaymentMethod) IsExpiredAt(now time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = lagosLoc
	}
	y := NormalizeExpYear(m.ExpYear)
	if m.ExpMonth < 1 || m.ExpMonth > 12 || y < 2000 {
		return true
	}
	cutoff := time.Date(y, time.Month(m.ExpMonth)+1, 1, 0, 0, 0, 0, loc)
	return !now.In(loc).Before(cutoff)
}

func authCodeAAD(userID string, methodID uuid.UUID) []byte {
	return []byte("autopower-auth:" + userID + ":" + methodID.String())
}

func authEmailAAD(userID string, methodID uuid.UUID) []byte {
	return []byte("autopower-email:" + userID + ":" + methodID.String())
}

// SealAuthCode encrypts a reusable PSP authorization_code bound to userID and methodID.
func SealAuthCode(box *secure.Box, userID string, methodID uuid.UUID, authCode string) ([]byte, error) {
	if userID == "" || methodID == uuid.Nil || strings.TrimSpace(authCode) == "" {
		return nil, errors.New("autopower: userID, methodID and authCode are required")
	}
	return box.EncryptBound(strings.TrimSpace(authCode), authCodeAAD(userID, methodID))
}

// OpenAuthCode decrypts a reusable PSP authorization_code bound to userID and methodID.
func OpenAuthCode(box *secure.Box, userID string, methodID uuid.UUID, ct []byte) (string, error) {
	if userID == "" || methodID == uuid.Nil {
		return "", errors.New("autopower: userID and methodID are required")
	}
	return box.DecryptBound(ct, authCodeAAD(userID, methodID))
}

// SealAuthEmail encrypts the PSP customer email associated with the saved authorization.
func SealAuthEmail(box *secure.Box, userID string, methodID uuid.UUID, email string) ([]byte, error) {
	if userID == "" || methodID == uuid.Nil || strings.TrimSpace(email) == "" {
		return nil, errors.New("autopower: userID, methodID and email are required")
	}
	return box.EncryptBound(strings.ToLower(strings.TrimSpace(email)), authEmailAAD(userID, methodID))
}

// OpenAuthEmail decrypts the PSP customer email associated with the saved authorization.
func OpenAuthEmail(box *secure.Box, userID string, methodID uuid.UUID, ct []byte) (string, error) {
	if userID == "" || methodID == uuid.Nil {
		return "", errors.New("autopower: userID and methodID are required")
	}
	return box.DecryptBound(ct, authEmailAAD(userID, methodID))
}

// ---- Rules & runs ----------------------------------------------------------

type TriggerType string

const (
	TriggerSchedule   TriggerType = "SCHEDULE"
	TriggerLowBalance TriggerType = "LOW_BALANCE"
	TriggerBoth       TriggerType = "BOTH"
)

func (t TriggerType) UsesSchedule() bool {
	return t == TriggerSchedule || t == TriggerBoth
}

func (t TriggerType) UsesLowBalance() bool {
	return t == TriggerLowBalance || t == TriggerBoth
}

type Frequency string

const (
	FreqNone    Frequency = ""
	FreqWeekly  Frequency = "WEEKLY"
	FreqMonthly Frequency = "MONTHLY"
)

type RuleStatus string

const (
	RuleActive    RuleStatus = "ACTIVE"
	RulePaused    RuleStatus = "PAUSED"
	RuleCancelled RuleStatus = "CANCELLED"
	RuleCompleted RuleStatus = "COMPLETED"
)

const (
	PauseUser                 = "USER_PAUSED"
	PauseConsecutiveFailures  = "CONSECUTIVE_FAILURES"
	PausePaymentMethodDeleted = "PAYMENT_METHOD_DELETED"
	PauseCardExpired          = "CARD_EXPIRED"
	PauseMeterAccessRevoked   = "METER_ACCESS_REVOKED"
	PauseIneligibleReading    = "LOW_BALANCE_INELIGIBLE"
)

type Rule struct {
	ID                      uuid.UUID   `json:"rule_id"`
	UserID                  string      `json:"-"`
	MeterID                 uuid.UUID   `json:"meter_id"`
	PaymentMethodID         uuid.UUID   `json:"payment_method_id"`
	TriggerType             TriggerType `json:"trigger_type"`
	AmountMinor             int64       `json:"amount_minor"`
	MonthlyCapMinor         int64       `json:"monthly_cap_minor"`
	Currency                string      `json:"currency"`
	DeliveryPreference      string      `json:"delivery_preference"`
	Frequency               Frequency   `json:"frequency,omitempty"`
	DaysOfWeek              []int       `json:"days_of_week,omitempty"`  // ISO 1=Mon .. 7=Sun
	DaysOfMonth             []int       `json:"days_of_month,omitempty"` // 1..31 (clamped on short months)
	TimeOfDay               string      `json:"time_of_day,omitempty"`   // "HH:MM" in Timezone
	Timezone                string      `json:"timezone"`
	EndsAt                  *time.Time  `json:"ends_at,omitempty"`
	LowBalanceMilliKWh      int64       `json:"-"`
	Status                  RuleStatus  `json:"status"`
	PauseReason             string      `json:"pause_reason,omitempty"`
	ConsecutiveFailures     int         `json:"consecutive_failures"`
	NextScheduledAt         *time.Time  `json:"next_scheduled_at,omitempty"`
	NextPollAt              *time.Time  `json:"next_poll_at,omitempty"`
	LastLowBalanceTriggered *time.Time  `json:"last_low_balance_triggered_at,omitempty"`
	LastReadingMilliKWh     *int64      `json:"-"`
	LastReadingAt           *time.Time  `json:"last_reading_at,omitempty"`
	LeaseUntil              time.Time   `json:"-"`
	CreatedAt               time.Time   `json:"created_at"`
	UpdatedAt               time.Time   `json:"updated_at"`
}

// MilliKWhFromFloat converts kWh to exact integer milli-kWh (0.001 kWh).
func MilliKWhFromFloat(kwh float64) (int64, error) {
	if math.IsNaN(kwh) || math.IsInf(kwh, 0) || kwh < 0.1 || kwh > 10000 {
		return 0, ErrLowBalanceThreshold
	}
	return int64(math.Round(kwh * 1000)), nil
}

// FloatFromMilliKWh converts exact integer milli-kWh back to kWh for JSON views.
func FloatFromMilliKWh(milli int64) float64 {
	return float64(milli) / 1000.0
}

type TriggerSource string

const (
	SourceSchedule   TriggerSource = "SCHEDULE"
	SourceLowBalance TriggerSource = "LOW_BALANCE"
)

type RunStatus string

const (
	RunScheduled   RunStatus = "SCHEDULED"
	RunCharging    RunStatus = "CHARGING"
	RunSucceeded   RunStatus = "SUCCEEDED"
	RunFailed      RunStatus = "FAILED"
	RunRefunded    RunStatus = "REFUNDED"
	RunSkippedUser RunStatus = "SKIPPED_USER"
	RunSkippedCap  RunStatus = "SKIPPED_CAP"
	RunMissed      RunStatus = "MISSED"
	RunCancelled   RunStatus = "CANCELLED"
)

// CountsTowardCap reports whether a run reserves or consumes monthly/daily spend limits.
func (s RunStatus) CountsTowardCap() bool {
	return s == RunCharging || s == RunSucceeded
}

type Run struct {
	ID                 uuid.UUID     `json:"run_id"`
	RuleID             uuid.UUID     `json:"rule_id"`
	UserID             string        `json:"-"`
	MeterID            uuid.UUID     `json:"meter_id"`
	TriggerSource      TriggerSource `json:"trigger_source"`
	ScheduledFor       time.Time     `json:"scheduled_for"`
	ExecutedAt         *time.Time    `json:"executed_at,omitempty"`
	AmountMinor        int64         `json:"amount_minor"`
	TotalMinor         int64         `json:"total_minor,omitempty"`
	Currency           string        `json:"currency"`
	Status             RunStatus     `json:"status"`
	HeadsUpSentAt      *time.Time    `json:"heads_up_sent_at,omitempty"`
	TransactionID      *uuid.UUID    `json:"transaction_id,omitempty"`
	PaymentProviderRef string        `json:"-"`
	ChargeDispatchedAt *time.Time    `json:"-"`
	FailureReason      string        `json:"failure_reason,omitempty"`
	CompletedAt        *time.Time    `json:"completed_at,omitempty"`
	LeaseUntil         time.Time     `json:"-"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// ParseTimeOfDay parses strict 24-hour "HH:MM" into hour (0..23) and minute (0..59).
func ParseTimeOfDay(s string) (hour, minute int, normalized string, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, "", fmt.Errorf("%w: time_of_day must be HH:MM (00:00–23:59)", ErrInvalidSchedule)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, "", fmt.Errorf("%w: time_of_day must be HH:MM (00:00–23:59)", ErrInvalidSchedule)
	}
	return h, m, fmt.Sprintf("%02d:%02d", h, m), nil
}

// NormalizeIntList deduplicates, sorts and validates day numbers within [minVal, maxVal].
func NormalizeIntList(in []int, minVal, maxVal int) ([]int, error) {
	if len(in) == 0 {
		return nil, ErrInvalidSchedule
	}
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if v < minVal || v > maxVal {
			return nil, fmt.Errorf("%w: day %d out of allowed range %d..%d", ErrInvalidSchedule, v, minVal, maxVal)
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out, nil
}
