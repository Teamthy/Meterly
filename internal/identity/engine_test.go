package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store/memstore"
)

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestPasskeyRegisterLoginAndOnboarding(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	eng := &identity.Engine{
		Users: st.Repos().Users(),
		Auth:  st.Repos().Auth(),
		Box:   box,
		Dev:   true,
	}
	ctx := context.Background()
	origin := "http://localhost:3000"

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubXY := append(priv.X.FillBytes(make([]byte, 32)), priv.Y.FillBytes(make([]byte, 32))...)
	credID := []byte("device-cred-1")

	opt, userID, err := eng.BeginRegister(ctx, origin, "Adaeze", "")
	if err != nil {
		t.Fatal(err)
	}
	if userID == "" || opt.Challenge == "" {
		t.Fatal("missing challenge")
	}
	ch, err := base64.RawURLEncoding.DecodeString(opt.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	rawCD, _ := json.Marshal(map[string]string{"type": "webauthn.create", "challenge": b64u(ch), "origin": origin})
	auth := make([]byte, 37)
	sum := sha256.Sum256([]byte("localhost"))
	copy(auth[:32], sum[:])
	auth[32] = 0x01
	token, sess, err := eng.FinishRegister(ctx, origin, identity.AttestationResponse{
		ID: b64u(credID), RawID: b64u(credID),
		ClientDataJSON:    b64u(rawCD),
		AuthenticatorData: b64u(auth),
		PublicKey:         b64u(pubXY),
	}, "Pixel")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || sess.UserID != userID {
		t.Fatalf("session %q %+v", token, sess)
	}
	if _, err := eng.LookupSession(ctx, token); err != nil {
		t.Fatal(err)
	}

	loginOpt, err := eng.BeginLogin(ctx, origin)
	if err != nil {
		t.Fatal(err)
	}
	lch, err := base64.RawURLEncoding.DecodeString(loginOpt.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	rawLCD, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": b64u(lch), "origin": origin})
	authGet := make([]byte, 37)
	copy(authGet[:32], sum[:])
	authGet[32] = 0x01
	binary.BigEndian.PutUint32(authGet[33:37], 1)
	cdh := sha256.Sum256(rawLCD)
	msg := append(append([]byte{}, authGet...), cdh[:]...)
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	token2, _, err := eng.FinishLogin(ctx, origin, identity.AssertionResponse{
		ID: b64u(credID), RawID: b64u(credID),
		AuthenticatorData: b64u(authGet),
		ClientDataJSON:    b64u(rawLCD),
		Signature:         b64u(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	if token2 == token {
		t.Fatal("expected a new session token")
	}

	p, err := eng.Users.Profile(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if p.OnboardingStep != identity.StepAccount {
		t.Fatalf("step %s", p.OnboardingStep)
	}
	if _, err := eng.AdvanceOnboarding(ctx, userID, identity.StepProfile); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.AdvanceOnboarding(ctx, userID, identity.StepDone); err == nil {
		t.Fatal("skipped steps")
	}
	if _, err := eng.AdvanceOnboarding(ctx, userID, identity.StepMeter); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.AdvanceOnboarding(ctx, userID, identity.StepAlerts); err != nil {
		t.Fatal(err)
	}
	done, err := eng.AdvanceOnboarding(ctx, userID, identity.StepDone)
	if err != nil || done.OnboardingCompletedAt == nil {
		t.Fatalf("%+v %v", done, err)
	}

	keys, err := eng.ListPasskeys(ctx, userID)
	if err != nil || len(keys) != 1 || keys[0].Name != "Pixel" {
		t.Fatalf("%+v %v", keys, err)
	}
	if err := eng.Logout(ctx, token2); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.LookupSession(ctx, token2); err == nil {
		t.Fatal("revoked session still valid")
	}
}

func TestBeginRegisterRejectsUnknownOrigin(t *testing.T) {
	st := memstore.New()
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	eng := &identity.Engine{
		Users: st.Repos().Users(), Auth: st.Repos().Auth(), Box: box,
		Origins: []string{"https://app.meterly.ng"},
	}
	_, _, err = eng.BeginRegister(context.Background(), "https://evil.example", "Ada", "")
	if err != identity.ErrOrigin {
		t.Fatalf("got %v", err)
	}
}
