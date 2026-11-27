package partners_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
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

func TestIssueAuthenticateRevokeSuspendAndPlaintextNeverStored(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
		if err != nil {
			t.Fatal(err)
		}
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		eng := &partners.Engine{Repo: st.Repos().Partners(), Box: box, Environment: partners.EnvSandbox}
		p, err := eng.Create(ctx, nil, uid, "Acme Power", partners.EnvSandbox, "ticket-1", 1_000_000, 60)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != partners.StatusActive {
			t.Fatalf("sandbox status %s", p.Status)
		}
		plain, key, err := eng.Issue(ctx, nil, uid, p.ID, []string{partners.ScopeMetersValidate, partners.ScopeFloatRead}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(plain, partners.PrefixTest) {
			t.Fatalf("secret %s", plain)
		}
		stored, err := st.Repos().Partners().GetKey(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SecretHash == plain || stored.Prefix == plain {
			t.Fatal("plaintext stored")
		}
		if strings.Contains(stored.SecretHash, plain) {
			t.Fatal("hash contains secret")
		}
		id, err := eng.Authenticate(ctx, plain)
		if err != nil {
			t.Fatal(err)
		}
		if id.Partner.ID != p.ID || !partners.HasScope(id.Key.Scopes, partners.ScopeFloatRead) {
			t.Fatalf("%+v", id)
		}
		if _, err := eng.Authenticate(ctx, plain[:len(plain)-1]+"x"); err == nil {
			t.Fatal("tampered key")
		}

		if _, err := eng.Revoke(ctx, nil, uid, p.ID, key.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Authenticate(ctx, plain); err == nil {
			t.Fatal("revoked still worked")
		}

		plain2, _, err := eng.Issue(ctx, nil, uid, p.ID, []string{partners.ScopeFloatRead}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Suspend(ctx, nil, uid, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Authenticate(ctx, plain2); err == nil {
			t.Fatal("suspended partner key worked")
		}
	})
}

func TestProductionTwoPersonAndEnvMismatch(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
		if err != nil {
			t.Fatal(err)
		}
		a, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		b, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:bola")
		if err != nil {
			t.Fatal(err)
		}
		eng := &partners.Engine{Repo: st.Repos().Partners(), Box: box, Environment: partners.EnvProduction}
		p, err := eng.Create(ctx, nil, a, "Live Co", partners.EnvProduction, "kyb-9", 0, 60)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != partners.StatusPending {
			t.Fatalf("status %s", p.Status)
		}
		if _, _, err := eng.Issue(ctx, nil, a, p.ID, []string{partners.ScopeFloatRead}, nil); err != partners.ErrNotActive {
			t.Fatalf("issue pending: %v", err)
		}
		proposed, err := eng.Activate(ctx, nil, a, p.ID)
		if err != nil || proposed.Status != partners.StatusPending || proposed.ProposedActivationBy != a {
			t.Fatalf("propose %+v %v", proposed, err)
		}
		if _, err := eng.Activate(ctx, nil, a, p.ID); err != partners.ErrSameApprover {
			t.Fatalf("same approver: %v", err)
		}
		active, err := eng.Activate(ctx, nil, b, p.ID)
		if err != nil || active.Status != partners.StatusActive {
			t.Fatalf("approve %+v %v", active, err)
		}
		plain, _, err := eng.Issue(ctx, nil, a, p.ID, []string{partners.ScopeFloatRead}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(plain, partners.PrefixLive) {
			t.Fatalf("live secret %s", plain)
		}
		if _, err := eng.Authenticate(ctx, plain); err != nil {
			t.Fatal(err)
		}
		eng.Environment = partners.EnvSandbox
		if _, err := eng.Authenticate(ctx, plain); err == nil {
			t.Fatal("live key accepted on sandbox process")
		}
	})
}

func TestRotationOverlapAndExpiry(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
		if err != nil {
			t.Fatal(err)
		}
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		eng := &partners.Engine{
			Repo: st.Repos().Partners(), Box: box, Environment: partners.EnvSandbox,
			Now: func() time.Time { return now },
		}
		p, err := eng.Create(ctx, nil, uid, "Rotate Co", partners.EnvSandbox, "", 0, 30)
		if err != nil {
			t.Fatal(err)
		}
		oldPlain, oldKey, err := eng.Issue(ctx, nil, uid, p.ID, []string{partners.ScopeWebhooksManage}, nil)
		if err != nil {
			t.Fatal(err)
		}
		newPlain, _, err := eng.Rotate(ctx, nil, uid, p.ID, oldKey.ID, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Authenticate(ctx, oldPlain); err != nil {
			t.Fatalf("old during overlap: %v", err)
		}
		if _, err := eng.Authenticate(ctx, newPlain); err != nil {
			t.Fatalf("new: %v", err)
		}
		eng.Now = func() time.Time { return now.Add(2 * time.Hour) }
		if _, err := eng.Authenticate(ctx, oldPlain); err == nil {
			t.Fatal("old after overlap")
		}
		if _, err := eng.Authenticate(ctx, newPlain); err != nil {
			t.Fatalf("new after overlap: %v", err)
		}

		eng.Now = func() time.Time { return now }
		exp := now.Add(time.Minute)
		shortPlain, _, err := eng.Issue(ctx, nil, uid, p.ID, []string{partners.ScopeFloatRead}, &exp)
		if err != nil {
			t.Fatal(err)
		}
		eng.Now = func() time.Time { return now }
		if _, err := eng.Authenticate(ctx, shortPlain); err != nil {
			t.Fatalf("before expiry: %v", err)
		}
		eng.Now = func() time.Time { return now.Add(2 * time.Minute) }
		if _, err := eng.Authenticate(ctx, shortPlain); err == nil {
			t.Fatal("expired key worked")
		}
	})
}
