package partners

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"strings"

	"github.com/teamthy/meterly/internal/secure"
)

const hashDomain = "partner-key:"

// GenerateSecret returns a new partner API key for env. The secret is 32
// cryptographically random bytes, encoded base64url, shown once.
func GenerateSecret(env string) (full, prefix string, err error) {
	head, err := PrefixForEnv(env)
	if err != nil {
		return "", "", err
	}
	raw := make([]byte, SecretBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	full = head + body
	prefix, err = LookupPrefix(full)
	return full, prefix, err
}

// LookupPrefix is the indexed identifier stored with the hash. It is never
// enough to authenticate.
func LookupPrefix(full string) (string, error) {
	env, head, ok := EnvForPrefix(full)
	if !ok || env == "" {
		return "", ErrInvalidKey
	}
	body := full[len(head):]
	if len(body) < LookupSecretChars {
		return "", ErrInvalidKey
	}
	return head + body[:LookupSecretChars], nil
}

func HashSecret(box *secure.Box, full string) string {
	if box == nil {
		return ""
	}
	return box.Hash(hashDomain + full)
}

// EqualHash compares two hex HMAC digests in constant time.
func EqualHash(stored, computed string) bool {
	if stored == "" || computed == "" {
		return false
	}
	if len(stored) != len(computed) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(computed)) == 1
}

func LooksLikeKey(tok string) bool {
	tok = strings.TrimSpace(tok)
	_, _, ok := EnvForPrefix(tok)
	return ok
}

func ValidatePlaintext(full string) error {
	env, head, ok := EnvForPrefix(full)
	if !ok || env == "" {
		return ErrInvalidKey
	}
	body := full[len(head):]
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < SecretBytes {
		return ErrInvalidKey
	}
	return nil
}
