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

	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestMeOnboardingBeneficiariesAndPasskeys(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	readings := mock.NewMeterReadings()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	orch.SetMeterReadingProvider(readings)
	eng := &identity.Engine{
		Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box, Dev: true,
	}
	s := &httpapi.Server{
		O: orch, Auth: auth.SessionThen(auth.DevAuth{}, eng), Identity: eng,
		Payments: pay, Log: log, Dev: true, DevPay: pay, DevRemote: remote, DevReadings: readings,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()

	do := func(method, path, bearer string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rdr = strings.NewReader(string(raw))
		}
		req := httptest.NewRequest(method, path, rdr)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", "http://localhost:3000")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := do("GET", "/v1/me", "ada", nil)
	if rr.Code != 200 {
		t.Fatalf("me %d %s", rr.Code, rr.Body.String())
	}
	var me struct {
		Profile identity.Profile `json:"profile"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &me)
	if me.Profile.OnboardingStep != identity.StepWelcome {
		t.Fatalf("step %s", me.Profile.OnboardingStep)
	}

	rr = do("PATCH", "/v1/me", "ada", map[string]any{
		"display_name": "Adaeze Okafor", "email": "ada@meterly.ng", "phone": "+2348011111111",
	})
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Adaeze") {
		t.Fatalf("patch me %d %s", rr.Code, rr.Body.String())
	}

	rr = do("POST", "/v1/me/onboarding/advance", "ada", map[string]any{"step": "account"})
	if rr.Code != 200 {
		t.Fatalf("advance %d %s", rr.Code, rr.Body.String())
	}
	rr = do("POST", "/v1/me/onboarding/advance", "ada", map[string]any{"step": "alerts"})
	if rr.Code != 422 {
		t.Fatalf("skip want 422 got %d %s", rr.Code, rr.Body.String())
	}

	rr = do("POST", "/v1/meters", "ada", map[string]any{
		"meter_number": "70011112222", "disco_code": "IKEDC", "label": "Home",
	})
	if rr.Code != 201 {
		t.Fatalf("register %d %s", rr.Code, rr.Body.String())
	}
	var meter struct {
		MeterID string `json:"meter_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &meter)

	rr = do("GET", "/v1/beneficiaries", "ada", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), meter.MeterID) {
		t.Fatalf("beneficiaries %d %s", rr.Code, rr.Body.String())
	}

	rr = do("POST", "/v1/dev/meters/"+meter.MeterID+"/reading", "", map[string]any{"kwh": 142.5})
	if rr.Code != 200 {
		t.Fatalf("set reading %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/v1/meters/"+meter.MeterID+"/reading", "ada", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "142.5") {
		t.Fatalf("get reading %d %s", rr.Code, rr.Body.String())
	}

	rr = do("PATCH", "/v1/me/settings", "ada", map[string]any{
		"sms": true, "email_enabled": false, "whatsapp": true,
	})
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"whatsapp":true`) {
		t.Fatalf("settings %d %s", rr.Code, rr.Body.String())
	}

	origin := "http://localhost:3000"
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubXY := append(priv.X.FillBytes(make([]byte, 32)), priv.Y.FillBytes(make([]byte, 32))...)
	credID := []byte("http-cred")

	rr = do("POST", "/v1/auth/passkeys/register/begin", "", map[string]any{
		"display_name": "Chidi", "origin": origin,
	})
	if rr.Code != 200 {
		t.Fatalf("begin register %d %s", rr.Code, rr.Body.String())
	}
	var opt identity.PublicKeyOptions
	_ = json.Unmarshal(rr.Body.Bytes(), &opt)
	ch, _ := base64.RawURLEncoding.DecodeString(opt.Challenge)
	cd, _ := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": b64u(ch), "origin": origin})
	authData := make([]byte, 37)
	sum := sha256.Sum256([]byte("localhost"))
	copy(authData[:32], sum[:])
	authData[32] = 0x05 // user present + user verified
	rr = do("POST", "/v1/auth/passkeys/register/finish", "", map[string]any{
		"origin":            origin,
		"name":              "This phone",
		"id":                b64u(credID),
		"rawId":             b64u(credID),
		"clientDataJSON":    b64u(cd),
		"authenticatorData": b64u(authData),
		"publicKey":         b64u(pubXY),
	})
	if rr.Code != 200 {
		t.Fatalf("finish register %d %s", rr.Code, rr.Body.String())
	}
	var sess struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sess)
	if !strings.HasPrefix(sess.Token, identity.SessionPrefix) {
		t.Fatalf("token %q", sess.Token)
	}

	rr = do("GET", "/v1/me", sess.Token, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Chidi") {
		t.Fatalf("session me %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/v1/me/passkeys", sess.Token, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "This phone") {
		t.Fatalf("passkeys %d %s", rr.Code, rr.Body.String())
	}

	rr = do("POST", "/v1/auth/logout", sess.Token, nil)
	if rr.Code != 200 {
		t.Fatalf("logout %d %s", rr.Code, rr.Body.String())
	}
	rr = do("GET", "/v1/me", sess.Token, nil)
	if rr.Code != 401 {
		t.Fatalf("revoked session %d %s", rr.Code, rr.Body.String())
	}
}

func TestPasskeyLoginHTTP(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(orchestrator.DefaultConfig(), st, registry.New(st, box, vend, remote), box, pay, vend, remote, log)
	eng := &identity.Engine{Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box, Dev: true}
	s := &httpapi.Server{
		O: orch, Auth: auth.SessionThen(auth.DevAuth{}, eng), Identity: eng, Log: log,
		Limits: httpapi.Limits{WritesPerMinute: 6000},
	}
	h := s.Routes()
	origin := "http://localhost:3000"
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubXY := append(priv.X.FillBytes(make([]byte, 32)), priv.Y.FillBytes(make([]byte, 32))...)
	credID := []byte("login-cred")

	post := func(path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := post("/v1/auth/passkeys/register/begin", map[string]any{"display_name": "Kemi", "origin": origin})
	var opt identity.PublicKeyOptions
	_ = json.Unmarshal(rr.Body.Bytes(), &opt)
	ch, _ := base64.RawURLEncoding.DecodeString(opt.Challenge)
	cd, _ := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": b64u(ch), "origin": origin})
	ad := make([]byte, 37)
	sum := sha256.Sum256([]byte("localhost"))
	copy(ad[:32], sum[:])
	ad[32] = 0x05 // user present + user verified
	rr = post("/v1/auth/passkeys/register/finish", map[string]any{
		"origin": origin, "id": b64u(credID), "rawId": b64u(credID),
		"clientDataJSON": b64u(cd), "authenticatorData": b64u(ad), "publicKey": b64u(pubXY),
	})
	if rr.Code != 200 {
		t.Fatalf("register %d %s", rr.Code, rr.Body.String())
	}

	rr = post("/v1/auth/passkeys/login/begin", map[string]any{"origin": origin})
	if rr.Code != 200 {
		t.Fatalf("begin login %d %s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &opt)
	lch, _ := base64.RawURLEncoding.DecodeString(opt.Challenge)
	lcd, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": b64u(lch), "origin": origin})
	adGet := make([]byte, 37)
	copy(adGet[:32], sum[:])
	adGet[32] = 0x05 // user present + user verified
	binary.BigEndian.PutUint32(adGet[33:37], 2)
	cdh := sha256.Sum256(lcd)
	msg := append(append([]byte{}, adGet...), cdh[:]...)
	digest := sha256.Sum256(msg)
	r, sVal, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), sVal.FillBytes(make([]byte, 32))...)
	rr = post("/v1/auth/passkeys/login/finish", map[string]any{
		"origin": origin, "id": b64u(credID), "rawId": b64u(credID),
		"authenticatorData": b64u(adGet), "clientDataJSON": b64u(lcd), "signature": b64u(sig),
	})
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), identity.SessionPrefix) {
		t.Fatalf("login %d %s", rr.Code, rr.Body.String())
	}
}
