package privacy_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/dataexport"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/erasure"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func seedErasureUser(t *testing.T, st store.Store, ext string) (uid string, txnID uuid.UUID, amount int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	uid, err := st.Repos().Users().EnsureByExternalID(ctx, ext)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
		UserID: uid, EmailCiphertext: []byte("email-ct"), EmailHMAC: "hmac-" + ext,
		EmailMasked: "a***@x.com", EmailKeyVersion: "t1",
		PhoneCiphertext: []byte("phone-ct"), PhoneHMAC: "phmac-" + ext,
		PhoneMasked: "+234***89", PhoneKeyVersion: "t1", DisplayName: "Ada",
	}); err != nil {
		t.Fatal(err)
	}
	pub := make([]byte, 64)
	if err := st.Repos().Auth().InsertCredential(ctx, identity.Credential{
		ID: uuid.New(), UserID: uid, CredentialID: []byte("cred-" + ext), PublicKey: pub,
		Name: "laptop", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	meterID := uuid.New()
	ct, _ := box.Encrypt("45012345678")
	if err := st.Repos().Meters().Insert(ctx, &meters.Meter{
		ID: meterID, NumberEncrypted: ct, KeyVersion: box.KeyVersion,
		NumberHash: box.Hash("meter:IKEDC:" + uuid.NewString()), Masked: "*******5678",
		DiscoCode: "IKEDC", MeterType: "PREPAID", ValidatedAt: now, CustomerName: "Ada Oni", Address: "12 Broad St",
	}); err != nil {
		t.Fatal(err)
	}
	q := &tx.Quote{
		ID: uuid.New(), CustomerID: uid, MeterID: meterID,
		Amount: money.New(500_000, money.NGN), Fee: money.New(10_000, money.NGN), Total: money.New(510_000, money.NGN),
		DeliveryMethod: tx.MethodToken, DeliveryOptions: []tx.DeliveryMethod{tx.MethodToken},
		ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now, Used: true,
	}
	if err := st.Repos().Quotes().Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	txn := tx.New(uid, q, false, "", now)
	_ = txn.SetPayment(tx.PayPending, "init")
	_ = txn.SetPayment(tx.PayConfirmed, "paid")
	_ = txn.SetVending(tx.VendPending, "vend")
	_ = txn.SetVending(tx.VendConfirmed, "ok")
	_ = txn.SetDelivery(tx.DelPending, "send")
	_ = txn.SetDelivery(tx.DelConfirmed, "done")
	if err := st.Repos().Transactions().Create(ctx, txn); err != nil {
		t.Fatal(err)
	}
	amount = 510_000
	if _, err := st.Repos().Ledger().Post(ctx, ledger.Transaction{
		ID: uuid.New(), Reference: "erase-" + txn.ID.String(), SourceType: "txn", SourceID: txn.ID,
		PostedAt: now, Entries: []ledger.Entry{
			{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: money.New(amount, money.NGN)},
			{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: money.New(amount, money.NGN)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return uid, txn.ID, amount
}

func TestErasureKeepsBooksWipesIdentityAndResumes(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		uid, txnID, amount := seedErasureUser(t, st, "erase-keep-books")
		tb0, err := st.Repos().Ledger().TrialBalance(ctx)
		if err != nil || tb0 != 0 {
			t.Fatalf("tb before %d err=%v", tb0, err)
		}
		psp0, _ := st.Repos().Ledger().Balance(ctx, ledger.PSPSettlementReceivable, money.NGN)

		job := privacy.ErasureRequest{
			ID: uuid.New(), UserID: uid, Status: privacy.ErasureReady,
			DueAt: now.Add(-time.Minute), CreatedAt: now, UpdatedAt: now,
		}
		if err := st.Repos().Privacy().CreateErasure(ctx, job); err != nil {
			t.Fatal(err)
		}
		n, err := erasure.Process(ctx, st, now, 10, log)
		if err != nil || n != 1 {
			t.Fatalf("process n=%d err=%v", n, err)
		}
		got, err := st.Repos().Transactions().Get(ctx, txnID)
		if err != nil {
			t.Fatal(err)
		}
		if got.CustomerID != privacy.TombstoneUserID.String() {
			t.Fatalf("customer %s want tombstone", got.CustomerID)
		}
		if got.Total.Minor != amount {
			t.Fatalf("amount changed %d", got.Total.Minor)
		}
		tb1, _ := st.Repos().Ledger().TrialBalance(ctx)
		psp1, _ := st.Repos().Ledger().Balance(ctx, ledger.PSPSettlementReceivable, money.NGN)
		if tb1 != 0 || psp1 != psp0 {
			t.Fatalf("books moved tb=%d psp %d→%d", tb1, psp0, psp1)
		}
		stt, err := st.Repos().Users().Status(ctx, uid)
		if err != nil || stt != store.UserErased {
			t.Fatalf("status %s err=%v", stt, err)
		}
		c, err := st.Repos().Users().Contacts(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if c.EmailHMAC != "" || c.PhoneHMAC != "" || len(c.EmailCiphertext) != 0 || c.DisplayName != "" {
			t.Fatalf("identity remains %+v", c)
		}
		creds, _ := st.Repos().Auth().ListCredentials(ctx, uid)
		if len(creds) != 0 {
			t.Fatalf("passkeys remain %d", len(creds))
		}
		n, err = erasure.Process(ctx, st, now, 10, log)
		if err != nil || n != 0 {
			t.Fatalf("re-run n=%d err=%v", n, err)
		}
		box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
		exp := privacy.ExportJob{
			ID: uuid.New(), UserID: uid, Day: privacy.ExportDay(now),
			Status: privacy.ExportPending, TokenHMAC: "hmac-erased-export",
			ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
		}
		if err := st.Repos().Privacy().CreateExport(ctx, exp); err != nil {
			t.Fatal(err)
		}
		if _, err := dataexport.Process(ctx, st, box, now, 10, log); err != nil {
			t.Fatal(err)
		}
		gotExp, err := st.Repos().Privacy().GetExport(ctx, exp.ID)
		if err != nil || gotExp.Status != privacy.ExportFailed || gotExp.Error != "account_erased" {
			t.Fatalf("export of erased user %+v err=%v", gotExp, err)
		}

		uid2, err := st.Repos().Users().EnsureByExternalID(ctx, "erase-reregister")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
			UserID: uid2, EmailCiphertext: []byte("new"), EmailHMAC: "hmac-erase-keep-books",
			EmailMasked: "a***@x.com", EmailKeyVersion: "t1",
			PhoneCiphertext: []byte("newp"), PhoneHMAC: "phmac-erase-keep-books",
			PhoneMasked: "+234***89", PhoneKeyVersion: "t1",
		}); err != nil {
			t.Fatal(err)
		}
		c2, _ := st.Repos().Users().Contacts(ctx, uid2)
		if c2.EmailHMAC != "hmac-erase-keep-books" {
			t.Fatalf("re-register hmac %s", c2.EmailHMAC)
		}

		audits, err := st.Repos().Audit().ForResource(ctx, "erasure_request", job.ID)
		if err != nil || len(audits) == 0 {
			t.Fatalf("audit n=%d err=%v", len(audits), err)
		}
		raw, _ := json.Marshal(audits)
		if strings.Contains(string(raw), "a***@x.com") || strings.Contains(string(raw), "Ada") || strings.Contains(string(raw), "hmac-erase") {
			t.Fatalf("PII in audit %s", raw)
		}
	})
}

func TestErasureCancelAndResume(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		uid, txnID, _ := seedErasureUser(t, st, "erase-cancel")
		job := privacy.ErasureRequest{
			ID: uuid.New(), UserID: uid, Status: privacy.ErasureCoolingOff,
			DueAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
		}
		if err := st.Repos().Privacy().CreateErasure(ctx, job); err != nil {
			t.Fatal(err)
		}
		tcan := now
		job.Status = privacy.ErasureCancelled
		job.CancelledAt = &tcan
		job.UpdatedAt = now
		if err := st.Repos().Privacy().UpdateErasure(ctx, job); err != nil {
			t.Fatal(err)
		}
		n, err := erasure.Process(ctx, st, now.Add(2*time.Hour), 10, log)
		if err != nil || n != 0 {
			t.Fatalf("cancelled process n=%d err=%v", n, err)
		}
		stt, _ := st.Repos().Users().Status(ctx, uid)
		if stt != "" && stt != store.UserActive {
			t.Fatalf("cancelled user status %s", stt)
		}
		got, _ := st.Repos().Transactions().Get(ctx, txnID)
		if got.CustomerID != uid {
			t.Fatalf("cancelled still re-pointed")
		}

		uid2, txn2, _ := seedErasureUser(t, st, "erase-resume")
		job2 := privacy.ErasureRequest{
			ID: uuid.New(), UserID: uid2, Status: privacy.ErasureRunning,
			DueAt: now.Add(-time.Minute), Step: privacy.ErasureStepFinancial,
			CreatedAt: now, UpdatedAt: now,
		}
		tomb, err := st.Repos().Privacy().EnsureTombstone(ctx)
		if err != nil {
			t.Fatal(err)
		}
		job2.TombstoneID = tomb
		if err := st.Repos().Privacy().ApplyErasureStep(ctx, uid2, tomb, privacy.ErasureStepFinancial, now); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Privacy().CreateErasure(ctx, job2); err != nil {
			t.Fatal(err)
		}
		n, err = erasure.Process(ctx, st, now, 10, log)
		if err != nil || n != 1 {
			t.Fatalf("resume n=%d err=%v", n, err)
		}
		got2, _ := st.Repos().Transactions().Get(ctx, txn2)
		if got2.CustomerID != privacy.TombstoneUserID.String() {
			t.Fatalf("resume txn customer %s", got2.CustomerID)
		}
		c, _ := st.Repos().Users().Contacts(ctx, uid2)
		if c.EmailHMAC != "" {
			t.Fatalf("resume left identity %+v", c)
		}
	})
}

