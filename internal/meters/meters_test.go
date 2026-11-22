package meters

import "testing"

func TestPermissionsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		held, want Permission
		allowed    bool
	}{
		{PermManage, PermView, true}, {PermManage, PermPurchase, true}, {PermPurchase, PermManage, false}, {PermView, PermPurchase, false},
		{"invalid", PermView, false}, {PermManage, "invalid", false}, {"", "", false},
	} {
		if got := tc.held.Allows(tc.want); got != tc.allowed {
			t.Errorf("%q allows %q = %v", tc.held, tc.want, got)
		}
	}
}
