package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func partnerHarness(t *testing.T, partnerPerMin int) (*httpapi.Server, client, *partners.Engine, *syncBuf) {
	t.Helper()
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &partners.Engine{Repo: st.Repos().Partners(), Box: box, Environment: partners.EnvSandbox}
	s := &httpapi.Server{
		O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Partners: eng,
		PartnerEnvironment: partners.EnvSandbox,
		Limits: httpapi.Limits{
			WritesPerMinute: 6000, AuthFailuresPerMinute: 6000, PartnerPerMinute: partnerPerMin,
		},
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return s, client{t, srv}, eng, logs
}

func TestPartnerAdminIssueAuthScopeRateLimitAndAudit(t *testing.T) {
	_, c, _, logs := partnerHarness(t, 60)

	code, body := c.do("POST", "/v1/admin/partners", "ada", map[string]any{
		"reason": "onboard acme sandbox", "organisation": "Acme", "environment": "SANDBOX", "contact_ref": "t-1",
	}, nil)
	if code != 403 {
		t.Fatalf("customer create %d %v", code, body)
	}

	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "onboard acme sandbox", "organisation": "Acme", "environment": "SANDBOX",
		"contact_ref": "t-1", "daily_value_cap_minor": 500000, "rate_limit_per_minute": 60,
	}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	pid := created["id"].(string)
	if created["status"] != partners.StatusActive {
		t.Fatalf("sandbox %+v", created)
	}

	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "first sandbox key",
		"scopes": []string{partners.ScopeMetersValidate, partners.ScopeFloatRead},
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	secret, _ := issued["secret"].(string)
	prefix, _ := issued["prefix"].(string)
	if secret == "" || !strings.HasPrefix(secret, partners.PrefixTest) {
		t.Fatalf("secret %v", issued)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("plaintext key logged")
	}

	code, listed := c.do("GET", "/v1/admin/partners/"+pid+"/keys", "operator:ada", nil, nil)
	if code != 200 {
		t.Fatalf("list keys %d %v", code, listed)
	}
	raw, _ := json.Marshal(listed)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "secret_hash") {
		t.Fatalf("list leaked secret or hash: %s", raw)
	}

	code, me := c.do("GET", "/v1/partner/me", secret, nil, nil)
	if code != 200 || me["partner_id"] != pid || me["key_prefix"] != prefix {
		t.Fatalf("me %d %v", code, me)
	}
	scopes, _ := me["scopes"].([]any)
	if len(scopes) != 2 {
		t.Fatalf("scopes %v", me["scopes"])
	}

	code, userMe := c.do("GET", "/v1/me", secret, nil, nil)
	if code != 401 {
		t.Fatalf("partner on user route %d %v", code, userMe)
	}

	code, usage := c.do("GET", "/v1/admin/partners/"+pid+"/usage", "operator:ada", nil, nil)
	if code != 200 {
		t.Fatalf("usage %d %v", code, usage)
	}
	rows, _ := usage["usage"].([]any)
	if len(rows) == 0 {
		t.Fatalf("expected usage row %v", usage)
	}

	code, got := c.do("GET", "/v1/admin/partners/"+pid, "operator:ada", nil, nil)
	if code != 200 || got["id"] != pid {
		t.Fatalf("get %d %v", code, got)
	}
	if _, ok := created["secret"]; ok {
		t.Fatal("create returned a key")
	}
}

func TestPartnerRevokedExpiredSuspendedAndEnvMismatch(t *testing.T) {
	s, c, eng, _ := partnerHarness(t, 60)
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "sandbox partner", "organisation": "Beta", "environment": "SANDBOX",
	}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	pid := created["id"].(string)
	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "issue key", "scopes": []string{partners.ScopeFloatRead},
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	secret := issued["secret"].(string)
	keyID := issued["id"].(string)

	code, _ = c.do("POST", "/v1/admin/partners/"+pid+"/keys/"+keyID+"/revoke", "operator:ada", map[string]any{
		"reason": "compromised credential",
	}, nil)
	if code != 200 {
		t.Fatalf("revoke %d", code)
	}
	if code, body := c.do("GET", "/v1/partner/me", secret, nil, nil); code != 401 {
		t.Fatalf("revoked %d %v", code, body)
	}

	code, issued2 := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "replacement key", "scopes": []string{partners.ScopeFloatRead},
	}, nil)
	if code != 201 {
		t.Fatalf("issue2 %d %v", code, issued2)
	}
	secret2 := issued2["secret"].(string)
	eng.Environment = partners.EnvProduction
	s.Partners.Environment = partners.EnvProduction
	if code, body := c.do("GET", "/v1/partner/me", secret2, nil, nil); code != 401 {
		t.Fatalf("sandbox key on production process %d %v", code, body)
	}
	eng.Environment = partners.EnvSandbox
	s.Partners.Environment = partners.EnvSandbox

	code, _ = c.do("POST", "/v1/admin/partners/"+pid+"/suspend", "operator:ada", map[string]any{
		"reason": "contract paused today",
	}, nil)
	if code != 200 {
		t.Fatalf("suspend %d", code)
	}
	if code, body := c.do("GET", "/v1/partner/me", secret2, nil, nil); code != 401 {
		t.Fatalf("suspended %d %v", code, body)
	}
}

