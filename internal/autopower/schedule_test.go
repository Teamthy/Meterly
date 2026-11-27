package autopower

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

func TestMonthlyScheduleAcrossMonthAndLeapYearBoundaries(t *testing.T) {
	loc := LagosLocation()

	// 1st and 15th at 07:00 Africa/Lagos (= 06:00 UTC).
	spec := ScheduleSpec{
		Frequency:   FreqMonthly,
		DaysOfMonth: []int{15, 1, 15},
		TimeOfDay:   "07:00",
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	cur := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	wantUTC := []time.Time{
		time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 15, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 15, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 2, 1, 6, 0, 0, 0, time.UTC),
	}
	for i, want := range wantUTC {
		got, ok := NextSlotAfter(cur, spec, loc)
		if !ok || !got.Equal(want) {
			t.Fatalf("step %d: got (%v, %v), want (%v, true)", i, got, ok, want)
		}
		cur = got
	}

	// 31st clamps cleanly across Jan 31 -> Feb 28 (non-leap) -> Mar 31 -> Apr 30,
	// and hits Feb 29 on a leap year (2028).
	endOfMonth := ScheduleSpec{
		Frequency:   FreqMonthly,
		DaysOfMonth: []int{31},
		TimeOfDay:   "07:00",
	}
	cur = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	wantClamped := []time.Time{
		time.Date(2027, 1, 31, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 2, 28, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 31, 6, 0, 0, 0, time.UTC),
		time.Date(2027, 4, 30, 6, 0, 0, 0, time.UTC),
	}
	for i, want := range wantClamped {
		got, ok := NextSlotAfter(cur, endOfMonth, loc)
		if !ok || !got.Equal(want) {
			t.Fatalf("clamped step %d: got (%v, %v), want (%v, true)", i, got, ok, want)
		}
		cur = got
	}

	leapFeb, ok := NextSlotAfter(time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC), endOfMonth, loc)
	if !ok || !leapFeb.Equal(time.Date(2028, 2, 29, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("leap year Feb: got (%v, %v)", leapFeb, ok)
	}
}

func TestDSTTransitionsSpringForwardAndFallBack(t *testing.T) {
	ny, err := ResolveLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}

	// Spring-forward: 2026-03-08 clocks jump 02:00 EST -> 03:00 EDT.
	// A weekly Sunday rule at 02:30 must fire once at 03:00 EDT (07:00 UTC),
	// never jumping backward to 01:30 EST or skipping the day.
	springSpec := ScheduleSpec{
		Frequency:  FreqWeekly,
		DaysOfWeek: []int{7}, // Sunday
		TimeOfDay:  "02:30",
	}
	beforeSpring := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC) // 00:00 EST
	slot1, ok := NextSlotAfter(beforeSpring, springSpec, ny)
	wantSpring := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC) // 03:00 EDT
	if !ok || !slot1.Equal(wantSpring) {
		t.Fatalf("spring-forward slot: got (%v, %v), want %v", slot1, ok, wantSpring)
	}
	// Advancing from slot1 must move to next Sunday (2026-03-15 02:30 EDT = 06:30 UTC).
	slot2, ok := NextSlotAfter(slot1, springSpec, ny)
	wantNextSun := time.Date(2026, 3, 15, 6, 30, 0, 0, time.UTC)
	if !ok || !slot2.Equal(wantNextSun) {
		t.Fatalf("post-spring Sunday: got (%v, %v), want %v", slot2, ok, wantNextSun)
	}

	// Fall-back: 2026-11-01 01:30 occurs twice (01:30 EDT = 05:30 UTC, 01:30 EST = 06:30 UTC).
	// Must select the first occurrence once and advance to next Sunday, never firing twice on Nov 1.
	fallSpec := ScheduleSpec{
		Frequency:  FreqWeekly,
		DaysOfWeek: []int{7},
		TimeOfDay:  "01:30",
	}
	beforeFall := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC) // 00:00 EDT
	fallSlot1, ok := NextSlotAfter(beforeFall, fallSpec, ny)
	wantFall1 := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // first 01:30 (EDT)
	if !ok || !fallSlot1.Equal(wantFall1) {
		t.Fatalf("fall-back first slot: got (%v, %v), want %v", fallSlot1, ok, wantFall1)
	}
	fallSlot2, ok := NextSlotAfter(fallSlot1, fallSpec, ny)
	wantFall2 := time.Date(2026, 11, 8, 6, 30, 0, 0, time.UTC) // 2026-11-08 01:30 EST
	if !ok || !fallSlot2.Equal(wantFall2) {
		t.Fatalf("fall-back next slot must be Nov 8 (not second 01:30 on Nov 1): got (%v, %v), want %v", fallSlot2, ok, wantFall2)
	}
}

func TestLagosMonthAndDayBoundsAtUTCMidnightEdge(t *testing.T) {
	loc := LagosLocation()
	// 2026-11-30 23:30 UTC == 2026-12-01 00:30 WAT (Africa/Lagos).
	// Must belong to December 2026 and Dec 1 in Africa/Lagos!
	inst := time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC)
	mStart, mEnd := MonthBoundsUTC(inst, loc)
	if !mStart.Equal(time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)) ||
		!mEnd.Equal(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected Lagos December bounds: [%v, %v)", mStart, mEnd)
	}
	dStart, dEnd := DayBoundsUTC(inst, loc)
	if !dStart.Equal(time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)) ||
		!dEnd.Equal(time.Date(2026, 12, 1, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected Lagos Dec 1 bounds: [%v, %v)", dStart, dEnd)
	}
}

func TestBoundEncryptionAndCardExpiry(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	mid := uuid.New()
	ct, err := SealAuthCode(box, uid, mid, "AUTH_secret_123")
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenAuthCode(box, uid, mid, ct)
	if err != nil || got != "AUTH_secret_123" {
		t.Fatalf("decrypt: %v %q", err, got)
	}
	// Wrong user or wrong method ID must fail AEAD authentication.
	if _, err := OpenAuthCode(box, uuid.NewString(), mid, ct); err == nil {
		t.Fatal("cross-user decrypt must fail")
	}
	if _, err := OpenAuthCode(box, uid, uuid.New(), ct); err == nil {
		t.Fatal("cross-method decrypt must fail")
	}

	pm := &SavedPaymentMethod{ExpMonth: 11, ExpYear: 2026}
	// Valid at 2026-11-30 23:59:59 WAT (2026-11-30 22:59:59 UTC).
	if pm.IsExpiredAt(time.Date(2026, 11, 30, 22, 59, 59, 0, time.UTC), LagosLocation()) {
		t.Fatal("card must still be valid at end of Nov 2026 WAT")
	}
	// Expired at 2026-12-01 00:00:00 WAT (2026-11-30 23:00:00 UTC).
	if !pm.IsExpiredAt(time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC), LagosLocation()) {
		t.Fatal("card must be expired at start of Dec 2026 WAT")
	}
}
