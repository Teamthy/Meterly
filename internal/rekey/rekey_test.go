package rekey_test

import (
	"testing"

	"github.com/teamthy/meterly/internal/rekey"
	"github.com/teamthy/meterly/internal/secure"
)

func TestReencryptUnboundAndBound(t *testing.T) {
	hmac := secure.RandomKey(32)
	oldB, err := secure.NewBox(secure.RandomKey(32), hmac, "v1")
	if err != nil {
		t.Fatal(err)
	}
	newB, err := secure.NewBox(secure.RandomKey(32), hmac, "v2")
	if err != nil {
		t.Fatal(err)
	}
	ct, err := oldB.Encrypt("20digitstokenvaluexx")
	if err != nil {
		t.Fatal(err)
	}
	next, err := rekey.ReencryptUnbound(oldB, newB, ct)
	if err != nil {
		t.Fatal(err)
	}
	got, err := newB.Decrypt(next)
	if err != nil || got != "20digitstokenvaluexx" {
		t.Fatalf("unbound %q %v", got, err)
	}
	if _, err := oldB.Decrypt(next); err == nil {
		t.Fatal("old box should not open new ciphertext")
	}
	aad := []byte("gift-contact:not-a-secret")
	bct, err := oldB.EncryptBound("08012345678", aad)
	if err != nil {
		t.Fatal(err)
	}
	bnext, err := rekey.ReencryptBound(oldB, newB, bct, aad)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := newB.DecryptBound(bnext, aad)
	if err != nil || plain != "08012345678" {
		t.Fatalf("bound %q %v", plain, err)
	}
}
