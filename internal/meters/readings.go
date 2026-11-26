package meters

import (
	"math"
	"time"

	"github.com/google/uuid"
)

// ReadingSample is one observed remote kWh balance for a meter.
type ReadingSample struct {
	ID       uuid.UUID
	MeterID  uuid.UUID
	MilliKWh int64
	ReadAt   time.Time
	Provider string
}

func MilliKWh(kwh float64) int64 {
	if math.IsNaN(kwh) || math.IsInf(kwh, 0) || kwh < 0 {
		return 0
	}
	return int64(math.Round(kwh * 1000))
}

func KWh(milli int64) float64 {
	return float64(milli) / 1000.0
}
