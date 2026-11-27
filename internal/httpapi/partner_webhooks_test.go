package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/partnerwebhooks"
	"github.com/teamthy/meterly/internal/store"
)

func TestPartnerWebhookHTTPAndPurchaseEvents(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		s, c, _ := partnerAPI(t, st, false)
		s.Partners.Guard = &partners.Guard{AllowLoopback: true}
		s.Partners.Runner = partnerwebhooks.Runner(st)
		s.Partners.Token = func(ctx context.Context, partnerID, txnID uuid.UUID) (string, error) {
			tok, _, err := s.O.PartnerToken(ctx, partnerID, txnID)
			return tok, err
		}

		scopes := append(append([]string{}, purchaseScopes...), partners.ScopeWebhooksManage)
		pid, secret := issuePartner(t, c, "Acme Hooks", 0, scopes)
		fundPartner(t, c, pid, "BANK-HOOK-001", 2_000_000)

		var mu sync.Mutex
		var rec []struct {
			typ, sig string
			body     []byte
		}
		recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			rec = append(rec, struct {
				typ, sig string
				body     []byte
			}{r.Header.Get("Meterly-Event-Type"), r.Header.Get("Meterly-Signature"), b})
			mu.Unlock()
			w.WriteHeader(200)
		}))
		t.Cleanup(recv.Close)

		code, created := c.do("POST", "/v1/partner/webhooks", secret, map[string]any{
			"url": recv.URL + "/hooks", "description": "demo",
			"events": []string{
				partners.EventPaymentConfirmed, partners.EventVended, partners.EventDelivered,
				partners.EventFailed, partners.EventRefunded, partners.EventUnknown,
			},
			"include_token": true,
		}, nil)
		if code != 201 {
			t.Fatalf("create webhook %d %v", code, created)
		}
		whSecret, _ := created["secret"].(string)
		if !strings.HasPrefix(whSecret, partners.WebhookSecretPrefix) {
			t.Fatalf("secret %v", created)
		}
		if code, listed := c.do("GET", "/v1/partner/webhooks", secret, nil, nil); code != 200 {
			t.Fatalf("list %d %v", code, listed)
		} else if listed["secret"] != nil {
			t.Fatal("list leaked secret")
		}

		code, val := c.do("POST", "/v1/partner/meters/validate", secret, map[string]any{
			"meter_number": "45012345678", "disco_code": "IKEDC",
		}, nil)
		if code != 200 {
			t.Fatalf("validate %d %v", code, val)
		}
		code, q := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
			"meter_number": "45012345678", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
		}, nil)
		if code != 201 {
			t.Fatalf("quote %d %v", code, q)
		}
		code, purchase := c.do("POST", "/v1/partner/purchases", secret, map[string]any{
			"quote_id": q["quote_id"], "partner_ref": "hook-order-0001",
		}, map[string]string{"Idempotency-Key": "partner-hook-00000001"})
		if code != 201 {
			t.Fatalf("purchase %d %v", code, purchase)
		}

		pub := outbox.NewPublisher(st, partnerwebhooks.Sink(s.Partners), s.Log)
		if _, err := pub.PublishBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Partners.ProcessDue(context.Background(), 20); err != nil || n == 0 {
			t.Fatalf("deliver n=%d err=%v", n, err)
		}

		mu.Lock()
		got := rec
		mu.Unlock()
		seen := map[string]bool{}
		now := time.Now().UTC()
		for _, e := range got {
			seen[e.typ] = true
			if err := partners.VerifySignature([]string{whSecret}, e.sig, e.body, now, partners.WebhookReplayWindow); err != nil {
				t.Fatalf("sig %s: %v", e.typ, err)
			}
			if e.typ == partners.EventVended && !partners.PayloadHasToken(e.body) {
				t.Fatal("expected token on vended when include_token")
			}
		}
		for _, want := range []string{partners.EventPaymentConfirmed, partners.EventVended, partners.EventDelivered} {
			if !seen[want] {
				t.Fatalf("missing event %s in %v", want, seen)
			}
		}

		code, ping := c.do("POST", "/v1/partner/webhooks/"+created["id"].(string)+"/ping", secret, map[string]any{}, nil)
		if code != 200 || ping["status"] != partners.DeliveryDelivered {
			t.Fatalf("ping %d %v", code, ping)
		}

		code, deliveries := c.do("GET", "/v1/partner/webhooks/"+created["id"].(string)+"/deliveries", secret, nil, nil)
		if code != 200 {
			t.Fatalf("deliveries %d %v", code, deliveries)
		}

		code, rotated := c.do("POST", "/v1/partner/webhooks/"+created["id"].(string)+"/rotate", secret, map[string]any{}, nil)
		if code != 200 || rotated["secret"] == nil || rotated["secret"] == whSecret {
			t.Fatalf("rotate %d %v", code, rotated)
		}

		if code, body := c.do("POST", "/v1/partner/webhooks", secret, map[string]any{
			"url": "https://169.254.169.254/hooks", "events": []string{partners.EventVended},
		}, nil); code != 400 {
			t.Fatalf("ssrf create %d %v", code, body)
		}

		code, qFail := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
			"meter_number": "45012340000", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
		}, nil)
		if code != 201 {
			t.Fatalf("fail quote %d %v", code, qFail)
		}
		code, failP := c.do("POST", "/v1/partner/purchases", secret, map[string]any{
			"quote_id": qFail["quote_id"], "partner_ref": "hook-order-fail1",
		}, map[string]string{"Idempotency-Key": "partner-hook-fail0001"})
		if code != 201 {
			t.Fatalf("fail purchase %d %v", code, failP)
		}
		if _, err := pub.PublishBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Partners.ProcessDue(context.Background(), 20); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got = rec
		mu.Unlock()
		seenFail := false
		seenRefund := false
		for _, e := range got {
			if e.typ == partners.EventFailed {
				seenFail = true
			}
			if e.typ == partners.EventRefunded {
				seenRefund = true
			}
		}
		if !seenFail || !seenRefund {
			t.Fatalf("sandbox fail meters: failed=%v refunded=%v", seenFail, seenRefund)
		}

		code, qUnk := c.do("POST", "/v1/partner/quotes", secret, map[string]any{
			"meter_number": "45012348888", "disco_code": "IKEDC", "amount_minor": 500000, "currency": "NGN",
		}, nil)
		if code != 201 {
			t.Fatalf("unk quote %d %v", code, qUnk)
		}
		code, unkP := c.do("POST", "/v1/partner/purchases", secret, map[string]any{
			"quote_id": qUnk["quote_id"], "partner_ref": "hook-order-unk01",
		}, map[string]string{"Idempotency-Key": "partner-hook-unk00001"})
		if code != 201 {
			t.Fatalf("unk purchase %d %v", code, unkP)
		}
		if _, err := pub.PublishBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Partners.ProcessDue(context.Background(), 20); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got = rec
		mu.Unlock()
		seenUnk := false
		for _, e := range got {
			if e.typ == partners.EventUnknown {
				seenUnk = true
				var m map[string]any
				_ = json.Unmarshal(e.body, &m)
			}
		}
		if !seenUnk {
			t.Fatal("sandbox 8888 did not emit purchase.unknown")
		}
	})
}

func TestPartnerWebhookScopeDenied(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		_, c, _ := partnerAPI(t, st, false)
		_, secret := issuePartner(t, c, "No Hooks", 0, []string{partners.ScopeMetersValidate})
		code, body := c.do("POST", "/v1/partner/webhooks", secret, map[string]any{
			"url": "https://example.com/h", "events": []string{partners.EventVended},
		}, nil)
		if code != 403 {
			t.Fatalf("want 403 got %d %v", code, body)
		}
	})
}