func TestErasureBlockedConditions(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Now().UTC()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))

		runBlocked := func(name, want string, setup func(uid string, txnID, meterID uuid.UUID)) {
			t.Helper()
			uid, txnID, _ := seedErasureUser(t, st, "erase-block-"+name)
			got, _ := st.Repos().Transactions().Get(ctx, txnID)
			setup(uid, txnID, got.MeterID)
			job := privacy.ErasureRequest{
				ID: uuid.New(), UserID: uid, Status: privacy.ErasureReady,
				DueAt: now.Add(-time.Minute), CreatedAt: now, UpdatedAt: now,
			}
			if err := st.Repos().Privacy().CreateErasure(ctx, job); err != nil {
				t.Fatal(err)
			}
			n, err := erasure.Process(ctx, st, now, 10, log)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("%s processed %d jobs", name, n)
			}
			gotJob, err := st.Repos().Privacy().GetErasure(ctx, job.ID)
			if err != nil || gotJob.Status != privacy.ErasureBlocked || gotJob.Error != want {
				t.Fatalf("%s got %+v err=%v want BLOCKED %s", name, gotJob, err, want)
			}
			stt, _ := st.Repos().Users().Status(ctx, uid)
			if stt == store.UserErased {
				t.Fatalf("%s erased despite block", name)
			}
		}

		runBlocked("dispute", privacy.BlockOpenDispute, func(uid string, txnID, _ uuid.UUID) {
			if _, err := st.Repos().Disputes().Upsert(ctx, disputes.Dispute{
				ID: uuid.New(), PSPDisputeID: "psp-" + uuid.NewString(), TransactionID: txnID,
				Status: disputes.StatusOpen, Amount: money.New(100, money.NGN), CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
		})
		runBlocked("review", privacy.BlockUnresolvedTxn, func(uid string, txnID, _ uuid.UUID) {
			txn, err := st.Repos().Transactions().Get(ctx, txnID)
			if err != nil {
				t.Fatal(err)
			}
			txn.FlagForReview("manual")
			if err := st.Repos().Transactions().Save(ctx, txn); err != nil {
				t.Fatal(err)
			}
		})
		runBlocked("pot", privacy.BlockPotWithFunds, func(uid string, _ uuid.UUID, meterID uuid.UUID) {
			p := &pots.Pot{
				ID: uuid.New(), Kind: pots.KindSplitMeter, OwnerID: uid, MeterID: meterID,
				TargetMinor: 1000, Currency: money.NGN, Status: pots.StatusFunded,
				CodeHMAC: "pot-" + uuid.NewString(), Deadline: now.Add(time.Hour),
				NextCheckAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
			}
			if err := st.Repos().Pots().Create(ctx, p); err != nil {
				t.Fatal(err)
			}
		})
		runBlocked("unpaid", privacy.BlockUnpaidObligation, func(uid string, _ uuid.UUID, meterID uuid.UUID) {
			pr := &payrequests.PaymentRequest{
				ID: uuid.New(), RequesterID: uid, MeterID: meterID,
				AmountMode: payrequests.ModeFixed, AmountMinor: 5000, Currency: money.NGN,
				Status: payrequests.StatusOpen, MaxPayments: 1, CodeHMAC: "pay-" + uuid.NewString(),
				ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
			}
			if err := st.Repos().PayRequests().Create(ctx, pr); err != nil {
				t.Fatal(err)
			}
		})
	})
}
