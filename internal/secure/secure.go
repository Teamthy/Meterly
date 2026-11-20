// Package secure provides encryption for STS tokens and keyed hashing /
// masking for meter numbers. Raw tokens must never be logged or stored in
// plaintext.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// Box encrypts and decrypts sensitive values with AES-256-GCM.
type Box struct {
	aead       cipher.AEAD
	KeyVersion string
	hmacKey    []byte
}

// NewBox builds a Box from a 32-byte encryption key and a separate HMAC key.
func NewBox(encKey, hmacKey []byte, keyVersion string) (*Box, error) {
	if len(encKey) != 32 {
		return nil, errors.New("secure: encryption key must be 32 bytes")
	}
	if len(hmacKey) < 32 {
		return nil, errors.New("secure: hmac key must be at least 32 bytes")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, KeyVersion: keyVersion, hmacKey: hmacKey}, nil
}

// RandomKey returns n cryptographically random bytes (for dev/test only).
func RandomKey(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return b
}

func (b *Box) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (b *Box) Decrypt(ciphertext []byte) (string, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < ns {
		return "", errors.New("secure: ciphertext too short")
	}
	pt, err := b.aead.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Hash returns a keyed HMAC-SHA256 of v, suitable for indexed lookup
// without storing the identifier in plaintext.
func (b *Box) Hash(v string) string {
	m := hmac.New(sha256.New, b.hmacKey)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

// Mask shows only the last `visible` characters.
func Mask(v string, visible int) string {
	if len(v) <= visible {
		return strings.Repeat("*", len(v))
	}
	return strings.Repeat("*", len(v)-visible) + v[len(v)-visible:]
}
