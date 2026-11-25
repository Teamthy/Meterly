package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestAutoPowerRepoParity(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) store.Store
	}{
		{name: "memstore", open: func(*testing.T) store.Store { return memstore.New() }},
		{name: "pgstore", open: func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runAutoPowerRepoParity(t, tc.open(t))
		})
	}
}

func runAutoPowerRepoParity(t *testing.T, st store.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}

	userID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|ap-user-"+uuid.NewString())
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

	// Seed a paid source transaction for saving the card.
	q := &tx.Quote{
		ID: uuid.New(), CustomerID: userID, MeterID: meterID,
		Amount: money.New(500_000, money.NGN), Fee: money.New(10_000, money.NGN), Total: money.New(510_000, money.NGN),
		DeliveryMethod: tx.MethodToken, DeliveryOptions: []tx.DeliveryMethod{tx.MethodToken},
		ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now, Used: true,
	}
	if err := st.Repos().Quotes().Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	srcTxn := tx.New(userID, q, false, "", now)
	srcTxn.PaymentProviderName = "paystack"
	srcTxn.PaymentProviderRef = "ps_seed_" + uuid.NewString()
	_ = srcTxn.SetPayment(tx.PayPending, "init")
	_ = srcTxn.SetPayment(tx.PayConfirmed, "paid")
	if err := st.Repos().Transactions().Create(ctx, srcTxn); err != nil {
		t.Fatal(err)
	}

	pmID := uuid.New()
	authCT, err := autopower.SealAuthCode(box, userID, pmID, "AUTH_reusable_1")
	if err != nil {
		t.Fatal(err)
	}
	emailCT, err := autopower.SealAuthEmail(box, userID, pmID, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	pm := &autopower.SavedPaymentMethod{
		ID:                  pmID,
		UserID:              userID,
		Provider:            "paystack",
		SourceTransactionID: srcTxn.ID,
		AuthCodeCiphertext:  authCT,
		EmailCiphertext:     emailCT,
		KeyVersion:          box.KeyVersion,
		CardBrand:           "visa",
		Last4:               "4081",
		ExpMonth:            12,
		ExpYear:             2030,
		Signature:           "SIG_1",
		ConsentVersion:      autopower.DefaultConsentVersion,
		ConsentText:         autopower.DefaultConsentTextV1,
		ConsentedAt:         now,
		CreatedAt:           now,
	}
	if err := st.Repos().AutoPower().CreatePaymentMethod(ctx, pm); err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	// Duplicate active save for same source txn must return ErrDuplicate.
	dupPM := *pm
	dupPM.ID = uuid.New()
	if err := st.Repos().AutoPower().CreatePaymentMethod(ctx, &dupPM); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate on duplicate source_transaction_id, got %v", err)
	}

	nextSlot := time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC)
	rule := &autopower.Rule{
		ID:                 uuid.New(),
		UserID:             userID,
		MeterID:            meterID,
		PaymentMethodID:    pm.ID,
		TriggerType:        autopower.TriggerSchedule,
		AmountMinor:        1_000_000,
		MonthlyCapMinor:    4_000_000,
		Currency:           "NGN",
		DeliveryPreference: "AUTO",
		Frequency:          autopower.FreqMonthly,
		DaysOfMonth:        []int{1, 15},
		TimeOfDay:          "07:00",
		Timezone:           autopower.DefaultTimezone,
		Status:             autopower.RuleActive,
		NextScheduledAt:    &nextSlot,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := st.Repos().AutoPower().CreateRule(ctx, rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Claim rule for materialisation (SKIP LOCKED lease prevents immediate double claim).
	claimedRules, err := st.Repos().AutoPower().ClaimDueRulesForMaterialize(ctx, nextSlot, now, time.Minute, 10)
	if err != nil || len(claimedRules) != 1 || claimedRules[0] != rule.ID {
		t.Fatalf("claim due rules: got %v err=%v", claimedRules, err)
	}
	claimedAgain, err := st.Repos().AutoPower().ClaimDueRulesForMaterialize(ctx, nextSlot, now, time.Minute, 10)
	if err != nil || len(claimedAgain) != 0 {
		t.Fatalf("second claim within lease must return empty, got %v err=%v", claimedAgain, err)
	}

	// Unique (rule_id, scheduled_for) slot enforcement.
	run1 := &autopower.Run{
		ID:            uuid.New(),
		RuleID:        rule.ID,
		UserID:        userID,
		MeterID:       meterID,
		TriggerSource: autopower.SourceSchedule,
		ScheduledFor:  nextSlot,
		AmountMinor:   1_000_000,
		Currency:      "NGN",
		Status:        autopower.RunScheduled,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := st.Repos().AutoPower().CreateRun(ctx, run1); err != nil {
		t.Fatalf("create run: %v", err)
	}
	dupRun := *run1
	dupRun.ID = uuid.New()
	if err := st.Repos().AutoPower().CreateRun(ctx, &dupRun); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate on duplicate (rule_id, scheduled_for), got %v", err)
	}

	// Spend guard sums count CHARGING and SUCCEEDED within [startUTC, endUTC), excluding FAILED/REFUNDED/SKIPPED.
	execAt := nextSlot
	run1.Status = autopower.RunCharging
	run1.ExecutedAt = &execAt
	run1.TransactionID = &srcTxn.ID
	run1.UpdatedAt = nextSlot
	if err := st.Repos().AutoPower().UpdateRun(ctx, run1); err != nil {
		t.Fatalf("update run: %v", err)
	}

	mStart, mEnd := autopower.MonthBoundsUTC(nextSlot, autopower.LagosLocation())
	ruleSpend, err := st.Repos().AutoPower().SumRuleSpendMinor(ctx, rule.ID, mStart, mEnd)
	if err != nil || ruleSpend != 1_000_000 {
		t.Fatalf("rule spend sum: got %d err=%v", ruleSpend, err)
	}
	userSpend, err := st.Repos().AutoPower().SumUserSpendMinor(ctx, userID, mStart, mEnd)
	if err != nil || userSpend != 1_000_000 {
		t.Fatalf("user spend sum: got %d err=%v", userSpend, err)
	}

	// Account-level notification delivery (TransactionID = uuid.Nil, UserID = userID).
	deliv := &notifications.Delivery{
		EventID:         uuid.New(),
		TransactionID:   uuid.Nil,
		UserID:          userID,
		RecipientKind:   notifications.RecipientPurchaser,
		RecipientSlot:   run1.ID.String(),
		Channel:         notifications.SMS,
		DestinationHMAC: box.Hash("dest:+2348012345678"),
		TemplateID:      "autopower_heads_up",
		TemplateVersion: "v1",
		Locale:          notifications.LocaleEN,
		Status:          notifications.StatusPending,
		NextAttemptAt:   now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	inserted, err := st.Repos().Notifications().CreateDelivery(ctx, deliv)
	if err != nil || !inserted {
		t.Fatalf("create account-level delivery: inserted=%v err=%v", inserted, err)
	}
	loadedDeliv, err := st.Repos().Notifications().GetDelivery(ctx, deliv.ID)
	if err != nil || loadedDeliv.UserID != userID || loadedDeliv.TransactionID != uuid.Nil {
		t.Fatalf("loaded delivery mismatch: %+v err=%v", loadedDeliv, err)
	}

	// User row lock inside InTx.
	if err := st.InTx(ctx, func(r store.Repos) error {
		return r.Users().LockForUpdate(ctx, userID)
	}); err != nil {
		t.Fatalf("user LockForUpdate: %v", err)
	}
}
