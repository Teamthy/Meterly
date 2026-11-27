package payrequests_test

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/secure"
)

func TestGenerateCodeAndHMACDomainSeparation(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := payrequests.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := payrequests.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 || len(c1) != payrequests.CodeLength {
		t.Fatalf("unexpected codes: %q %q", c1, c2)
	}
	linkHMAC, ok := payrequests.LinkCodeHMAC(box, strings.ToLower("  "+c1+"  "))
	if !ok {
		t.Fatal("expected normalized link code to succeed")
	}
	statusHMAC, ok := payrequests.StatusCodeHMAC(box, c1)
	if !ok {
		t.Fatal("expected status code HMAC to succeed")
	}
	if linkHMAC == statusHMAC {
		t.Fatal("link code HMAC and status code HMAC must be domain-separated")
	}
	if _, ok := payrequests.LinkCodeHMAC(box, "short"); ok {
		t.Fatal("short code must fail")
	}
	if _, ok := payrequests.LinkCodeHMAC(box, strings.Repeat("1", 26)); ok {
		t.Fatal("non-base32 digit '1' must fail")
	}
}

func TestValidateNoteAntiPhishingAndLength(t *testing.T) {
	valid := []string{
		"",
		"Light don go, abeg 🙏",
		"Flat 2B. Please help with ₦5,000.00 before 7 a.m.!",
	}
	for _, s := range valid {
		if _, err := payrequests.ValidateNote(s); err != nil {
			t.Errorf("ValidateNote(%q) unexpected error: %v", s, err)
		}
	}

	invalid := []string{
		"Pay here https://evil.example/pay",
		"Visit www.paystack-fake.com now",
		"Open meterly.ng/fake",
		"Click bit.ly/abc123",
		"wa.me/2348012345678",
		strings.Repeat("a", 141),
	}
	for _, s := range invalid {
		if _, err := payrequests.ValidateNote(s); err == nil {
			t.Errorf("ValidateNote(%q) expected error, got nil", s)
		}
	}
}

func TestBoundEncryptionContextIsolation(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	id1, id2 := uuid.New(), uuid.New()
	ct, kv, err := payrequests.SealNote(box, id1, "Light don go 🙏")
	if err != nil || kv != "v1" {
		t.Fatalf("seal note: %v", err)
	}
	got, err := payrequests.OpenNote(box, id1, ct)
	if err != nil || got != "Light don go 🙏" {
		t.Fatalf("open note: %q %v", got, err)
	}
	if _, err := payrequests.OpenNote(box, id2, ct); err == nil {
		t.Fatal("ciphertext bound to id1 must fail authentication under id2")
	}
}

func TestEncodeQRPNGBase64ProducesValidPNG(t *testing.T) {
	code, err := payrequests.GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	b64, err := payrequests.EncodeQRPNGBase64("https://meterly.ng/p/" + code)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("invalid PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() < 100 || b.Dx() != b.Dy() {
		t.Fatalf("unexpected QR PNG dimensions: %dx%d", b.Dx(), b.Dy())
	}
}
