package gifts

import "testing"

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		gift             bool
		phone            string
		requested, valid bool
	}{
		{true, "", false, true}, {false, "", false, true}, {true, "+2348000000123", true, true},
		{false, "+2348000000123", true, false}, {true, "+2348000000123", false, false},
		{true, "", true, false}, {true, "08000000123", true, false}, {true, "+2348000000123\n", true, false},
	} {
		if (Validate(tc.gift, tc.phone, tc.requested) == nil) != tc.valid {
			t.Errorf("unexpected validation result: %+v", tc)
		}
	}
}
