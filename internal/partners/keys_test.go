package partners

import (
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/secure"
)

func TestGenerateAndHashNeverEqualsPlaintext(t *testing.T) {
	box, err := secure.NewBox(secure.RandomKey(32), secure.RandomKey(32), "t1")
	if err != nil {
		t.Fatal(err)
	}
	full, prefix, err := GenerateSecret(EnvSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, PrefixTest) {
		t.Fatalf("prefix %s", full)
	}
	if err := ValidatePlaintext(full); err != nil {
		t.Fatal(err)
	}
	if prefix == full || !strings.HasPrefix(full, prefix) {
		t.Fatalf("lookup prefix %q full %q", prefix, full)
	}
	sum := HashSecret(box, full)
	if sum == "" || sum == full || strings.Contains(full, sum) || strings.Contains(sum, full) {
		t.Fatalf("hash leaked plaintext: hash=%s full=%s", sum, full)
	}
	if EqualHash(sum, full) {
		t.Fatal("hash equalled plaintext")
	}
	if !EqualHash(sum, HashSecret(box, full)) {
		t.Fatal("hash not stable")
	}
	other, _, err := GenerateSecret(EnvSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if EqualHash(sum, HashSecret(box, other)) {
		t.Fatal("distinct keys hashed equal")
	}
	live, livePrefix, err := GenerateSecret(EnvProduction)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(live, PrefixLive) || !strings.HasPrefix(livePrefix, PrefixLive) {
		t.Fatalf("live %s %s", live, livePrefix)
	}
}

func TestLooksLikeKey(t *testing.T) {
	if LooksLikeKey("ada") || LooksLikeKey("mls_abc") || LooksLikeKey("mtr_") {
		t.Fatal("false positive")
	}
	full, _, err := GenerateSecret(EnvSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeKey(full) {
		t.Fatal("want true")
	}
}
