package partners_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/partnerwebhooks"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func webhookEngine(t *testing.T, st store.Store, allowLoopback bool) *partners.Engine {
	t.Helper()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return &partners.Engine{
		Repo:   st.Repos().Partners(),
		Box:    box,
		Guard:  &partners.Guard{AllowLoopback: allowLoopback},
		Runner: partnerwebhooks.Runner(st),
		Log:    nil,
	}
}

func TestWebhookRetryThenDeadLetter(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
		eng := webhookEngine(t, st, true)
		eng.Now = func() time.Time { return now }
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		p, err := eng.Create(ctx, nil, uid, "Acme Hooks", partners.EnvSandbox, "t-1", 0, 60)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		t.Cleanup(srv.Close)
		ep, _, err := eng.CreateWebhook(ctx, nil, p.ID, srv.URL+"/hooks", "fail", []string{partners.EventVended}, false)
		if err != nil {
			t.Fatal(err)
		}
		ev := events.New("electricity.vending.completed", uuid.New(), nil)
		if err := eng.Enqueue(ctx, nil, ev, partners.PurchaseSnapshot{
			ID: ev.AggregateID, PartnerID: p.ID, PartnerRef: "r1", Currency: "NGN",
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := eng.ProcessDue(ctx, 10); err != nil || n != 1 {
			t.Fatalf("first attempt n=%d err=%v", n, err)
		}
		list, err := eng.ListDeliveries(ctx, nil, p.ID, ep.ID, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("list %v %v", list, err)
		}
		if list[0].Status != partners.DeliveryFailed || list[0].AttemptCount != 1 {
			t.Fatalf("after fail %+v", list[0])
		}
		now = now.Add(24*time.Hour + time.Minute)
		if n, err := eng.ProcessDue(ctx, 10); err != nil || n != 1 {
			t.Fatalf("horizon attempt n=%d err=%v", n, err)
		}
		list, err = eng.ListDeliveries(ctx, nil, p.ID, ep.ID, 10)
		if err != nil || list[0].Status != partners.DeliveryDeadLetter {
			t.Fatalf("want dead letter %+v %v", list, err)
		}
		redel, err := eng.Redeliver(ctx, nil, p.ID, ep.ID, list[0].ID)
		if err != nil || redel.Status != partners.DeliveryPending {
			t.Fatalf("redeliver %+v %v", redel, err)
		}
	})
}

func TestWebhookDuplicateEventIsSingleDelivery(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		eng := webhookEngine(t, st, true)
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		p, err := eng.Create(ctx, nil, uid, "Acme Dup", partners.EnvSandbox, "t-1", 0, 60)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		t.Cleanup(srv.Close)
		ep, _, err := eng.CreateWebhook(ctx, nil, p.ID, srv.URL+"/h", "", []string{partners.EventPaymentConfirmed}, false)
		if err != nil {
			t.Fatal(err)
		}
		ev := events.New("payment.confirmed", uuid.New(), nil)
		snap := partners.PurchaseSnapshot{ID: ev.AggregateID, PartnerID: p.ID, Currency: "NGN"}
		if err := eng.Enqueue(ctx, nil, ev, snap); err != nil {
			t.Fatal(err)
		}
		if err := eng.Enqueue(ctx, nil, ev, snap); err != nil {
			t.Fatal(err)
		}
		list, err := eng.ListDeliveries(ctx, nil, p.ID, ep.ID, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("want 1 delivery, got %d %v", len(list), err)
		}
	})
}

