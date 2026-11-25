package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestPowerDeskRepoParity(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) store.Store
	}{
		{name: "memstore", open: func(*testing.T) store.Store { return memstore.New() }},
		{name: "pgstore", open: func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runPowerDeskRepoParity(t, tc.open(t))
		})
	}
}

func runPowerDeskRepoParity(t *testing.T, st store.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}

	ownerUID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|pd-owner-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	mgrUID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|pd-mgr-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	viewerUID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|pd-viewer-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	opUID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|pd-op-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	threshold := int64(2_000_000)
	org := &powerdesk.Organisation{
		ID:                            uuid.New(),
		Name:                          "Lekki Phase 1 Courts",
		LegalName:                     "Lekki Phase 1 Courts Ltd",
		RegistrationNumber:            "RC-882910",
		OrganisationType:              powerdesk.OrgEstate,
		VerificationStatus:            powerdesk.VerificationPending,
		ManagerApprovalThresholdMinor: &threshold,
		CreatedAt:                     now,
		UpdatedAt:                     now,
	}
	if err := st.InTx(ctx, func(r store.Repos) error {
		if err := r.PowerDesk().CreateOrg(ctx, org); err != nil {
			return err
		}
		return r.PowerDesk().UpsertMember(ctx, &powerdesk.Member{
			OrganisationID: org.ID,
			UserID:         ownerUID,
			Role:           powerdesk.RoleOwner,
			CreatedAt:      now,
		})
	}); err != nil {
		t.Fatalf("create org + owner: %v", err)
	}

	// Verify InTx rollback leaves zero partial state.
	abortErr := errors.New("abort")
	_ = st.InTx(ctx, func(r store.Repos) error {
		if err := r.PowerDesk().UpsertMember(ctx, &powerdesk.Member{
			OrganisationID: org.ID,
			UserID:         mgrUID,
			Role:           powerdesk.RoleManager,
			CreatedAt:      now.Add(time.Second),
		}); err != nil {
			return err
		}
		return abortErr
	})
	if _, err := st.Repos().PowerDesk().GetActiveMemberRole(ctx, org.ID, mgrUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled-back member must not exist, got %v", err)
	}

	// Add manager & viewer, update role, remove & re-add viewer.
	if err := st.Repos().PowerDesk().UpsertMember(ctx, &powerdesk.Member{
		OrganisationID: org.ID, UserID: mgrUID, Role: powerdesk.RoleManager, CreatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().PowerDesk().UpsertMember(ctx, &powerdesk.Member{
		OrganisationID: org.ID, UserID: viewerUID, Role: powerdesk.RoleViewer, CreatedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if cnt, err := st.Repos().PowerDesk().CountActiveOwners(ctx, org.ID); err != nil || cnt != 1 {
		t.Fatalf("expected 1 active owner, got %d (%v)", cnt, err)
	}
	if err := st.Repos().PowerDesk().RemoveMember(ctx, org.ID, viewerUID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Repos().PowerDesk().GetActiveMemberRole(ctx, org.ID, viewerUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after RemoveMember, got %v", err)
	}
	// Re-adding removed member restores ACTIVE status.
	if err := st.Repos().PowerDesk().UpsertMember(ctx, &powerdesk.Member{
		OrganisationID: org.ID, UserID: viewerUID, Role: powerdesk.RoleViewer, CreatedAt: now.Add(3 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	members, err := st.Repos().PowerDesk().ListActiveMembers(ctx, org.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("expected 3 active members, got %d (%v)", len(members), err)
	}

	// Properties, default building, units and duplicate unit protection.
	prop := &powerdesk.Property{
		ID:             uuid.New(),
		OrganisationID: org.ID,
		OwnerUserID:    ownerUID,
		Name:           "Block A",
		Address:        "12 Admiralty Way",
		City:           "Lagos",
		State:          "LA",
		Country:        "NG",
		CreatedAt:      now,
	}
	if err := st.Repos().PowerDesk().CreateProperty(ctx, prop); err != nil {
		t.Fatal(err)
	}
	bld, err := st.Repos().PowerDesk().EnsureDefaultBuilding(ctx, prop.ID, "", now)
	if err != nil {
		t.Fatal(err)
	}
	bld2, err := st.Repos().PowerDesk().EnsureDefaultBuilding(ctx, prop.ID, "main building", now)
	if err != nil || bld2.ID != bld.ID {
		t.Fatalf("EnsureDefaultBuilding must be case-insensitively idempotent: got %v / %v", bld2, err)
	}

	u1 := &powerdesk.Unit{ID: uuid.New(), BuildingID: bld.ID, UnitName: "Flat 1A", OccupancyStatus: "OCCUPIED", CreatedAt: now}
	if err := st.Repos().PowerDesk().CreateUnit(ctx, u1); err != nil {
		t.Fatal(err)
	}
	if u1.OrganisationID != org.ID || u1.PropertyID != prop.ID {
		t.Fatalf("CreateUnit did not populate org/property: %+v", u1)
	}
	dupUnit := &powerdesk.Unit{ID: uuid.New(), BuildingID: bld.ID, UnitName: "flat 1a", CreatedAt: now}
	if err := st.Repos().PowerDesk().CreateUnit(ctx, dupUnit); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate unit_name in building should fail with ErrDuplicate, got %v", err)
	}

	// Attach meter to unit and verify via operator.
	meterID := uuid.New()
	ctMeter, _ := box.Encrypt("75012345678")
	if err := st.Repos().Meters().Insert(ctx, &meters.Meter{
		ID: meterID, NumberEncrypted: ctMeter, KeyVersion: box.KeyVersion,
		NumberHash: box.Hash("IKEDC:75012345678:" + uuid.NewString()), Masked: "*******5678",
		DiscoCode: "IKEDC", MeterType: "PREPAID", CustomerName: "LEKKI PHASE 1 COURTS LTD", ValidatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	um := &powerdesk.UnitMeter{
		ID:               uuid.New(),
		UnitID:           u1.ID,
		MeterID:          meterID,
		DiscoAccountName: "LEKKI PHASE 1 COURTS LTD",
		CreatedByUserID:  mgrUID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := st.Repos().PowerDesk().AttachUnitMeter(ctx, um); err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().PowerDesk().AttachUnitMeter(ctx, &powerdesk.UnitMeter{
		UnitID: u1.ID, MeterID: meterID, CreatedByUserID: mgrUID, CreatedAt: now,
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second active meter on same unit must return ErrDuplicate, got %v", err)
	}
	verifiedAt := now.Add(5 * time.Minute)
	um.Verified = true
	um.VerificationMethod = powerdesk.VerifyAccountNameMatch
	um.VerifiedByUserID = opUID
	um.VerifiedAt = &verifiedAt
	um.VerificationReason = "Matched DisCo customer name to RC-882910"
	um.UpdatedAt = verifiedAt
	if err := st.Repos().PowerDesk().UpdateUnitMeter(ctx, um); err != nil {
		t.Fatal(err)
	}
	gotUM, err := st.Repos().PowerDesk().GetActiveUnitMeterByUnit(ctx, u1.ID)
	if err != nil || !gotUM.Verified || gotUM.VerificationMethod != powerdesk.VerifyAccountNameMatch {
		t.Fatalf("unexpected verified unit_meter: %+v (%v)", gotUM, err)
	}

	// PENDING_APPROVAL LANDLORD_BULK pot + BulkTopup + BulkTopupItem + ClaimDue skip check.
	code, _ := pots.GenerateCode()
	codeHMAC, _ := pots.BulkCodeHMAC(box, code)
	pot := &pots.Pot{
		ID:          uuid.New(),
		Kind:        pots.KindLandlordBulk,
		OwnerID:     mgrUID,
		TargetMinor: 2_510_000,
		FeeMinor:    10_000,
		Currency:    money.NGN,
		Status:      pots.StatusPendingApproval,
		CodeHMAC:    codeHMAC,
		Deadline:    now.Add(72 * time.Hour),
		NextCheckAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	bt := &powerdesk.BulkTopup{
		PotID:                 pot.ID,
		OrganisationID:        org.ID,
		CreatedByUserID:       mgrUID,
		CreatorRole:           powerdesk.RoleManager,
		ApprovalStatus:        powerdesk.ApprovalPending,
		ElectricityTotalMinor: 2_500_000,
		FeeTotalMinor:         10_000,
		TotalMinor:            2_510_000,
		Currency:              money.NGN,
		IdempotencyKey:        "bulk-nov-2026",
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	item := &powerdesk.BulkTopupItem{
		ID:             uuid.New(),
		PotID:          pot.ID,
		OrganisationID: org.ID,
		PropertyID:     prop.ID,
		UnitID:         u1.ID,
		UnitMeterID:    um.ID,
		MeterID:        meterID,
		AmountMinor:    2_500_000,
		FeeMinor:       10_000,
		TotalMinor:     2_510_000,
		Currency:       money.NGN,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := st.InTx(ctx, func(r store.Repos) error {
		if err := r.Pots().Create(ctx, pot); err != nil {
			return err
		}
		return r.PowerDesk().CreateBulkTopup(ctx, bt, []*powerdesk.BulkTopupItem{item})
	}); err != nil {
		t.Fatalf("create PENDING_APPROVAL pot + bulk topup: %v", err)
	}

	// ClaimDue must NOT claim PENDING_APPROVAL pots even when stale.
	due, err := st.Repos().Pots().ClaimDue(ctx, now.Add(time.Hour), now.Add(30*time.Minute), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range due {
		if id == pot.ID {
			t.Fatal("ClaimDue must never claim PENDING_APPROVAL pots")
		}
	}

	gotBT, err := st.Repos().PowerDesk().GetBulkTopupByIdempotency(ctx, org.ID, "bulk-nov-2026")
	if err != nil || gotBT.PotID != pot.ID || gotBT.ApprovalStatus != powerdesk.ApprovalPending {
		t.Fatalf("unexpected GetBulkTopupByIdempotency: %+v (%v)", gotBT, err)
	}
	items, err := st.Repos().PowerDesk().ListBulkTopupItemsForOrg(ctx, org.ID, &prop.ID)
	if err != nil || len(items) != 1 || items[0].UnitID != u1.ID {
		t.Fatalf("unexpected ListBulkTopupItemsForOrg: %+v (%v)", items, err)
	}
}
