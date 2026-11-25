package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
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

func TestOfflineRepoParity(t *testing.T) {
	run := func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
		if err != nil {
			t.Fatal(err)
		}

		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "offline-user-1")
		if err != nil {
			t.Fatal(err)
		}
		ct, phoneHMAC, masked, err := notifications.SealUserPhone(box, uid, "+2348031234567")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Users().UpsertContacts(ctx, notifications.UserContacts{
			UserID:          uid,
			PhoneCiphertext: ct,
			PhoneHMAC:       phoneHMAC,
			PhoneMasked:     masked,
			PhoneKeyVersion: "v1",
			UpdatedAt:       now,
		}); err != nil {
			t.Fatal(err)
		}

		foundUID, err := st.Repos().Offline().FindUserIDByPhoneHMAC(ctx, phoneHMAC)
		if err != nil || foundUID != uid {
			t.Fatalf("FindUserIDByPhoneHMAC = (%q, %v), want (%q, nil)", foundUID, err, uid)
		}

		// PhoneLink upsert & lookup
		link := &conversation.PhoneLink{
			PhoneHMAC:    phoneHMAC,
			UserID:       uid,
			Status:       conversation.LinkVerified,
			VerifiedAt:   &now,
			LastActiveAt: now,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := st.Repos().Offline().UpsertPhoneLink(ctx, link); err != nil {
			t.Fatalf("UpsertPhoneLink: %v", err)
		}
		gotLink, err := st.Repos().Offline().GetPhoneLink(ctx, phoneHMAC)
		if err != nil || gotLink.Status != conversation.LinkVerified || gotLink.UserID != uid {
			t.Fatalf("GetPhoneLink = (%+v, %v)", gotLink, err)
		}

		// OTP create, lookup, update
		otp := &conversation.ChannelOTP{
			ID:           uuid.New(),
			PhoneHMAC:    phoneHMAC,
			TargetUserID: uid,
			CodeHMAC:     "hmac-123456",
			Attempts:     0,
			ExpiresAt:    now.Add(10 * time.Minute),
			CreatedAt:    now,
		}
		if err := st.Repos().Offline().CreateOTP(ctx, otp); err != nil {
			t.Fatalf("CreateOTP: %v", err)
		}
		if err := st.InTx(ctx, func(r store.Repos) error {
			cur, err := r.Offline().GetLatestOTPForUpdate(ctx, phoneHMAC)
			if err != nil {
				return err
			}
			cur.Attempts = 1
			cur.ConsumedAt = &now
			return r.Offline().UpdateOTP(ctx, cur)
		}); err != nil {
			t.Fatalf("UpdateOTP InTx: %v", err)
		}

		// Meter + Session + ProcessedMessage + OfflinePurchase + TokenResend
		ctMeter, _ := box.Encrypt("45012341234")
		m := &meters.Meter{
			ID:              uuid.New(),
			NumberEncrypted: ctMeter,
			NumberHash:      "mh-" + uuid.NewString(),
			KeyVersion:      "v1",
			Masked:          "*******1234",
			DiscoCode:       "IKEDC",
			MeterType:       "PREPAID",
			ValidatedAt:     now,
		}
		if err := st.Repos().Meters().Insert(ctx, m); err != nil {
			t.Fatal(err)
		}

		sess := &conversation.Session{
			ID:                uuid.New(),
			Channel:           conversation.ChannelWhatsApp,
			ProviderSessionID: "wa-sess-1",
			PhoneHMAC:         phoneHMAC,
			UserID:            uid,
			State:             conversation.StateAwaitMeterChoice,
			Step:              1,
			MeterOptions:      []conversation.MeterOption{{MeterID: m.ID, Masked: m.Masked, Disco: "IKEDC", Label: "Home"}},
			LastRawInput:      "buy",
			LastReply:         "Select a meter",
			HasCachedTurn:     true,
			LastInboundMsgTS:  now,
			ExpiresAt:         now.Add(30 * time.Minute),
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := st.Repos().Offline().CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := st.Repos().Offline().CreateSession(ctx, sess); !errors.Is(err, store.ErrDuplicate) {
			t.Fatalf("duplicate CreateSession expected ErrDuplicate, got %v", err)
		}

		if err := st.InTx(ctx, func(r store.Repos) error {
			active, err := r.Offline().GetActiveSessionForPhoneForUpdate(ctx, conversation.ChannelWhatsApp, phoneHMAC, now.Add(time.Minute))
			if err != nil {
				return err
			}
			if len(active.MeterOptions) != 1 || active.MeterOptions[0].MeterID != m.ID {
				t.Fatalf("unexpected MeterOptions: %+v", active.MeterOptions)
			}
			active.State = conversation.StateCompleted
			active.UpdatedAt = now.Add(2 * time.Minute)
			return r.Offline().UpdateSession(ctx, active)
		}); err != nil {
			t.Fatalf("active session roundtrip: %v", err)
		}

		ins1, err := st.Repos().Offline().RecordProcessedMessage(ctx, conversation.ChannelWhatsApp, "wamid.1", phoneHMAC, now)
		if err != nil || !ins1 {
			t.Fatalf("first RecordProcessedMessage = (%v, %v)", ins1, err)
		}
		ins2, err := st.Repos().Offline().RecordProcessedMessage(ctx, conversation.ChannelWhatsApp, "wamid.1", phoneHMAC, now)
		if err != nil || ins2 {
			t.Fatalf("duplicate RecordProcessedMessage = (%v, %v), want (false, nil)", ins2, err)
		}

		// Quote + Transaction + OfflinePurchase + Resend count
		q := &tx.Quote{
			ID:             uuid.New(),
			CustomerID:     uid,
			MeterID:        m.ID,
			Amount:         money.New(300_000, money.NGN),
			Fee:            money.New(10_000, money.NGN),
			Total:          money.New(310_000, money.NGN),
			DeliveryMethod: tx.MethodToken,
			DeliveryOptions: []tx.DeliveryMethod{
				tx.MethodToken,
			},
			ExpiresAt: now.Add(15 * time.Minute),
			Used:      true,
			CreatedAt: now,
		}
		if err := st.Repos().Quotes().Create(ctx, q); err != nil {
			t.Fatal(err)
		}
		tr := tx.New(uid, q, false, "idem-offline-1", now)
		if err := st.Repos().Transactions().Create(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Offline().RecordPurchase(ctx, &conversation.OfflinePurchase{
			TransactionID:     tr.ID,
			UserID:            uid,
			Channel:           conversation.ChannelUSSD,
			PhoneHMAC:         phoneHMAC,
			ProviderSessionID: "ATUid_1",
			AmountMinor:       300_000,
			Currency:          money.NGN,
			CreatedAt:         now,
		}); err != nil {
			t.Fatalf("RecordPurchase: %v", err)
		}

		startUTC, endUTC := conversation.LagosDayBounds(now)
		sum, err := st.Repos().Offline().SumPhoneSpendMinor(ctx, phoneHMAC, startUTC, endUTC)
		if err != nil || sum != 300_000 {
			t.Fatalf("SumPhoneSpendMinor = (%d, %v), want 300000", sum, err)
		}

		if err := st.Repos().Offline().RecordTokenResend(ctx, &conversation.TokenResend{
			ID:            uuid.New(),
			UserID:        uid,
			TransactionID: tr.ID,
			Channel:       conversation.ChannelUSSD,
			PhoneHMAC:     phoneHMAC,
			CreatedAt:     now,
		}); err != nil {
			t.Fatalf("RecordTokenResend: %v", err)
		}
		cnt, err := st.Repos().Offline().CountTokenResends(ctx, uid, startUTC, endUTC)
		if err != nil || cnt != 1 {
			t.Fatalf("CountTokenResends = (%d, %v), want 1", cnt, err)
		}
	}

	t.Run("memstore", func(t *testing.T) {
		run(t, memstore.New())
	})
	t.Run("pgstore", func(t *testing.T) {
		run(t, pgstore.New(pgtest.NewPool(t), "SANDBOX"))
	})
}
