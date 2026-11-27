package orchestrator

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// ErrReadingUnavailable means no confirmed kWh reading is available for this meter.
var ErrReadingUnavailable = errors.New("no confirmed meter reading is available")

const (
	usageHistory  = 7 * 24 * time.Hour
	usageLimit    = 200
	minRateWindow = 5 * time.Minute
	sampleDedupe  = 30 * time.Second
)

type MeterReading struct {
	MeterID   uuid.UUID `json:"meter_id"`
	KWh       float64   `json:"kwh"`
	ReadAt    time.Time `json:"read_at"`
	Provider  string    `json:"provider"`
	Confirmed bool      `json:"confirmed"`
}

type LastCredit struct {
	UnitsKWh       float64   `json:"units_kwh"`
	CreditedAt     time.Time `json:"credited_at"`
	DeliveryMethod string    `json:"delivery_method"`
	TransactionID  uuid.UUID `json:"transaction_id"`
}

type Consumption struct {
	KWhPerHour  float64   `json:"kwh_per_hour"`
	KWhPerDay   float64   `json:"kwh_per_day"`
	WindowHours float64   `json:"window_hours"`
	Samples     int       `json:"samples"`
	ComputedAt  time.Time `json:"computed_at"`
}

// MeterUsage is the server-computed remaining balance, consumption rate and
// time-left. The web app must display these fields, not derive them.
type MeterUsage struct {
	MeterID                 uuid.UUID    `json:"meter_id"`
	RemainingKWh            *float64     `json:"remaining_kwh"`
	RemainingSource         string       `json:"remaining_source"`
	ReadAt                  *time.Time   `json:"read_at,omitempty"`
	Provider                string       `json:"provider,omitempty"`
	Confirmed               bool         `json:"confirmed"`
	Consumption             *Consumption `json:"consumption"`
	EstimatedEmptyAt        *time.Time   `json:"estimated_empty_at,omitempty"`
	EstimatedHoursRemaining *float64     `json:"estimated_hours_remaining,omitempty"`
	LastCredit              *LastCredit  `json:"last_credit"`
	UnavailableReason       string       `json:"unavailable_reason,omitempty"`
}

// LatestMeterReading returns a live kWh reading when the meter snapshot says
// remote reading is supported and a MeterReadingProvider is configured.
// It never invents AMI: no provider → unavailable.
func (o *Orchestrator) LatestMeterReading(ctx context.Context, userID string, meterID uuid.UUID) (MeterReading, error) {
	m, err := o.Meters.Authorize(ctx, userID, meterID, meters.PermView)
	if err != nil {
		return MeterReading{}, err
	}
	return o.fetchAndRecordReading(ctx, m)
}

func (o *Orchestrator) fetchAndRecordReading(ctx context.Context, m *meters.Meter) (MeterReading, error) {
	var zero MeterReading
	if !m.Capabilities.RemoteReading.Supported {
		return zero, ErrNotEligible
	}
	if o.readings == nil {
		return zero, ErrReadingUnavailable
	}
	number, err := o.box.Decrypt(m.NumberEncrypted)
	if err != nil {
		return zero, err
	}
	kWh, at, err := o.readings.LatestReading(ctx, number)
	if errors.Is(err, providers.ErrReadingUnavailable) {
		return zero, ErrReadingUnavailable
	}
	if err != nil {
		return zero, err
	}
	if at.IsZero() {
		at = o.Now().UTC()
	}
	reading := MeterReading{
		MeterID:   m.ID,
		KWh:       kWh,
		ReadAt:    at.UTC(),
		Provider:  o.readings.Name(),
		Confirmed: m.Capabilities.RemoteReading.Confirmed,
	}
	o.recordSample(ctx, m.ID, reading)
	return reading, nil
}

func (o *Orchestrator) recordSample(ctx context.Context, meterID uuid.UUID, reading MeterReading) {
	recent, err := o.st.Repos().Readings().ListSamples(ctx, meterID, time.Time{}, 1)
	if err == nil && len(recent) > 0 {
		last := recent[0]
		dt := reading.ReadAt.Sub(last.ReadAt)
		if dt < 0 {
			dt = -dt
		}
		if last.MilliKWh == meters.MilliKWh(reading.KWh) && dt < sampleDedupe {
			return
		}
	}
	_ = o.st.Repos().Readings().InsertSample(ctx, meters.ReadingSample{
		ID: uuid.New(), MeterID: meterID, MilliKWh: meters.MilliKWh(reading.KWh),
		ReadAt: reading.ReadAt, Provider: reading.Provider,
	})
}

// MeterUsage returns remaining units, consumption rate and estimated empty
// time computed on the server from stored reading samples.
func (o *Orchestrator) MeterUsage(ctx context.Context, userID string, meterID uuid.UUID) (MeterUsage, error) {
	m, err := o.Meters.Authorize(ctx, userID, meterID, meters.PermView)
	if err != nil {
		return MeterUsage{}, err
	}
	return o.usageForMeter(ctx, userID, m), nil
}

