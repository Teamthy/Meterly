package httpapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", rr.Header())
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

// softKey is a minimal in-process passkey authenticator for HTTP tests.
type softKey struct {
	priv   *ecdsa.PrivateKey
	credID []byte
	count  uint32
}

func newSoftKey(t *testing.T, id string) *softKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &softKey{priv: k, credID: []byte(id)}
}

func rawB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (k *softKey) authData(flags byte) []byte {
	sum := sha256.Sum256([]byte("localhost"))
	ad := append([]byte{}, sum[:]...)
	k.count++
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, k.count)
	return append(append(ad, flags), cnt...)
}

func (k *softKey) attestation(challenge, origin string) map[string]any {
	ch, _ := base64.RawURLEncoding.DecodeString(challenge)
	cd, _ := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": rawB64(ch), "origin": origin})
	pub := append(k.priv.X.FillBytes(make([]byte, 32)), k.priv.Y.FillBytes(make([]byte, 32))...)
	return map[string]any{"id": rawB64(k.credID), "rawId": rawB64(k.credID), "clientDataJSON": rawB64(cd),
		"authenticatorData": rawB64(k.authData(0x05)), "publicKey": rawB64(pub)}
}

func (k *softKey) assertion(challenge, origin string) map[string]any {
	ch, _ := base64.RawURLEncoding.DecodeString(challenge)
	cd, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": rawB64(ch), "origin": origin})
	ad := k.authData(0x05)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdh[:]...))
	r, s, _ := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return map[string]any{"id": rawB64(k.credID), "rawId": rawB64(k.credID), "clientDataJSON": rawB64(cd),
		"authenticatorData": rawB64(ad), "signature": rawB64(sig)}
}

// A passkey session is bound to the network it was stepped up from: replaying
// it elsewhere must require a new step-up, and enrolling another passkey on
// the account needs a fresh step-up too.
func TestStepUpBindsSessionToNetworkAndGatesAddingPasskeys(t *testing.T) {
	st := memstore.New()
	box, _ := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &identity.Engine{Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box, Dev: true}
	clock := time.Now().UTC()
	sec := &security.Engine{Store: st.Repos().Auth(), Identity: eng, Box: box, Now: func() time.Time { return clock }}
	s := &httpapi.Server{
		O: orch, Auth: auth.SessionThen(auth.DevAuth{}, eng), Identity: eng, Security: sec, Log: log,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()
	const origin = "http://localhost:3000"
	call := func(method, path, bearer, ip string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req := httptest.NewRequest(method, path, rd)
		req.RemoteAddr = ip + ":40000"
		req.Header.Set("Origin", origin)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}
	errCode := func(out map[string]any) string {
		e, _ := out["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}
	const ipA, ipB = "10.0.0.1", "203.0.113.9"
	key := newSoftKey(t, "phone-key")

	code, opt := call("POST", "/v1/auth/passkeys/register/begin", "", ipA, map[string]any{"display_name": "Ada"})
	if code != 200 {
		t.Fatalf("register begin %d %v", code, opt)
	}
	code, reg := call("POST", "/v1/auth/passkeys/register/finish", "", ipA, map[string]any{"credential": key.attestation(opt["challenge"].(string), origin)})
	if code != 200 {
		t.Fatalf("register finish %d %v", code, reg)
	}
	tok := reg["token"].(string)
	tokenPath := "/v1/electricity/transactions/" + uuid.NewString() + "/token"

	if code, out := call("GET", tokenPath, tok, ipA, nil); code != 403 || errCode(out) != "STEP_UP_REQUIRED" {
		t.Fatalf("token read before step-up: %d %v", code, out)
	}
	stepUp := func(ip string) {
		t.Helper()
		code, so := call("POST", "/v1/security/step-up/begin", tok, ip, map[string]any{})
		if code != 200 {
			t.Fatalf("step-up begin %d %v", code, so)
		}
		if code, out := call("POST", "/v1/security/step-up/finish", tok, ip, map[string]any{"credential": key.assertion(so["challenge"].(string), origin)}); code != 200 {
			t.Fatalf("step-up finish %d %v", code, out)
		}
	}
	stepUp(ipA)
	if code, out := call("GET", tokenPath, tok, ipA, nil); errCode(out) == "STEP_UP_REQUIRED" {
		t.Fatalf("fresh step-up on the bound network must pass the gate: %d %v", code, out)
	}
	// Replayed from another network: the gate must fire, every time.
	for i := 0; i < 2; i++ {
		if code, out := call("GET", tokenPath, tok, ipB, nil); code != 403 || errCode(out) != "STEP_UP_REQUIRED" {
			t.Fatalf("replay from new network (attempt %d) passed the gate: %d %v", i+1, code, out)
		}
	}
	// Stepping up on the new network re-binds the session there.
	stepUp(ipB)
	if code, out := call("GET", tokenPath, tok, ipB, nil); errCode(out) == "STEP_UP_REQUIRED" {
		t.Fatalf("after step-up on the new network: %d %v", code, out)
	}
	if code, out := call("GET", tokenPath, tok, ipA, nil); code != 403 || errCode(out) != "STEP_UP_REQUIRED" {
		t.Fatalf("old network after re-bind: %d %v", code, out)
	}

	// Adding a passkey to the account: allowed with a fresh step-up...
	if code, out := call("POST", "/v1/auth/passkeys/register/begin", tok, ipB, map[string]any{"display_name": "Ada"}); code != 200 {
		t.Fatalf("add passkey with fresh step-up: %d %v", code, out)
	}
	// ...but not once the step-up has gone stale.
	clock = clock.Add(security.DefaultStepUpAfter + time.Minute)
	if code, out := call("POST", "/v1/auth/passkeys/register/begin", tok, ipB, map[string]any{"display_name": "Ada"}); code != 403 || errCode(out) != "STEP_UP_REQUIRED" {
		t.Fatalf("adding a passkey without a fresh step-up: %d %v", code, out)
	}
}
