package privacy_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	"github.com/teamthy/meterly/internal/support"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

func eachStore(t *testing.T, fn func(t *testing.T, st store.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, memstore.New()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("DATABASE_URL") == "" {
			t.Skip("DATABASE_URL not set")
		}
		fn(t, pgstore.New(pgtest.NewPool(t), "SANDBOX"))
	})
}

func TestRetentionPurgeExactExpiredAndHolds(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "privacy-user")
		if err != nil {
			t.Fatal(err)
		}
		heldUID, err := st.Repos().Users().EnsureByExternalID(ctx, "privacy-held")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Privacy().AddLegalHold(ctx, privacy.Hold{
			ID: uuid.New(), UserID: heldUID, Reason: "open dispute", CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		oldOTP := &conversation.ChannelOTP{
			ID: uuid.New(), PhoneHMAC: "hmac-old", TargetUserID: uid, CodeHMAC: "c1",
			ExpiresAt: now.Add(-8 * 24 * time.Hour), CreatedAt: now.Add(-8 * 24 * time.Hour),
		}
		freshOTP := &conversation.ChannelOTP{
			ID: uuid.New(), PhoneHMAC: "hmac-fresh", TargetUserID: uid, CodeHMAC: "c2",
			ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now.Add(-time.Hour),
		}
		heldOTP := &conversation.ChannelOTP{
			ID: uuid.New(), PhoneHMAC: "hmac-held", TargetUserID: heldUID, CodeHMAC: "c3",
			ExpiresAt: now.Add(-8 * 24 * time.Hour), CreatedAt: now.Add(-8 * 24 * time.Hour),
		}
		for _, o := range []*conversation.ChannelOTP{oldOTP, freshOTP, heldOTP} {
			if err := st.Repos().Offline().CreateOTP(ctx, o); err != nil {
				t.Fatal(err)
			}
		}

		oldCh := identity.Challenge{
			ID: uuid.New(), Challenge: []byte("old-chal-bytes-32..............."),
			Purpose: "login", UserID: uid, ExpiresAt: now.Add(-8 * 24 * time.Hour),
		}
		if err := st.Repos().Auth().PutChallenge(ctx, oldCh); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Auth().InsertSession(ctx, identity.Session{
			ID: uuid.New(), UserID: uid, TokenHMAC: "sess-old",
			ExpiresAt: now.Add(-91 * 24 * time.Hour), CreatedAt: now.Add(-100 * 24 * time.Hour),
			LastSeenAt: now.Add(-91 * 24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Auth().InsertSession(ctx, identity.Session{
			ID: uuid.New(), UserID: uid, TokenHMAC: "sess-fresh",
			ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now.Add(-time.Hour), LastSeenAt: now,
		}); err != nil {
			t.Fatal(err)
		}

		oldIdem := idempotency.Record{
			Scope: uid, Operation: "quote", Key: "k-old", RequestHash: "h", ResultRef: "done",
			CreatedAt: now.Add(-31 * 24 * time.Hour), ExpiresAt: now.Add(24 * time.Hour),
		}
		if _, err := st.Repos().Idempotency().Reserve(ctx, oldIdem); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Idempotency().Complete(ctx, oldIdem.Scope, oldIdem.Operation, oldIdem.Key, "done"); err != nil {
			t.Fatal(err)
		}
		freshIdem := idempotency.Record{
			Scope: uid, Operation: "quote", Key: "k-fresh", RequestHash: "h2",
			CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour),
		}
		if _, err := st.Repos().Idempotency().Reserve(ctx, freshIdem); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Idempotency().Complete(ctx, freshIdem.Scope, freshIdem.Operation, freshIdem.Key, "done"); err != nil {
			t.Fatal(err)
		}

		if err := st.Repos().Risk().InsertObservation(ctx, risk.Observation{
			ID: uuid.New(), ObservedAt: now.Add(-91 * 24 * time.Hour), Kind: risk.KindCheckout,
			Surface: risk.SurfacePurchase, UserID: uid,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Risk().InsertObservation(ctx, risk.Observation{
			ID: uuid.New(), ObservedAt: now.Add(-time.Hour), Kind: risk.KindCheckout,
			Surface: risk.SurfacePurchase, UserID: uid,
		}); err != nil {
			t.Fatal(err)
		}

		closed := &support.Thread{
			ID: uuid.New(), CustomerID: uid, Mode: support.ModeAgent, Status: support.StatusClosed,
			CreatedAt: now.Add(-800 * 24 * time.Hour), UpdatedAt: now.Add(-800 * 24 * time.Hour),
			LastMessageAt: now.Add(-800 * 24 * time.Hour),
		}
		openTh := &support.Thread{
			ID: uuid.New(), CustomerID: uid, Mode: support.ModeAgent, Status: support.StatusOpen,
			CreatedAt: now.Add(-800 * 24 * time.Hour), UpdatedAt: now.Add(-800 * 24 * time.Hour),
			LastMessageAt: now.Add(-time.Hour),
		}
		if err := st.Repos().Support().CreateThread(ctx, closed); err != nil {
			t.Fatal(err)
		}
		if err := st.Repos().Support().CreateThread(ctx, openTh); err != nil {
			t.Fatal(err)
		}

		settings := privacy.DefaultSettings()
		settings.BatchSize = 50

		dry, err := privacy.Run(ctx, st.Repos().Privacy(), func() privacy.Settings {
			s := settings
			s.DryRun = true
			return s
		}(), nil, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if dry.Counts["channel_otps"] < 1 {
			t.Fatalf("dry-run should count expired OTPs, got %v", dry.Counts)
		}
		if _, err := st.Repos().Offline().GetLatestOTPForUpdate(ctx, "hmac-old"); err != nil {
			t.Fatalf("dry-run must not delete: %v", err)
		}
		if _, err := st.Repos().Auth().GetSessionByHMAC(ctx, "sess-old"); err != nil {
			t.Fatalf("dry-run must not delete sessions: %v", err)
		}
		if _, err := st.Repos().Support().GetThread(ctx, closed.ID); err != nil {
			t.Fatalf("dry-run must not delete threads: %v", err)
		}

		res, err := privacy.Run(ctx, st.Repos().Privacy(), settings, nil, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Counts["channel_otps"] != 1 {
			t.Fatalf("want 1 expired OTP purged (held skipped), got %v", res.Counts)
		}
		if res.Counts["auth_challenges"] != 1 || res.Counts["auth_sessions"] != 1 {
			t.Fatalf("want 1 challenge and 1 session purged, got %v", res.Counts)
		}
		if res.Counts["idempotency_keys"] != 1 || res.Counts["risk_observations"] != 1 || res.Counts["support_threads"] != 1 {
			t.Fatalf("want 1 idempotency, 1 risk observation, 1 support thread, got %v", res.Counts)
		}
		if _, err := st.Repos().Offline().GetLatestOTPForUpdate(ctx, "hmac-old"); err == nil {
			t.Fatal("expired OTP should be gone")
		}
		if _, err := st.Repos().Offline().GetLatestOTPForUpdate(ctx, "hmac-fresh"); err != nil {
			t.Fatalf("fresh OTP: %v", err)
		}
		if _, err := st.Repos().Offline().GetLatestOTPForUpdate(ctx, "hmac-held"); err != nil {
			t.Fatalf("held OTP must survive: %v", err)
		}
		if _, err := st.Repos().Auth().GetSessionByHMAC(ctx, "sess-old"); err == nil {
			t.Fatal("expired session should be gone")
		}
		if _, err := st.Repos().Auth().GetSessionByHMAC(ctx, "sess-fresh"); err != nil {
			t.Fatalf("fresh session: %v", err)
		}
		if _, err := st.Repos().Support().GetThread(ctx, closed.ID); err == nil {
			t.Fatal("closed old thread should be gone")
		}
		if _, err := st.Repos().Support().GetThread(ctx, openTh.ID); err != nil {
			t.Fatalf("open thread: %v", err)
		}
		if _, ok := res.Counts["electricity_transactions"]; ok {
			t.Fatalf("must not purge financial tables: %v", res.Counts)
		}
		if _, ok := res.Counts["payments"]; ok {
			t.Fatalf("must not purge payments: %v", res.Counts)
		}

		again, err := privacy.Run(ctx, st.Repos().Privacy(), settings, nil, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if again.Counts["channel_otps"] != 0 || again.Counts["auth_sessions"] != 0 || again.Counts["support_threads"] != 0 {
			t.Fatalf("second run should be a no-op, got %v", again.Counts)
		}

		recent, err := st.Repos().Audit().Recent(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		var saw bool
		for _, e := range recent {
			if e.Action == "retention.purge" {
				saw = true
				if _, ok := e.Metadata["counts"]; !ok {
					t.Fatal("audit missing counts")
				}
			}
		}
		if !saw {
			t.Fatal("expected retention.purge audit row")
		}
	})
}
