package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func newRiskEnv(t *testing.T, st store.Store) (client, *orchestrator.Orchestrator) {
	t.Helper()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	o.SetFeatureRiskEngine(true)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return client{t, srv}, o
}

func TestRiskAdminRulesAndDenylist(t *testing.T) {
	eachStore(t, func(t *testing.T, st store.Store) {
		c, _ := newRiskEnv(t, st)
		if code, _ := c.do("GET", "/v1/admin/risk/rules", "ada", nil, nil); code != 403 {
			t.Fatalf("customer list: %d", code)
		}
		code, list := c.do("GET", "/v1/admin/risk/rules", "operator:ola", nil, nil)
		if code != 200 {
			t.Fatalf("list %d %v", code, list)
		}
		data, _ := list["data"].([]any)
		if len(data) < 8 {
			t.Fatalf("expected seeded rules, got %d", len(data))
		}
		code, updated := c.do("PUT", "/v1/admin/risk/rules/velocity_user", "operator:ola",
			map[string]any{"reason": "tighten velocity for pilot", "threshold": 2}, nil)
		if code != 200 || updated["threshold"].(float64) != 2 {
			t.Fatalf("update %d %v", code, updated)
		}
		// The change is audited atomically, with what changed.
		evs, err := st.Repos().Audit().ForResource(context.Background(), "risk_rule", uuid.Nil)
		if err != nil || len(evs) != 1 || evs[0].Action != "risk.rule.update" {
			t.Fatalf("audit %v %+v", err, evs)
		}
		if evs[0].Metadata["threshold_after"] != float64(2) && evs[0].Metadata["threshold_after"] != int64(2) && evs[0].Metadata["threshold_after"] != 2 {
			t.Fatalf("audit lacks before/after values: %+v", evs[0].Metadata)
		}
		if code, _ := c.do("GET", "/v1/admin/risk/denylist", "operator:ola", nil, nil); code != 403 {
			t.Fatalf("operator denylist get: %d", code)
		}
		code, created := c.do("POST", "/v1/admin/risk/denylist", "admin:ada",
			map[string]any{"reason": "block known card tester", "kind": "EMAIL", "value": "hmac-abc"}, nil)
		if code != 201 {
			t.Fatalf("add %d %v", code, created)
		}
		id, _ := created["id"].(string)
		if id == "" {
			t.Fatalf("missing id %v", created)
		}
		code, _ = c.do("DELETE", "/v1/admin/risk/denylist/"+id, "admin:ada",
			map[string]any{"reason": "false positive after review"}, nil)
		if code != 204 {
			t.Fatalf("delete %d", code)
		}
		if code, _ := c.do("PUT", "/v1/admin/risk/rules/velocity_user", "operator:ola",
			map[string]any{"threshold": 0}, nil); code != 400 {
			t.Fatalf("missing reason: %d", code)
		}
		code, _ = c.do("PUT", "/v1/admin/risk/rules/velocity_user", "operator:ola",
			map[string]any{"reason": "block card testing during pilot", "threshold": 0}, nil)
		if code != 200 {
			t.Fatalf("zero threshold %d", code)
		}
		code, m := c.do("POST", "/v1/meters", "ada", map[string]any{"meter_number": "61100000021", "disco_code": "IKEDC", "label": "Home"}, nil)
		if code != 201 {
			t.Fatalf("register %d %v", code, m)
		}
		code, q := c.do("POST", "/v1/electricity/quotes", "ada", map[string]any{"meter_id": m["meter_id"], "amount_minor": 500000, "currency": "NGN"}, nil)
		if code != 201 {
			t.Fatalf("quote %d %v", code, q)
		}
		code, denied := c.do("POST", "/v1/electricity/transactions", "ada",
			map[string]any{"quote_id": q["quote_id"], "email": "ada@example.com"},
			map[string]string{"Idempotency-Key": "risk-deny-0123456789ab"})
		if code != 403 {
			t.Fatalf("DENY want 403, got %d %v", code, denied)
		}
		errObj, _ := denied["error"].(map[string]any)
		if errObj["code"] != "FORBIDDEN" {
			t.Fatalf("DENY code %v", denied)
		}
	})
}

func TestRiskPublicLinkFailuresPause(t *testing.T) {
	st := memstore.New()
	if err := st.Repos().Risk().UpdateRule(context.Background(), risk.Rule{
		Code: risk.RulePublicLinkFailures, Enabled: true, Action: risk.Deny, WindowSeconds: 900, Threshold: 2,
	}); err != nil {
		t.Fatal(err)
	}
	eng := risk.NewEngine(st.Repos().Risk(), nil)
	link := uuid.New()
	req := risk.Request{Surface: risk.SurfacePayForMe, LinkID: link}
	if err := eng.RecordFailure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordFailure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Evaluate(context.Background(), req); err == nil {
		t.Fatal("expected deny after pause")
	}
}

func TestDisputeAdminList(t *testing.T) {
	st := memstore.New()
	c, o := newRiskEnv(t, st)
	if err := o.HandlePaymentDispute(context.Background(), disputes.Notice{
		Type: "charge.dispute.create", PSPDisputeID: "77", Reference: "missing-ref", AmountMinor: 1000, Currency: "NGN",
	}); err != nil {
		t.Fatal(err)
	}
	code, list := c.do("GET", "/v1/admin/disputes", "operator:ola", nil, nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("want 1 dispute, got %v", list)
	}
	row := data[0].(map[string]any)
	id, _ := row["id"].(string)
	code, ev := c.do("GET", "/v1/admin/disputes/"+id+"/evidence", "operator:ola", nil, nil)
	if code != 200 {
		t.Fatalf("evidence %d %v", code, ev)
	}
	if _, ok := ev["token"]; ok {
		t.Fatal("evidence pack must not include a token")
	}
}