func TestPartnerRotationOverlapHTTP(t *testing.T) {
	_, c, _, _ := partnerHarness(t, 60)
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "rotate test org", "organisation": "Gamma", "environment": "SANDBOX",
	}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	pid := created["id"].(string)
	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "original key", "scopes": []string{partners.ScopeWebhooksManage},
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	oldSecret := issued["secret"].(string)
	keyID := issued["id"].(string)
	code, rotated := c.do("POST", "/v1/admin/partners/"+pid+"/keys/"+keyID+"/rotate", "operator:ada", map[string]any{
		"reason": "scheduled rotation", "overlap_seconds": 3600,
	}, nil)
	if code != 201 {
		t.Fatalf("rotate %d %v", code, rotated)
	}
	newSecret := rotated["secret"].(string)
	if oldSecret == newSecret {
		t.Fatal("rotate returned same secret")
	}
	if code, _ := c.do("GET", "/v1/partner/me", oldSecret, nil, nil); code != 200 {
		t.Fatalf("old during overlap %d", code)
	}
	if code, _ := c.do("GET", "/v1/partner/me", newSecret, nil, nil); code != 200 {
		t.Fatalf("new during overlap %d", code)
	}
}

func TestPartnerProductionTwoPersonAndFeatureFlag(t *testing.T) {
	_, c, _, logs := partnerHarness(t, 60)
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "production partner kyb", "organisation": "Live Co", "environment": "PRODUCTION",
	}, nil)
	if code != 201 || created["status"] != partners.StatusPending {
		t.Fatalf("create prod %d %v", code, created)
	}
	pid := created["id"].(string)
	code, keys := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "too early", "scopes": []string{partners.ScopeFloatRead},
	}, nil)
	if code != 409 {
		t.Fatalf("issue pending %d %v", code, keys)
	}
	code, proposed := c.do("POST", "/v1/admin/partners/"+pid+"/activate", "operator:ada", map[string]any{
		"reason": "propose production go-live",
	}, nil)
	if code != 200 || proposed["status"] != partners.StatusPending {
		t.Fatalf("propose %d %v", code, proposed)
	}
	code, same := c.do("POST", "/v1/admin/partners/"+pid+"/activate", "operator:ada", map[string]any{
		"reason": "cannot self approve live",
	}, nil)
	if code != 409 {
		t.Fatalf("same approver %d %v", code, same)
	}
	errObj, _ := same["error"].(map[string]any)
	if errObj["code"] != "SAME_APPROVER" {
		t.Fatalf("code %v", same)
	}
	code, active := c.do("POST", "/v1/admin/partners/"+pid+"/activate", "operator:bola", map[string]any{
		"reason": "second operator approves live",
	}, nil)
	if code != 200 || active["status"] != partners.StatusActive {
		t.Fatalf("approve %d %v", code, active)
	}

	offSt := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), offSt, registry.New(offSt, box, vend, remote), box, pay, vend, remote, log)
	off := &httpapi.Server{
		O: o, Auth: auth.DevAuth{}, Log: log, PartnerDisabled: true,
		Limits: httpapi.Limits{WritesPerMinute: 6000, AuthFailuresPerMinute: 6000},
	}
	offSrv := httptest.NewServer(off.Routes())
	defer offSrv.Close()
	cOff := client{t, offSrv}
	code, disabled := cOff.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "should 404", "organisation": "Nope", "environment": "SANDBOX",
	}, nil)
	if code != 404 {
		t.Fatalf("disabled admin %d %v", code, disabled)
	}
	code, disabledMe := cOff.do("GET", "/v1/partner/me", "mtr_test_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, nil)
	if code != 404 {
		t.Fatalf("disabled me %d %v", code, disabledMe)
	}
	_ = logs
}

func TestPartnerRateLimit(t *testing.T) {
	_, c, _, _ := partnerHarness(t, 2)
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "rate limit partner", "organisation": "Delta", "environment": "SANDBOX",
	}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	pid := created["id"].(string)
	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "rate key", "scopes": []string{partners.ScopeFloatRead},
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	secret := issued["secret"].(string)
	var limited bool
	for i := 0; i < 8; i++ {
		code, body := c.do("GET", "/v1/partner/me", secret, nil, nil)
		if code == 429 {
			limited = true
			if errObj, _ := body["error"].(map[string]any); errObj["code"] != "RATE_LIMITED" {
				t.Fatalf("want RATE_LIMITED got %v", body)
			}
			break
		}
		if code != 200 {
			t.Fatalf("me %d %v", code, body)
		}
	}
	if !limited {
		t.Fatal("expected partner rate limit")
	}
}

func TestPartnerPlaintextNeverInAudit(t *testing.T) {
	s, c, _, logs := partnerHarness(t, 60)
	code, created := c.do("POST", "/v1/admin/partners", "operator:ada", map[string]any{
		"reason": "audit secret check", "organisation": "Echo", "environment": "SANDBOX",
	}, nil)
	if code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	pid := created["id"].(string)
	code, issued := c.do("POST", "/v1/admin/partners/"+pid+"/keys", "operator:ada", map[string]any{
		"reason": "show once only", "scopes": []string{partners.ScopeMetersValidate},
	}, nil)
	if code != 201 {
		t.Fatalf("issue %d %v", code, issued)
	}
	secret := issued["secret"].(string)
	events, err := s.O.Store().Repos().Audit().Recent(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(events)
	if strings.Contains(string(blob), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("plaintext key in audit or logs")
	}
	found := false
	for _, e := range events {
		if e.Action == "partner.key.issued" {
			found = true
			if e.Metadata["prefix"] == nil || e.Metadata["reason"] == nil {
				t.Fatalf("audit %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("missing issued audit: %+v", events)
	}
}
