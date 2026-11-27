package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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

func TestNotificationsSchemaRemovesPlaintextContacts(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'users' AND column_name IN ('email', 'phone')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("plaintext users.email / users.phone must be dropped, found %d columns", count)
	}

	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'notification_deliveries'
		  AND column_name IN ('body', 'message', 'subject', 'destination', 'phone', 'email', 'token')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("notification_deliveries must never store body/destination/token, found %d forbidden columns", count)
	}
}

func TestNotificationRepositoriesParity(t *testing.T) {
	run := func(t *testing.T, st store.Store) {
		ctx := context.Background()
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
		if err != nil {
			t.Fatal(err)
		}

		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "ada")
		if err != nil {
			t.Fatal(err)
		}

		// Default preferences: SMS=true, Email=true, WhatsApp=false.
		prefs, err := st.Repos().Notifications().GetPreferences(ctx, uid)
		if err != nil || !prefs.SMS || !prefs.Email || prefs.WhatsApp {
			t.Fatalf("default prefs: %+v %v", prefs, err)
		}

		// Rollback test on SetPreferences + UpsertContacts + AddOptOut.
		errBoom := errors.New("boom")
		ctEmail, hmacEmail, maskEmail, _ := notifications.SealUserEmail(box, uid, "ada@example.com")
		if err := st.InTx(ctx, func(r store.Repos) error {
			if err := r.Users().UpsertContacts(ctx, notifications.UserContacts{
				UserID: uid, EmailCiphertext: ctEmail, EmailHMAC: hmacEmail,
				EmailMasked: maskEmail, EmailKeyVersion: box.KeyVersion, DisplayName: "Ada",
			}); err != nil {
				return err
			}
			if err := r.Notifications().SetPreferences(ctx, uid, notifications.Preferences{SMS: false, Email: true, WhatsApp: true}, time.Now()); err != nil {
				return err
			}
			if _, err := r.Notifications().AddOptOut(ctx, notifications.OptOut{DestinationHMAC: hmacEmail, Channel: notifications.Email}); err != nil {
				return err
			}
			return errBoom
		}); !errors.Is(err, errBoom) {
			t.Fatalf("want boom, got %v", err)
		}
		if c, _ := st.Repos().Users().Contacts(ctx, uid); len(c.EmailCiphertext) != 0 {
			t.Fatal("contacts were not rolled back")
		}
		if p, _ := st.Repos().Notifications().GetPreferences(ctx, uid); !p.SMS || p.WhatsApp {
			t.Fatalf("preferences were not rolled back: %+v", p)
		}
		if opted, _ := st.Repos().Notifications().IsOptedOut(ctx, hmacEmail, notifications.Email); opted {
			t.Fatal("opt-out was not rolled back")
		}

		// Commit contacts, preferences, opt-out and WhatsApp window.
		ctPhone, hmacPhone, maskPhone, _ := notifications.SealUserPhone(box, uid, "+2348012345678")
		if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
			UserID: uid, EmailCiphertext: ctEmail, EmailHMAC: hmacEmail, EmailMasked: maskEmail, EmailKeyVersion: box.KeyVersion,
			PhoneCiphertext: ctPhone, PhoneHMAC: hmacPhone, PhoneMasked: maskPhone, PhoneKeyVersion: box.KeyVersion,
			DisplayName: "Ada",
		}); err != nil {
			t.Fatal(err)
		}
		gotC, err := st.Repos().Users().Contacts(ctx, uid)
		if err != nil || gotC.EmailHMAC != hmacEmail || gotC.PhoneHMAC != hmacPhone || gotC.DisplayName != "Ada" {
			t.Fatalf("contacts: %+v %v", gotC, err)
		}
		if err := st.Repos().Notifications().SetPreferences(ctx, uid, notifications.Preferences{SMS: true, Email: false, WhatsApp: true}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if p, _ := st.Repos().Notifications().GetPreferences(ctx, uid); !p.SMS || p.Email || !p.WhatsApp {
			t.Fatalf("updated prefs: %+v", p)
		}

		ins, err := st.Repos().Notifications().AddOptOut(ctx, notifications.OptOut{DestinationHMAC: hmacPhone, Channel: notifications.SMS})
		if err != nil || !ins {
			t.Fatalf("first opt-out insert: %v %v", ins, err)
		}
		ins, err = st.Repos().Notifications().AddOptOut(ctx, notifications.OptOut{DestinationHMAC: hmacPhone, Channel: notifications.SMS})
		if err != nil || ins {
			t.Fatalf("duplicate opt-out insert: %v %v", ins, err)
		}
		if opted, _ := st.Repos().Notifications().IsOptedOut(ctx, hmacPhone, notifications.SMS); !opted {
			t.Fatal("expected SMS opt-out")
		}
		if opted, _ := st.Repos().Notifications().IsOptedOut(ctx, hmacPhone, notifications.WhatsApp); opted {
			t.Fatal("SMS opt-out must be channel-scoped")
		}

		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := st.Repos().Notifications().RecordWhatsAppInbound(ctx, hmacPhone, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if active, _ := st.Repos().Notifications().HasActiveWhatsAppWindow(ctx, hmacPhone, now); !active {
			t.Fatal("2h-old inbound should have active 24h window")
		}
		if active, _ := st.Repos().Notifications().HasActiveWhatsAppWindow(ctx, hmacPhone, now.Add(23*time.Hour)); active {
			t.Fatal("25h-old inbound must not have active window")
		}

		// Create minimal meter + quote + transaction so notification_deliveries FK is satisfied.
		mid := uuid.New()
		ctMeter, _ := box.Encrypt("12345678901")
		if err := st.Repos().Meters().Insert(ctx, &meters.Meter{
			ID: mid, NumberEncrypted: ctMeter, KeyVersion: box.KeyVersion,
			NumberHash: box.Hash("meter:EKEDC:12345678901"), Masked: "*******8901",
			DiscoCode: "EKEDC", MeterType: "PREPAID", ValidatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		qid := uuid.New()
		amt := money.New(500000, money.NGN)
		qObj := &tx.Quote{
			ID: qid, CustomerID: uid, MeterID: mid, Amount: amt, Fee: money.New(0, money.NGN), Total: amt,
			DeliveryMethod: tx.MethodToken, DeliveryOptions: []tx.DeliveryMethod{tx.MethodToken},
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		}
		if err := st.Repos().Quotes().Create(ctx, qObj); err != nil {
			t.Fatal(err)
		}
		tObj := tx.New(uid, qObj, false, "", now)
		if err := st.Repos().Transactions().Create(ctx, tObj); err != nil {
			t.Fatal(err)
		}

		evID := uuid.New()
		oldCreated := now.Add(-400 * 24 * time.Hour)
		d := &notifications.Delivery{
			EventID:         evID,
			TransactionID:   tObj.ID,
			RecipientKind:   notifications.RecipientPurchaser,
			Channel:         notifications.SMS,
			DestinationHMAC: hmacPhone,
			TemplateID:      notifications.TemplatePaymentConfirmed,
			TemplateVersion: notifications.CurrentTemplateVersion,
			Status:          notifications.StatusPending,
			NextAttemptAt:   now.Add(-time.Minute),
			CreatedAt:       oldCreated,
			UpdatedAt:       oldCreated,
		}
		created, err := st.Repos().Notifications().CreateDelivery(ctx, d)
		if err != nil || !created {
			t.Fatalf("create delivery: %v %v", created, err)
		}
		dup := *d
		dup.ID = uuid.New()
		created2, err := st.Repos().Notifications().CreateDelivery(ctx, &dup)
		if err != nil || created2 || dup.ID != d.ID {
			t.Fatalf("unique (event_id, recipient_kind, channel) dedupe failed: created=%v id=%s want=%s err=%v", created2, dup.ID, d.ID, err)
		}

		due, err := st.Repos().Notifications().ListDueDeliveries(ctx, now, now.Add(-5*time.Minute), 10)
		if err != nil || len(due) != 1 || due[0] != d.ID {
			t.Fatalf("due deliveries: %v %v", due, err)
		}

		sentAt := now
		d.Status = notifications.StatusSent
		d.Attempts = 1
		d.SentAt = &sentAt
		d.UpdatedAt = now
		if err := st.Repos().Notifications().UpdateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}

		purged, err := st.Repos().Notifications().PurgeOldDestinationHMACs(ctx, now.Add(-365*24*time.Hour))
		if err != nil || purged != 1 {
			t.Fatalf("purge old destination_hmac: purged=%d err=%v", purged, err)
		}
		after, err := st.Repos().Notifications().GetDelivery(ctx, d.ID)
		if err != nil || after.DestinationHMAC != "" || after.Status != notifications.StatusSent {
			t.Fatalf("after purge: %+v %v", after, err)
		}
	}

	t.Run("memstore", func(t *testing.T) { run(t, memstore.New()) })
	t.Run("pgstore", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		run(t, pgstore.New(pool, "SANDBOX"))
	})
}
