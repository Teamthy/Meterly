package meters

import (
	"testing"
	"time"
)

func TestNewRemoteCohort(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	c, err := NewRemoteCohort("ikedc", "62", "Hexing", "MAP letter 2026-03 for IKEDC Hexing AMI", "op-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if c.DiscoCode != "IKEDC" || c.NumberPrefix != "62" || !c.Authorised || c.ID == [16]byte{} {
		t.Fatalf("got %+v", c)
	}
	if !c.Matches("IKEDC", "62111111111") || c.Matches("EKEDC", "62111111111") || c.Matches("IKEDC", "45011111111") {
		t.Fatal("match")
	}

	for _, tc := range []struct{ disco, prefix, evidence string }{
		{"", "62", "enough evidence here"},
		{"IKEDC", "", "enough evidence here"},
		{"IKEDC", "62a", "enough evidence here"},
		{"IKEDC", "62", "no"},
		{"ik", "62", "enough evidence here"}, // disco too short? "IK" is 2 — valid
	} {
		if tc.disco == "ik" {
			if _, err := NewRemoteCohort(tc.disco, tc.prefix, "", tc.evidence, "op", now); err != nil {
				t.Fatalf("IK should be valid: %v", err)
			}
			continue
		}
		if _, err := NewRemoteCohort(tc.disco, tc.prefix, "", tc.evidence, "op", now); err == nil {
			t.Fatalf("expected invalid %+v", tc)
		}
	}
}
