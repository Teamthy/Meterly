package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestSecurityConsoleAndStepUpGate(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &identity.Engine{Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box, Dev: true}
	sec := &security.Engine{Store: st.Repos().Auth(), Identity: eng, Box: box}
	s := &httpapi.Server{
		O: orch, Auth: auth.SessionThen(auth.DevAuth{}, eng), Identity: eng, Security: sec, Log: log,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()
	do := func(method, path, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := do("GET", "/v1/security/console", "ada")
	if rr.Code != 200 {
		t.Fatalf("console %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "This device") {
		t.Fatalf("console body %s", rr.Body.String())
	}

	rr = do("GET", "/v1/electricity/transactions/00000000-0000-0000-0000-000000000001/token", "ada")
	if rr.Code == 403 && strings.Contains(rr.Body.String(), "STEP_UP_REQUIRED") {
		t.Fatalf("dev bearer should not require step-up: %s", rr.Body.String())
	}

	origin := "http://localhost:3000"
	raw, _ := json.Marshal(map[string]any{"display_name": "Ada", "origin": origin})
	req := httptest.NewRequest("POST", "/v1/auth/passkeys/register/begin", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("begin register %d %s", rr.Code, rr.Body.String())
	}
}

func TestSecurityLastPasskeyDenied(t *testing.T) {
	e := &security.Engine{}
	d := e.Evaluate(nil, security.Request{Action: security.ActionDeletePasskey, Passkeys: 1})
	if d.Outcome != security.OutcomeDeny {
		t.Fatalf("got %s", d.Outcome)
	}
}
