package households

import "testing"

func TestNamesMatch(t *testing.T) {
	if !NamesMatch("TEST CUSTOMER 5678", "test customer 5678") {
		t.Fatal("case/space")
	}
	if !NamesMatch("Ada-Lovelace", "ada lovelace") {
		t.Fatal("hyphen")
	}
	if NamesMatch("", "x") || NamesMatch("Ada", "Bob") {
		t.Fatal("mismatch")
	}
}

func TestPermissionForRoleNeverManage(t *testing.T) {
	for _, role := range []Role{RoleOwner, RoleAdministrator, RoleMember, RoleViewer, RoleRecipient} {
		if PermissionForRole(role).Allows("MANAGE") && PermissionForRole(role) != "" {
			if string(PermissionForRole(role)) == "MANAGE" {
				t.Fatalf("%s minted MANAGE", role)
			}
		}
	}
	if PermissionForRole(RoleOwner) != "PURCHASE" || PermissionForRole(RoleViewer) != "VIEW" {
		t.Fatal(PermissionForRole(RoleOwner), PermissionForRole(RoleViewer))
	}
}
