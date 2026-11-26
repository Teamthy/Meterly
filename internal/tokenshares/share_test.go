package tokenshares

import (
	"testing"
	"time"
)

func TestParseChannel(t *testing.T) {
	cases := map[string]string{"COPY": "COPY", "sms": "SMS", "Email": "EMAIL", "WHATSAPP": "WHATSAPP"}
	for in, want := range cases {
		got, err := ParseChannel(in)
		if err != nil || got != want {
			t.Fatalf("%s: %q %v", in, got, err)
		}
	}
	if _, err := ParseChannel("TELEGRAM"); err == nil {
		t.Fatal("telegram")
	}
	if IsSendChannel(Copy) || !IsSendChannel(SMS) {
		t.Fatal("IsSendChannel")
	}
}

func TestLagosDayBounds(t *testing.T) {
	// 23 May 2026 23:30 UTC = 00:30 WAT on 24 May.
	now := time.Date(2026, 5, 23, 23, 30, 0, 0, time.UTC)
	start, end := LagosDayBounds(now)
	if !start.Equal(time.Date(2026, 5, 23, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("start %s", start)
	}
	if !end.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("end %s", end)
	}
}