func TestWebhookSecretRotationOverlapOnWire(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		eng := webhookEngine(t, st, true)
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		p, err := eng.Create(ctx, nil, uid, "Acme Rot", partners.EnvSandbox, "t-1", 0, 60)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var gotHeader string
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotHeader = r.Header.Get("Meterly-Signature")
			gotBody = b
			mu.Unlock()
			w.WriteHeader(200)
		}))
		t.Cleanup(srv.Close)
		ep, oldSecret, err := eng.CreateWebhook(ctx, nil, p.ID, srv.URL+"/h", "", []string{partners.EventVended}, false)
		if err != nil {
			t.Fatal(err)
		}
		ep, newSecret, err := eng.RotateWebhook(ctx, nil, p.ID, ep.ID)
		if err != nil {
			t.Fatal(err)
		}
		d, err := eng.Ping(ctx, nil, p.ID, ep.ID)
		if err != nil || d.Status != partners.DeliveryDelivered {
			t.Fatalf("ping %+v %v", d, err)
		}
		mu.Lock()
		hdr, body := gotHeader, gotBody
		mu.Unlock()
		now := time.Now().UTC()
		if err := partners.VerifySignature([]string{newSecret}, hdr, body, now, partners.WebhookReplayWindow); err != nil {
			t.Fatalf("new secret: %v", err)
		}
		if err := partners.VerifySignature([]string{oldSecret}, hdr, body, now, partners.WebhookReplayWindow); err != nil {
			t.Fatalf("old secret during overlap: %v", err)
		}
	})
}

func TestWebhookTokenOnlyWhenEndpointAllows(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		eng := webhookEngine(t, st, true)
		eng.Token = func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return "12345678901234567890", nil }
		uid, err := st.Repos().Users().EnsureByExternalID(ctx, "operator:ada")
		if err != nil {
			t.Fatal(err)
		}
		p, err := eng.Create(ctx, nil, uid, "Acme Tok", partners.EnvSandbox, "t-1", 0, 60)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var bodies [][]byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
			w.WriteHeader(200)
		}))
		t.Cleanup(srv.Close)
		if _, _, err := eng.CreateWebhook(ctx, nil, p.ID, srv.URL+"/plain", "", []string{partners.EventVended}, false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := eng.CreateWebhook(ctx, nil, p.ID, srv.URL+"/tok", "", []string{partners.EventVended}, true); err != nil {
			t.Fatal(err)
		}
		ev := events.New("electricity.vending.completed", uuid.New(), nil)
		if err := eng.Enqueue(ctx, nil, ev, partners.PurchaseSnapshot{ID: ev.AggregateID, PartnerID: p.ID, Currency: "NGN"}); err != nil {
			t.Fatal(err)
		}
		if n, err := eng.ProcessDue(ctx, 10); err != nil || n != 2 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		mu.Lock()
		got := bodies
		mu.Unlock()
		if len(got) != 2 {
			t.Fatalf("bodies %d", len(got))
		}
		has, missing := 0, 0
		for _, b := range got {
			if partners.PayloadHasToken(b) {
				has++
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				data, _ := m["data"].(map[string]any)
				if data["token"] != "12345678901234567890" {
					t.Fatalf("token field %v", data)
				}
			} else {
				missing++
			}
		}
		if has != 1 || missing != 1 {
			t.Fatalf("token presence has=%d missing=%d", has, missing)
		}
	})
}

func TestMapOutboxAndSnapshot(t *testing.T) {
	if partners.MapOutbox("payment.confirmed") != partners.EventPaymentConfirmed {
		t.Fatal("payment")
	}
	if partners.MapOutbox("electricity.vending.unknown") != partners.EventUnknown {
		t.Fatal("unknown")
	}
	if partners.MapOutbox("token.available") != "" {
		t.Fatal("token.available must not be a partner event")
	}
	tr := &tx.Transaction{ID: uuid.New(), PartnerID: uuid.New(), PartnerRef: "abc", DeliveryMethod: tx.MethodToken}
	tr.Amount.Minor, tr.Total.Minor, tr.Total.Currency = 100, 100, "NGN"
	s := partners.SnapshotOf(tr)
	if s.PartnerRef != "abc" || s.Currency != "NGN" {
		t.Fatalf("%+v", s)
	}
	dead, _ := partners.DeadLetterAfterFailure(time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC().Add(24*time.Hour), 3)
	if !dead {
		t.Fatal("expected dead letter at horizon")
	}
}
