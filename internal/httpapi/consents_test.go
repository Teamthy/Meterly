package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestConsentsQueryAndWithdrawBlocksSend(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }},
		{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) {
			st := b.open(t)
			box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
			pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
			log := slog.Default()
			o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
			s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log}
			srv := httptest.NewServer(s.Routes())
			defer srv.Close()
			c := client{t, srv}

			code, body := c.do("GET", "/v1/me/consents", "ada", nil, nil)
			if code != 200 {
				t.Fatalf("get consents %d %v", code, body)
			}
			cur, _ := body["current"].([]any)
			if len(cur) != 6 {
				t.Fatalf("want 6 current cells, got %v", body)
			}

			code, body = c.do("POST", "/v1/me/consents/withdraw", "ada", map[string]any{
				"purpose": "TRANSACTIONAL", "channel": "SMS",
			}, nil)
			if code != 200 {
				t.Fatalf("withdraw %d %v", code, body)
			}
			code, prefs := c.do("GET", "/v1/me/notification-preferences", "ada", nil, nil)
			if code != 200 || prefs["sms"] != false {
				t.Fatalf("transactional SMS should be off after withdraw: %d %v", code, prefs)
			}
			p := notifications.Preferences{SMS: prefs["sms"] == true, Email: prefs["email"] == true, WhatsApp: prefs["whatsapp"] == true}
			if privacy.Allows(privacy.PurposeTransactional, notifications.SMS, p, false, false) {
				t.Fatal("next transactional SMS send must be blocked")
			}

			hist, _ := body["history"].([]any)
			if len(hist) < 1 {
				t.Fatalf("want withdrawal history, got %v", body)
			}

			code, _ = c.do("POST", "/v1/me/consents/withdraw", "ada", map[string]any{
				"purpose": "NOPE", "channel": "SMS",
			}, nil)
			if code != 400 {
				t.Fatalf("bad purpose want 400, got %d", code)
			}
		})
	}
}
