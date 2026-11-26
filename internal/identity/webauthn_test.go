package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestParseCOSEAndVerify(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x := priv.X.FillBytes(make([]byte, 32))
	y := priv.Y.FillBytes(make([]byte, 32))
	pubXY := append(x, y...)

	rpID := "localhost"
	challenge := []byte("0123456789abcdef0123456789abcdef")
	origin := "http://localhost:3000"
	cd := clientData{Type: "webauthn.get", Challenge: b64u(challenge), Origin: origin}
	rawCD, _ := json.Marshal(cd)
	if err := parseClientData(rawCD, "webauthn.get", origin, challenge); err != nil {
		t.Fatal(err)
	}

	auth := make([]byte, 37)
	copy(auth[:32], rpIDHash(rpID))
	auth[32] = 0x01
	binary.BigEndian.PutUint32(auth[33:37], 2)
	cdh := sha256.Sum256(rawCD)
	msg := append(append([]byte{}, auth...), cdh[:]...)
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	if err := verifyES256(pubXY, auth, rawCD, sig); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingOrder(t *testing.T) {
	if !CanAdvance(StepWelcome, StepAccount) || CanAdvance(StepWelcome, StepMeter) {
		t.Fatal("step order")
	}
	n, err := NextStep(StepAlerts)
	if err != nil || n != StepDone {
		t.Fatalf("%s %v", n, err)
	}
}