// ListMeterUsage returns usage snapshots for every meter the caller can view.
func (o *Orchestrator) ListMeterUsage(ctx context.Context, userID string) ([]MeterUsage, error) {
	list, err := o.Meters.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]MeterUsage, 0, len(list))
	for _, item := range list {
		m, err := o.Meters.Authorize(ctx, userID, item.ID, meters.PermView)
		if err != nil {
			continue
		}
		out = append(out, o.usageForMeter(ctx, userID, m))
	}
	return out, nil
}

func (o *Orchestrator) usageForMeter(ctx context.Context, userID string, m *meters.Meter) MeterUsage {
	now := o.Now().UTC()
	u := MeterUsage{
		MeterID:         m.ID,
		RemainingSource: "unavailable",
		LastCredit:      o.lastCredit(ctx, userID, m.ID),
	}
	if !m.Capabilities.RemoteReading.Supported {
		u.UnavailableReason = "REMOTE_READING_NOT_SUPPORTED"
		return u
	}
	if o.readings == nil {
		u.UnavailableReason = "NO_PROVIDER"
		u.Consumption = o.consumptionFromStore(ctx, m.ID, now)
		return u
	}
	reading, err := o.fetchAndRecordReading(ctx, m)
	if err != nil {
		u.UnavailableReason = "NO_READING"
		u.Consumption = o.consumptionFromStore(ctx, m.ID, now)
		return u
	}
	kwh := reading.KWh
	at := reading.ReadAt
	u.RemainingKWh = &kwh
	u.RemainingSource = "remote_reading"
	u.ReadAt = &at
	u.Provider = reading.Provider
	u.Confirmed = reading.Confirmed
	u.Consumption = o.consumptionFromStore(ctx, m.ID, now)
	if u.Consumption != nil && u.Consumption.KWhPerHour > 0 && kwh >= 0 {
		hours := kwh / u.Consumption.KWhPerHour
		u.EstimatedHoursRemaining = &hours
		empty := now.Add(time.Duration(hours * float64(time.Hour)))
		u.EstimatedEmptyAt = &empty
	}
	return u
}

func (o *Orchestrator) consumptionFromStore(ctx context.Context, meterID uuid.UUID, now time.Time) *Consumption {
	samples, err := o.st.Repos().Readings().ListSamples(ctx, meterID, now.Add(-usageHistory), usageLimit)
	if err != nil {
		return nil
	}
	return computeConsumption(samples, now)
}

func (o *Orchestrator) lastCredit(ctx context.Context, userID string, meterID uuid.UUID) *LastCredit {
	list, err := o.st.Repos().Transactions().Search(ctx, store.TxnFilter{
		CustomerID: userID, MeterID: meterID, Vending: tx.VendConfirmed, Limit: 30,
	})
	if err != nil {
		return nil
	}
	for _, t := range list {
		if t.MeterID != meterID || strings.TrimSpace(t.UnitsKWh) == "" {
			continue
		}
		units, err := strconv.ParseFloat(strings.TrimSpace(t.UnitsKWh), 64)
		if err != nil || units < 0 {
			continue
		}
		at := t.UpdatedAt.UTC()
		if at.IsZero() {
			at = t.CreatedAt.UTC()
		}
		return &LastCredit{
			UnitsKWh: units, CreditedAt: at, DeliveryMethod: string(t.DeliveryMethod), TransactionID: t.ID,
		}
	}
	return nil
}

func computeConsumption(samples []meters.ReadingSample, now time.Time) *Consumption {
	if len(samples) < 2 {
		return nil
	}
	// Store returns newest first; work oldest → newest.
	asc := make([]meters.ReadingSample, len(samples))
	copy(asc, samples)
	for i, j := 0, len(asc)-1; i < j; i, j = i+1, j-1 {
		asc[i], asc[j] = asc[j], asc[i]
	}
	start := 0
	for i := 1; i < len(asc); i++ {
		if asc[i].MilliKWh > asc[i-1].MilliKWh {
			start = i // last top-up; measure decline after it
		}
	}
	window := asc[start:]
	if len(window) < 2 {
		return nil
	}
	first, last := window[0], window[len(window)-1]
	dt := last.ReadAt.Sub(first.ReadAt)
	if dt < minRateWindow {
		return nil
	}
	consumed := meters.KWh(first.MilliKWh - last.MilliKWh)
	if consumed < 0 {
		consumed = 0
	}
	hours := dt.Hours()
	if hours <= 0 {
		return nil
	}
	perHour := consumed / hours
	return &Consumption{
		KWhPerHour:  round3(perHour),
		KWhPerDay:   round3(perHour * 24),
		WindowHours: round3(hours),
		Samples:     len(window),
		ComputedAt:  now.UTC(),
	}
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}
