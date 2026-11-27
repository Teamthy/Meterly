package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/erasure"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/privacy"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func TestDeleteAccountCoolingOffCancelAndExportRefused(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	s := &httpapi.Server{
		O: o, Auth: auth.DevAuth{}, Payments: pay, Log: log, Dev: true, DevPay: pay, DevRemote: remote,
		ErasureCooling: 7 * 24 * time.Hour, Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	c := client{t, srv}

	code, body := c.do("POST", "/v1/me/delete-account", "ada", map[string]any{"reason": "moving"}, nil)
	if code != 201 {
		t.Fatalf("delete %d %v", code, body)
	}
	if body["status"] != privacy.ErasureCoolingOff {
		t.Fatalf("status %v", body["status"])
	}
	code, again := c.do("POST", "/v1/me/delete-account", "ada", map[string]any{}, nil)
	if code != 200 || again["erasure_id"] != body["erasure_id"] {
		t.Fatalf("idempotent %d %v", code, again)
	}
	code, me := c.do("GET", "/v1/me", "ada", nil, nil)
	if code != 200 {
		t.Fatalf("me %d %v", code, me)
	}
	er, _ := me["erasure"].(map[string]any)
	if er == nil || er["status"] != privacy.ErasureCoolingOff {
		t.Fatalf("pending state %+v", me)
	}

	code, cancelled := c.do("POST", "/v1/me/delete-account/cancel", "ada", nil, nil)
	if code != 200 || cancelled["status"] != privacy.ErasureCancelled {
		t.Fatalf("cancel %d %v", code, cancelled)
	}
	code, me2 := c.do("GET", "/v1/me", "ada", nil, nil)
	if code != 200 || me2["erasure"] != nil {
		t.Fatalf("after cancel still pending %+v", me2)
	}

	code, body = c.do("POST", "/v1/me/delete-account", "bob", nil, nil)
	if code != 201 {
		t.Fatalf("bob delete %d %v", code, body)
	}
	ctx := context.Background()
	uid, err := st.Repos().Users().EnsureByExternalID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	n, err := erasure.Process(ctx, st, time.Now().UTC().Add(8*24*time.Hour), 10, log)
	if err != nil || n != 1 {
		t.Fatalf("process n=%d err=%v", n, err)
	}
	stt, _ := st.Repos().Users().Status(ctx, uid)
	if stt != store.UserErased {
		t.Fatalf("bob status %s", stt)
	}

	cam, err := st.Repos().Users().EnsureByExternalID(ctx, "cam")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Repos().Users().SetStatus(ctx, cam, store.UserErased); err != nil {
		t.Fatal(err)
	}
	code, denied := c.do("POST", "/v1/me/data-export", "cam", map[string]any{}, nil)
	if code != 409 {
		t.Fatalf("export erased %d %v", code, denied)
	}
	errObj, _ := denied["error"].(map[string]any)
	if errObj["code"] != "ACCOUNT_ERASED" {
		t.Fatalf("export code %v", denied)
	}

	off := &httpapi.Server{
		O: o, Auth: auth.DevAuth{}, Log: log, ErasureDisabled: true,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	offSrv := httptest.NewServer(off.Routes())
	defer offSrv.Close()
	cOff := client{t, offSrv}
	code, disabled := cOff.do("POST", "/v1/me/delete-account", "ada", nil, nil)
	if code != 404 {
		t.Fatalf("disabled %d %v", code, disabled)
	}
}

func TestDeleteAccountRequiresStepUp(t *testing.T) {
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
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
	const origin = "http://localhost:3000"
	key := newSoftKey(t, "erase-key")
	begin := httptest.NewRequest("POST", "/v1/auth/passkeys/register/begin", strings.NewReader(`{"display_name":"Ada","origin":"`+origin+`"}`))
	begin.Header.Set("Content-Type", "application/json")
	begin.Header.Set("Origin", origin)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, begin)
	if rr.Code != 200 {
		t.Fatalf("begin %d %s", rr.Code, rr.Body.String())
	}
	var opt map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &opt)
	finBody, _ := json.Marshal(map[string]any{"credential": key.attestation(opt["challenge"].(string), origin)})
	fin := httptest.NewRequest("POST", "/v1/auth/passkeys/register/finish", strings.NewReader(string(finBody)))
	fin.Header.Set("Content-Type", "application/json")
	fin.Header.Set("Origin", origin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, fin)
	if rr.Code != 200 {
		t.Fatalf("finish %d %s", rr.Code, rr.Body.String())
	}
	var reg map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &reg)
	tok := reg["token"].(string)

	req := httptest.NewRequest("POST", "/v1/me/delete-account", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "STEP_UP_REQUIRED") {
		t.Fatalf("want step-up, got %d %s", rr.Code, rr.Body.String())
	}
}
