package orchestrator

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
)

func TestComputeConsumptionDeclineAfterTopup(t *testing.T) {
	meter := uuid.New()
	t0 := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	samples := []meters.ReadingSample{
		{MeterID: meter, MilliKWh: 40000, ReadAt: t0.Add(6 * time.Hour)}, // newest 40 kWh
		{MeterID: meter, MilliKWh: 52000, ReadAt: t0.Add(3 * time.Hour)},
		{MeterID: meter, MilliKWh: 10000, ReadAt: t0.Add(2 * time.Hour)}, // top-up from 10 → 52
		{MeterID: meter, MilliKWh: 18000, ReadAt: t0},
	}
	c := computeConsumption(samples, t0.Add(6*time.Hour))
	if c == nil {
		t.Fatal("expected consumption")
	}
	// Window after top-up: 52 → 40 over 3 hours = 4 kWh/h
	if c.KWhPerHour != 4 || c.KWhPerDay != 96 || c.Samples != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestComputeConsumptionNeedsWindow(t *testing.T) {
	meter := uuid.New()
	t0 := time.Now().UTC()
	samples := []meters.ReadingSample{
		{MeterID: meter, MilliKWh: 10000, ReadAt: t0},
		{MeterID: meter, MilliKWh: 10100, ReadAt: t0.Add(time.Minute)},
	}
	if c := computeConsumption(samples, t0); c != nil {
		t.Fatalf("short window: %+v", c)
	}
}

func TestComputeConsumptionIdle(t *testing.T) {
	meter := uuid.New()
	t0 := time.Now().UTC()
	samples := []meters.ReadingSample{
		{MeterID: meter, MilliKWh: 20000, ReadAt: t0.Add(2 * time.Hour)},
		{MeterID: meter, MilliKWh: 20000, ReadAt: t0},
	}
	c := computeConsumption(samples, t0.Add(2*time.Hour))
	if c == nil || c.KWhPerHour != 0 {
		t.Fatalf("%+v", c)
	}
}
