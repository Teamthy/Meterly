package httpapi_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestReadyzAndAdminPilot(t *testing.T) {
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}

	code, body := c.do("GET", "/readyz", "", nil, nil)
	if code != 200 || body["service"] != "ok" {
		t.Fatalf("readyz %d %v", code, body)
	}

	if code, _ := c.do("POST", "/v1/admin/pilot/pause", "operator:ola", map[string]any{"reason": "need admin for pause"}, nil); code != 403 {
		t.Fatalf("operator pause: %d", code)
	}
	code, paused := c.do("POST", "/v1/admin/pilot/pause", "admin:ada", map[string]any{"reason": "disco float incident tonight"}, nil)
	if code != 200 || paused["service_paused"] != true {
		t.Fatalf("pause %d %v", code, paused)
	}
	code, body = c.do("GET", "/readyz", "", nil, nil)
	if code != 200 || body["status"] != "degraded" || body["service"] != "paused" {
		t.Fatalf("readyz paused %d %v", code, body)
	}

	uid := "11111111-1111-1111-1111-111111111111"
	code, added := c.do("POST", "/v1/admin/pilot/allowlist", "admin:ada",
		map[string]any{"reason": "first ikeja household", "kind": "USER", "value": uid}, nil)
	if code != 201 {
		t.Fatalf("add %d %v", code, added)
	}
	code, listed := c.do("GET", "/v1/admin/pilot", "operator:ola", nil, nil)
	if code != 200 || listed["service_paused"] != true {
		t.Fatalf("list %d %v", code, listed)
	}
	al, _ := listed["allowlist"].([]any)
	if len(al) != 1 {
		t.Fatalf("allowlist %v", listed["allowlist"])
	}
	code, _ = c.do("DELETE", "/v1/admin/pilot/allowlist", "admin:ada",
		map[string]any{"reason": "household left the cohort", "kind": "USER", "value": uid}, nil)
	if code != 204 {
		t.Fatalf("delete %d", code)
	}
	code, resumed := c.do("POST", "/v1/admin/pilot/resume", "admin:ada", map[string]any{"reason": "float restored after topup"}, nil)
	if code != 200 || resumed["service_paused"] != false {
		t.Fatalf("resume %d %v", code, resumed)
	}
}
