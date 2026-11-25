package autopower

import (
	"fmt"
	"time"
)

// ScheduleSpec captures the calendar schedule of a rule.
type ScheduleSpec struct {
	Frequency   Frequency
	DaysOfWeek  []int // 1=Mon .. 7=Sun
	DaysOfMonth []int // 1..31
	TimeOfDay   string
	EndsAt      *time.Time
}

// Validate normalizes and validates s.
func (s *ScheduleSpec) Validate() error {
	_, _, normTime, err := ParseTimeOfDay(s.TimeOfDay)
	if err != nil {
		return err
	}
	s.TimeOfDay = normTime
	switch s.Frequency {
	case FreqWeekly:
		if len(s.DaysOfMonth) > 0 {
			return fmt.Errorf("%w: days_of_month cannot be set when frequency=WEEKLY", ErrInvalidSchedule)
		}
		days, err := NormalizeIntList(s.DaysOfWeek, 1, 7)
		if err != nil {
			return err
		}
		s.DaysOfWeek = days
	case FreqMonthly:
		if len(s.DaysOfWeek) > 0 {
			return fmt.Errorf("%w: days_of_week cannot be set when frequency=MONTHLY", ErrInvalidSchedule)
		}
		days, err := NormalizeIntList(s.DaysOfMonth, 1, 31)
		if err != nil {
			return err
		}
		s.DaysOfMonth = days
	default:
		return fmt.Errorf("%w: frequency must be WEEKLY or MONTHLY", ErrInvalidSchedule)
	}
	return nil
}

// ScheduleSpecFromRule extracts the ScheduleSpec from r.
func ScheduleSpecFromRule(r *Rule) ScheduleSpec {
	return ScheduleSpec{
		Frequency:   r.Frequency,
		DaysOfWeek:  append([]int(nil), r.DaysOfWeek...),
		DaysOfMonth: append([]int(nil), r.DaysOfMonth...),
		TimeOfDay:   r.TimeOfDay,
		EndsAt:      r.EndsAt,
	}
}

// ResolveLocation loads an IANA timezone name, defaulting to Africa/Lagos.
// In v1 user rules always use Africa/Lagos; tests may inject DST-observing
// zones to verify spring-forward and fall-back handling.
func ResolveLocation(tz string) (*time.Location, error) {
	if tz == "" || tz == DefaultTimezone {
		return lagosLoc, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, ErrInvalidTimezone
	}
	return loc, nil
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func isoWeekday(wd time.Weekday) int {
	if wd == time.Sunday {
		return 7
	}
	return int(wd)
}

// wallTimeOnDate resolves (year, month, day, hour, minute) in loc to an exact
// Instant, handling DST transitions safely:
//   - Fall-back (repeated local wall time): selects the first occurrence once.
//   - Spring-forward (non-existent gap wall time): advances to the first valid
//     local instant at or after the transition rather than jumping backward.
func wallTimeOnDate(year int, month time.Month, day, wantHour, wantMin int, loc *time.Location) time.Time {
	cand := time.Date(year, month, day, wantHour, wantMin, 0, 0, loc)
	if cand.Year() == year && cand.Month() == month && cand.Day() == day &&
		cand.Hour() == wantHour && cand.Minute() == wantMin {
		return cand
	}
	// Non-existent local wall time during a spring-forward transition: Go's
	// time.Date normalizes backward into the pre-transition offset. Step
	// forward minute-by-minute across the gap to land on the exact transition
	// boundary (at most 180 iterations).
	wantTotal := wantHour*60 + wantMin
	cur := cand
	for i := 0; i < 180; i++ {
		cur = cur.Add(time.Minute)
		if cur.Year() > year ||
			(cur.Year() == year && cur.Month() > month) ||
			(cur.Year() == year && cur.Month() == month && cur.Day() > day) ||
			(cur.Year() == year && cur.Month() == month && cur.Day() == day && cur.Hour()*60+cur.Minute() >= wantTotal) {
			return cur.Truncate(time.Minute)
		}
	}
	return cand
}

func matchesCalendarDay(year int, month time.Month, day int, wd time.Weekday, spec ScheduleSpec) bool {
	switch spec.Frequency {
	case FreqWeekly:
		iso := isoWeekday(wd)
		for _, want := range spec.DaysOfWeek {
			if want == iso {
				return true
			}
		}
	case FreqMonthly:
		last := daysInMonth(year, month)
		for _, dom := range spec.DaysOfMonth {
			eff := dom
			if eff > last {
				eff = last
			}
			if day == eff {
				return true
			}
		}
	}
	return false
}

// NextSlotAfter returns the strictly earliest scheduled UTC instant > afterUTC
// in loc. Returns ok=false if spec is invalid or the next slot exceeds EndsAt.
func NextSlotAfter(afterUTC time.Time, spec ScheduleSpec, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = lagosLoc
	}
	wantHour, wantMin, _, err := ParseTimeOfDay(spec.TimeOfDay)
	if err != nil {
		return time.Time{}, false
	}
	if spec.Frequency != FreqWeekly && spec.Frequency != FreqMonthly {
		return time.Time{}, false
	}
	if (spec.Frequency == FreqWeekly && len(spec.DaysOfWeek) == 0) ||
		(spec.Frequency == FreqMonthly && len(spec.DaysOfMonth) == 0) {
		return time.Time{}, false
	}

	afterUTC = afterUTC.UTC()
	local := afterUTC.In(loc)
	startYear, startMonth, startDay := local.Date()

	// Anchor calendar-day iteration at noon local time so 23h/25h DST days never
	// skip or repeat a calendar date. Start at offset -1 to cover offset edges.
	for offset := -1; offset <= 400; offset++ {
		noon := time.Date(startYear, startMonth, startDay+offset, 12, 0, 0, 0, loc)
		y, m, d := noon.Date()
		if !matchesCalendarDay(y, m, d, noon.Weekday(), spec) {
			continue
		}
		slotUTC := wallTimeOnDate(y, m, d, wantHour, wantMin, loc).UTC()
		if !slotUTC.After(afterUTC) {
			continue
		}
		if spec.EndsAt != nil && slotUTC.After(spec.EndsAt.UTC()) {
			return time.Time{}, false
		}
		return slotUTC, true
	}
	return time.Time{}, false
}

// MonthBoundsUTC returns [startUTC, endUTC) of the calendar month in loc that
// contains instant.
func MonthBoundsUTC(instant time.Time, loc *time.Location) (startUTC, endUTC time.Time) {
	if loc == nil {
		loc = lagosLoc
	}
	local := instant.In(loc)
	startLocal := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	endLocal := time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc)
	return startLocal.UTC(), endLocal.UTC()
}

// DayBoundsUTC returns [startUTC, endUTC) of the calendar day in loc that
// contains instant.
func DayBoundsUTC(instant time.Time, loc *time.Location) (startUTC, endUTC time.Time) {
	if loc == nil {
		loc = lagosLoc
	}
	local := instant.In(loc)
	startLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	endLocal := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc)
	return startLocal.UTC(), endLocal.UTC()
}
