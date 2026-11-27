package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestPayRequestRepoRoundTripAndConstraints(t *testing.T) {
	cases := []struct {
		name string
		open func(t *testing.T) store.Store
	}{
		{name: "memstore", open: func(*testing.T) store.Store { return memstore.New() }},
		{name: "pgstore", open: func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "v1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runPayRequestRepoRoundTrip(t, tc.open(t))
		})
	}
}

func runPayRequestRepoRoundTrip(t *testing.T, st store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}

	requesterID, err := st.Repos().Users().EnsureByExternalID(ctx, "auth0|m7-req-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	meterID := uuid.New()
	ctMeter, err := box.Encrypt("45012345678")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().Meters().Insert(ctx, &meters.Meter{
		ID:              meterID,
		NumberEncrypted: ctMeter,
		KeyVersion:      box.KeyVersion,
		NumberHash:      box.Hash("meter:IKEDC:" + uuid.NewString()),
		Masked:          "*******5678",
		DiscoCode:       "IKEDC",
		MeterType:       "PREPAID",
		CustomerName:    "A. Okafor",
		Address:         "14 Marina Rd",
		ValidatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}

	code, err := payrequests.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	codeHMAC, _ := payrequests.LinkCodeHMAC(box, code)
	reqID := uuid.New()
	noteCT, noteKV, err := payrequests.SealNote(box, reqID, "Light don go, abeg 🙏")
	if err != nil {
		t.Fatal(err)
	}

	pr := &payrequests.PaymentRequest{
		ID:             reqID,
		RequesterID:    requesterID,
		MeterID:        meterID,
		AmountMode:     payrequests.ModeRange,
		MinAmountMinor: 200_000,
		MaxAmountMinor: 1_000_000,
		Currency:       money.NGN,
		NoteCiphertext: noteCT,
		NoteKeyVersion: noteKV,
		Status:         payrequests.StatusOpen,
		MaxPayments:    1,
		PaymentsCount:  0,
		CodeHMAC:       codeHMAC,
		ExpiresAt:      now.Add(72 * time.Hour),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// Verify rollback inside InTx leaves no trace.
	errBoom := errors.New("boom")
	if err := st.InTx(ctx, func(r store.Repos) error {
		if err := r.PayRequests().Create(ctx, pr); err != nil {
			return err
		}
		return errBoom
	}); !errors.Is(err, errBoom) {
		t.Fatalf("want boom, got %v", err)
	}
	if _, err := st.Repos().PayRequests().GetByCodeHMAC(ctx, codeHMAC); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected rollback, got %v", err)
	}

	if err := st.Repos().PayRequests().Create(ctx, pr); err != nil {
		t.Fatalf("create payment request: %v", err)
	}

	// Duplicate code_hmac is rejected.
	dup := *pr
	dup.ID = uuid.New()
	if err := st.Repos().PayRequests().Create(ctx, &dup); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate on code_hmac, got %v", err)
	}

	got, err := st.Repos().PayRequests().GetByCodeHMAC(ctx, codeHMAC)
	if err != nil {
		t.Fatalf("GetByCodeHMAC: %v", err)
	}
	note, err := payrequests.OpenNote(box, got.ID, got.NoteCiphertext)
	if err != nil || note != "Light don go, abeg 🙏" {
		t.Fatalf("OpenNote: %q %v", note, err)
	}

	// Create quote + transaction + guest payer row.
	amt := money.New(500_000, money.NGN)
	q := &tx.Quote{
		ID:              uuid.New(),
		CustomerID:      requesterID,
		MeterID:         meterID,
		Amount:          amt,
		Fee:             money.New(0, money.NGN),
		Total:           amt,
		DeliveryMethod:  tx.MethodToken,
		DeliveryOptions: []tx.DeliveryMethod{tx.MethodToken},
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	}
	txn := tx.New(requesterID, q, false, "", now)
	statusCode, _ := payrequests.GenerateCode()
	statusHMAC, _ := payrequests.StatusCodeHMAC(box, statusCode)
	emailCT, emailKV, _ := payrequests.SealPayerEmail(box, txn.ID, "guest@example.com")
	nameCT, nameKV, _ := payrequests.SealPayerName(box, txn.ID, "Tunde")
	scCT, scKV, _ := payrequests.SealStatusCode(box, txn.ID, statusCode)

	if err := st.InTx(ctx, func(r store.Repos) error {
		locked, err := r.PayRequests().GetByCodeHMACForUpdate(ctx, codeHMAC)
		if err != nil {
			return err
		}
		locked.PaymentsCount = 1
		locked.Status = payrequests.StatusPaid
		locked.UpdatedAt = now.Add(time.Second)
		if err := r.PayRequests().Update(ctx, locked); err != nil {
			return err
		}
		if err := r.Quotes().Create(ctx, q); err != nil {
			return err
		}
		if err := r.Transactions().Create(ctx, txn); err != nil {
			return err
		}
		return r.PayRequests().CreatePayer(ctx, payrequests.Payer{
			TransactionID:        txn.ID,
			PaymentRequestID:     reqID,
			EmailCiphertext:      emailCT,
			EmailKeyVersion:      emailKV,
			PayerNameCiphertext:  nameCT,
			PayerNameKeyVersion:  nameKV,
			StatusCodeHMAC:       statusHMAC,
			StatusCodeCiphertext: scCT,
			StatusCodeKeyVersion: scKV,
			CreatedAt:            now,
		})
	}); err != nil {
		t.Fatalf("atomic slot claim + payer insert: %v", err)
	}

	payer, err := st.Repos().PayRequests().GetPayerByStatusHMAC(ctx, statusHMAC)
	if err != nil {
		t.Fatalf("GetPayerByStatusHMAC: %v", err)
	}
	if payer.TransactionID != txn.ID || payer.SlotReleased {
		t.Fatalf("unexpected payer: %+v", payer)
	}
	if err := st.Repos().PayRequests().MarkPayerSlotReleased(ctx, txn.ID); err != nil {
		t.Fatalf("MarkPayerSlotReleased: %v", err)
	}
	payer2, err := st.Repos().PayRequests().GetPayer(ctx, txn.ID)
	if err != nil || !payer2.SlotReleased {
		t.Fatalf("expected SlotReleased=true, got %+v %v", payer2, err)
	}
}
