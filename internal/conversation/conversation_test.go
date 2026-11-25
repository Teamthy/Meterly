package conversation_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/secure"
)

func TestFormatUSSDResponseEnforces182CharLimit(t *testing.T) {
	short := conversation.FormatUSSDResponse(true, "1. Buy for my meter\n2. Buy for saved meter")
	if !strings.HasPrefix(short, "CON ") || len(short) > conversation.MaxUSSDChars {
		t.Fatalf("unexpected short USSD response: %q (len=%d)", short, len(short))
	}

	veryLong := strings.Repeat("Meter ****1234 (IKEDC) N50,000 | ", 20)
	got := conversation.FormatUSSDResponse(false, veryLong)
	if !strings.HasPrefix(got, "END ") {
		t.Fatalf("expected END prefix, got %q", got)
	}
	if len(got) > conversation.MaxUSSDChars || utf8.RuneCountInString(got) > conversation.MaxUSSDChars {
		t.Fatalf("USSD response exceeded 182 limit: bytes=%d runes=%d %q", len(got), utf8.RuneCountInString(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated USSD response split a UTF-8 sequence: %q", got)
	}
}

func TestExtractUSSDLatestInputAndParseAmount(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"1", "1"},
		{"1*2", "2"},
		{"1*2*5000", "5000"},
		{"1*2*5000*1", "1"},
	}
	for _, tc := range cases {
		if got := conversation.ExtractUSSDLatestInput(tc.raw); got != tc.want {
			t.Fatalf("ExtractUSSDLatestInput(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	amtCases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"5000", 500_000, true},
		{"₦5,000", 500_000, true},
		{"N10,500", 1_050_000, true},
		{"0", 0, false},
		{"-100", 0, false},
		{"50.5", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range amtCases {
		got, err := conversation.ParseNairaAmountMinor(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("ParseNairaAmountMinor(%q) = (%d, %v), want (%d, nil)", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("ParseNairaAmountMinor(%q) = %d, want error", tc.in, got)
		}
	}
}

func TestOTPVerifySIMSwapAndLagosDayBounds(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := conversation.GenerateOTPCode()
	if err != nil || len(code) != 6 {
		t.Fatalf("GenerateOTPCode() = %q, %v", code, err)
	}
	id := uuid.New()
	h := conversation.OTPCodeHMAC(box, id, code)
	if !conversation.VerifyOTPCode(box, id, code, h) {
		t.Fatal("expected valid OTP to verify")
	}
	if conversation.VerifyOTPCode(box, id, "000000", h) && code != "000000" {
		t.Fatal("expected wrong OTP to fail")
	}
	if conversation.VerifyOTPCode(box, uuid.New(), code, h) {
		t.Fatal("expected OTP bound to different ID to fail")
	}

	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	link := &conversation.PhoneLink{
		Status:       conversation.LinkVerified,
		VerifiedAt:   &t0,
		LastActiveAt: t0,
	}
	if link.NeedsReverification(t0.Add(29*24*time.Hour), 30*24*time.Hour) {
		t.Fatal("29 days inactive should not require reverification")
	}
	if !link.NeedsReverification(t0.Add(31*24*time.Hour), 30*24*time.Hour) {
		t.Fatal("31 days inactive must require SIM-swap reverification")
	}

	// 2026-11-23T23:30:00Z == 2026-11-24T00:30:00+01:00 (Africa/Lagos).
	startUTC, endUTC := conversation.LagosDayBounds(time.Date(2026, 11, 23, 23, 30, 0, 0, time.UTC))
	wantStart := time.Date(2026, 11, 23, 23, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	if !startUTC.Equal(wantStart) || !endUTC.Equal(wantEnd) {
		t.Fatalf("LagosDayBounds = [%s, %s), want [%s, %s)", startUTC, endUTC, wantStart, wantEnd)
	}

	if got := conversation.IdempotencyKey(conversation.ChannelUSSD, "ATUid_123", 4); got != "ussd:ATUid_123:4" {
		t.Fatalf("unexpected IdempotencyKey: %q", got)
	}
}
