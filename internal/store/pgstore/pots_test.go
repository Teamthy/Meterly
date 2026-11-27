package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestPotRepoLedgerAndFundingSourceParity(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) store.Store
	}{
		{name: "memstore", open: func(*testing.T) store.Store { return memstore.New() }},
		{name: "pgstore", open: func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "v1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runPotRepoParity(t, tc.open(t))
		})
	}
}

func runPotRepoParity(t *testing.T, st store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}

	ownerID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|pot-owner-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	meterID := uuid.New()
	ctMeter, _ := box.Encrypt("45012345678")
	if err := st.Repos().Meters().Insert(ctx, &meters.Meter{
		ID: meterID, NumberEncrypted: ctMeter, KeyVersion: box.KeyVersion,
		NumberHash: box.Hash("meter:IKEDC:" + uuid.NewString()), Masked: "*******5678",
		DiscoCode: "IKEDC", MeterType: "PREPAID", ValidatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	code, _ := pots.GenerateCode()
	codeHMAC, _ := pots.CodeHMAC(box, code)
	potID := uuid.New()
	pot := &pots.Pot{
		ID:              potID,
		Kind:            pots.KindSplitMeter,
		OwnerID:         ownerID,
		MeterID:         meterID,
		TargetMinor:     2_000_000,
		FeeMinor:        10_000,
		Currency:        money.NGN,
		SuggestedShares: 4,
		Status:          pots.StatusOpen,
		CodeHMAC:        codeHMAC,
		Deadline:        now.Add(24 * time.Hour),
		NextCheckAt:     now.Add(24 * time.Hour),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Rollback check.
	errBoom := errors.New("boom")
	if err := st.InTx(ctx, func(r store.Repos) error {
		if err := r.Pots().Create(ctx, pot); err != nil {
			return err
		}
		return errBoom
	}); !errors.Is(err, errBoom) {
		t.Fatalf("want boom, got %v", err)
	}
	if _, err := st.Repos().Pots().Get(ctx, potID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected rollback, got %v", err)
	}

	if err := st.Repos().Pots().Create(ctx, pot); err != nil {
		t.Fatalf("Create pot: %v", err)
	}

	// Verify POT_FUNDS_HELD ledger posting + POT-funded transaction + allocation round-trip.
	contribID := uuid.New()
	emCT, emKV, _ := pots.SealContributorEmail(box, contribID, "c1@example.com")
	contrib := &pots.Contribution{
		ID:                  contribID,
		PotID:               potID,
		EmailCiphertext:     emCT,
		EmailKeyVersion:     emKV,
		Initials:            "T.O.",
		AmountMinor:         2_000_000,
		RetainedMinor:       2_000_000,
		Currency:            money.NGN,
		Status:              pots.ContribPaid,
		PaymentProviderName: "paystack",
		PaymentProviderRef:  "ps_ref_" + uuid.NewString(),
		PaidAt:              &now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	q := &tx.Quote{
		ID:              uuid.New(),
		CustomerID:      ownerID,
		MeterID:         meterID,
		Amount:          money.New(1_990_000, money.NGN),
		Fee:             money.New(10_000, money.NGN),
		Total:           money.New(2_000_000, money.NGN),
		DeliveryMethod:  tx.MethodToken,
		DeliveryOptions: []tx.DeliveryMethod{tx.MethodToken},
		Used:            true,
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	}
	potTxn := tx.NewPotFunded(ownerID, q, now)
	alloc := &pots.Allocation{
		ID:            uuid.New(),
		PotID:         potID,
		TransactionID: potTxn.ID,
		MeterID:       meterID,
		AmountMinor:   2_000_000,
		Currency:      money.NGN,
		Status:        pots.AllocActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := st.InTx(ctx, func(r store.Repos) error {
		if err := r.Pots().CreateContribution(ctx, contrib); err != nil {
			return err
		}
		if _, err := r.Ledger().Post(ctx, ledger.Transaction{
			Reference:  "pot-contrib:" + contrib.ID.String(),
			SourceType: "pot_contribution",
			SourceID:   contrib.ID,
			Entries: []ledger.Entry{
				{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: money.New(2_000_000, money.NGN)},
				{Account: ledger.PotFundsHeld, Direction: ledger.Credit, Amount: money.New(2_000_000, money.NGN)},
			},
		}); err != nil {
			return err
		}
		if err := r.Quotes().Create(ctx, q); err != nil {
			return err
		}
		if err := r.Transactions().Create(ctx, potTxn); err != nil {
			return err
		}
		if err := r.Pots().CreateAllocation(ctx, alloc); err != nil {
			return err
		}
		_, err := r.Ledger().Post(ctx, ledger.Transaction{
			Reference:  "pot-alloc:" + alloc.ID.String(),
			SourceType: "pot_allocation",
			SourceID:   alloc.ID,
			Entries: []ledger.Entry{
				{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: money.New(2_000_000, money.NGN)},
				{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: money.New(2_000_000, money.NGN)},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("atomic pot contribution + allocation: %v", err)
	}

	loadedTxn, err := st.Repos().Transactions().Get(ctx, potTxn.ID)
	if err != nil {
		t.Fatalf("load POT-funded txn: %v", err)
	}
	if loadedTxn.FundingSource != tx.FundingPot || loadedTxn.Payment != tx.PayConfirmed {
		t.Fatalf("unexpected POT-funded txn: %+v", loadedTxn)
	}

	bal, err := st.Repos().Ledger().Balance(ctx, ledger.PotFundsHeld, money.NGN)
	if err != nil || bal != 0 {
		t.Fatalf("POT_FUNDS_HELD balance = %d (err=%v), want 0", bal, err)
	}
	tb, err := st.Repos().Ledger().TrialBalance(ctx)
	if err != nil || tb != 0 {
		t.Fatalf("TrialBalance = %d (err=%v), want 0", tb, err)
	}
}
