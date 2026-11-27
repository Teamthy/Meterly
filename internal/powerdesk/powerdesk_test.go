package powerdesk_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/powerdesk"
)

func TestRoleHierarchyAndApprovalRule(t *testing.T) {
	if !powerdesk.RoleOwner.Allows(powerdesk.RoleManager) || !powerdesk.RoleOwner.Allows(powerdesk.RoleViewer) {
		t.Fatal("OWNER must allow MANAGER and VIEWER")
	}
	if !powerdesk.RoleManager.Allows(powerdesk.RoleViewer) || powerdesk.RoleManager.Allows(powerdesk.RoleOwner) {
		t.Fatal("MANAGER must allow VIEWER and deny OWNER")
	}
	if powerdesk.RoleViewer.Allows(powerdesk.RoleManager) || powerdesk.RoleViewer.Allows(powerdesk.RoleOwner) {
		t.Fatal("VIEWER must not allow MANAGER or OWNER")
	}

	threshold := int64(5_000_000) // ₦50,000
	org := &powerdesk.Organisation{ManagerApprovalThresholdMinor: &threshold}
	if org.NeedsManagerApproval(powerdesk.RoleManager, 5_000_000) {
		t.Fatal("exact threshold should not require approval")
	}
	if !org.NeedsManagerApproval(powerdesk.RoleManager, 5_000_001) {
		t.Fatal("above threshold must require approval for MANAGER")
	}
	if org.NeedsManagerApproval(powerdesk.RoleOwner, 50_000_000) {
		t.Fatal("OWNER never requires approval")
	}
}

func TestAccountNameMatchAndEvidenceValidation(t *testing.T) {
	org := &powerdesk.Organisation{
		Name:      "Lekki Palms Estate",
		LegalName: "Lekki Palms Property Management Ltd.",
	}
	if !powerdesk.AccountNamesMatch("LEKKI PALMS PROPERTY MANAGEMENT LTD", org) {
		t.Fatal("expected normalized legal_name match")
	}
	if !powerdesk.AccountNamesMatch("lekki-palms estate", org) {
		t.Fatal("expected normalized name match")
	}
	if powerdesk.AccountNamesMatch("Unrelated Landlord Ventures", org) {
		t.Fatal("mismatched customer_name must be rejected")
	}

	if _, err := powerdesk.ValidateEvidenceRef("https://evil.example/deed.pdf"); err == nil {
		t.Fatal("URLs in evidence_reference must be rejected")
	}
	ref, err := powerdesk.ValidateEvidenceRef("  DEED-2026-LAG-00419 ")
	if err != nil || ref != "DEED-2026-LAG-00419" {
		t.Fatalf("unexpected ValidateEvidenceRef result %q, %v", ref, err)
	}
}

func TestStatementMonthAndCSVLabellingAndSanitization(t *testing.T) {
	startUTC, endUTC, canonical, err := powerdesk.ParseStatementMonth("2026-11")
	if err != nil || canonical != "2026-11" {
		t.Fatalf("unexpected ParseStatementMonth err: %v", err)
	}
	// Midnight 2026-11-01 WAT (UTC+1) == 2026-10-31T23:00:00Z
	if startUTC.Format("2006-01-02T15:04:05Z") != "2026-10-31T23:00:00Z" {
		t.Fatalf("unexpected startUTC: %s", startUTC.Format("2006-01-02T15:04:05Z"))
	}
	if endUTC.Format("2006-01-02T15:04:05Z") != "2026-11-30T23:00:00Z" {
		t.Fatalf("unexpected endUTC: %s", endUTC.Format("2006-01-02T15:04:05Z"))
	}
	if _, _, _, err := powerdesk.ParseStatementMonth("2026-13"); err == nil {
		t.Fatal("month 13 must fail")
	}

	propID := uuid.New()
	unitID := uuid.New()
	st := &powerdesk.MonthlyStatement{
		OrganisationID:            uuid.New(),
		OrganisationName:          "Lekki Gardens",
		Month:                     "2026-11",
		Timezone:                  "Africa/Lagos",
		Description:               powerdesk.StatementDescription,
		ElectricityPurchasedMinor: 500_000,
		FeesMinor:                 10_000,
		RefundsMinor:              0,
		NetChargedMinor:           510_000,
		Currency:                  money.NGN,
		VendedCount:               1,
		Properties: []powerdesk.PropertyStatement{{
			PropertyID:                propID,
			PropertyName:              "=CMD|' /C calc'!A0",
			Description:               powerdesk.StatementDescription,
			ElectricityPurchasedMinor: 500_000,
			FeesMinor:                 10_000,
			NetChargedMinor:           510_000,
			Currency:                  money.NGN,
			VendedCount:               1,
			Units: []powerdesk.UnitStatementRow{{
				PropertyID:                propID,
				PropertyName:              "=CMD|' /C calc'!A0",
				UnitID:                    unitID,
				UnitName:                  "+Flat 2B",
				MaskedMeterNumber:         "*******5678",
				Disco:                     "IKEDC",
				Description:               powerdesk.StatementDescription,
				ElectricityPurchasedMinor: 500_000,
				FeesMinor:                 10_000,
				NetChargedMinor:           510_000,
				Currency:                  money.NGN,
				VendedCount:               1,
			}},
		}},
	}
	csvBytes, err := powerdesk.FormatStatementCSV(st)
	if err != nil {
		t.Fatal(err)
	}
	out := string(csvBytes)
	if !strings.Contains(out, "electricity_purchased_minor") || !strings.Contains(out, "electricity purchased") {
		t.Fatalf("CSV must explicitly label 'electricity purchased': %s", out)
	}
	if strings.Contains(strings.ToLower(out), "electricity used") || strings.Contains(strings.ToLower(out), "electricity_used") {
		t.Fatalf("CSV must never label prepaid vending as 'used': %s", out)
	}
	if !strings.Contains(out, "'=CMD|") || !strings.Contains(out, "'+Flat 2B") {
		t.Fatalf("CSV formula prefixes must be neutralized: %s", out)
	}
	if got := powerdesk.FormatBulkSummary(10, 1, 1); got != "10 delivered, 1 failed and refunded, 1 pending" {
		t.Fatalf("unexpected FormatBulkSummary: %q", got)
	}
}
