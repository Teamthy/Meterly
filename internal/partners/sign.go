package partners

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"
)

func GenerateWebhookSecret() (full string, raw []byte, err error) {
	raw = make([]byte, SecretBytes)
	if _, err = io.ReadFull(rand.Reader, raw); err != nil {
		return "", nil, err
	}
	full = WebhookSecretPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return full, raw, nil
}

func webhookHMACKey(full string) ([]byte, error) {
	full = strings.TrimSpace(full)
	if !strings.HasPrefix(full, WebhookSecretPrefix) {
		return nil, ErrInvalidInput
	}
	raw, err := base64.RawURLEncoding.DecodeString(full[len(WebhookSecretPrefix):])
	if err != nil || len(raw) < SecretBytes {
		return nil, ErrInvalidInput
	}
	return raw, nil
}

func SignBody(secrets []string, ts int64, body []byte) (string, error) {
	if len(secrets) == 0 {
		return "", ErrInvalidInput
	}
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(ts, 10))
	msg := signingMessage(ts, body)
	for _, s := range secrets {
		key, err := webhookHMACKey(s)
		if err != nil {
			return "", err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(msg)
		b.WriteString(",v1=")
		b.WriteString(hex.EncodeToString(mac.Sum(nil)))
	}
	return b.String(), nil
}

func signingMessage(ts int64, body []byte) []byte {
	prefix := strconv.FormatInt(ts, 10) + "."
	out := make([]byte, len(prefix)+len(body))
	copy(out, prefix)
	copy(out[len(prefix):], body)
	return out
}

// VerifySignature checks Meterly-Signature (t=...,v1=...) against any of the
// secrets within the replay window. Used by partners and the demo script.
func VerifySignature(secrets []string, header string, body []byte, now time.Time, window time.Duration) error {
	if window <= 0 {
		window = WebhookReplayWindow
	}
	ts, sigs, err := parseSignatureHeader(header)
	if err != nil {
		return err
	}
	diff := now.UTC().Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(window.Seconds()) {
		return ErrInvalidInput
	}
	msg := signingMessage(ts, body)
	for _, secret := range secrets {
		key, err := webhookHMACKey(secret)
		if err != nil {
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(msg)
		want := mac.Sum(nil)
		for _, got := range sigs {
			if subtle.ConstantTimeCompare(want, got) == 1 {
				return nil
			}
		}
	}
	return ErrInvalidInput
}

func parseSignatureHeader(h string) (int64, [][]byte, error) {
	var ts int64
	var haveTS bool
	var sigs [][]byte
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, nil, ErrInvalidInput
			}
			ts = n
			haveTS = true
		case "v1":
			raw, err := hex.DecodeString(v)
			if err != nil || len(raw) != sha256.Size {
				continue
			}
			sigs = append(sigs, raw)
		}
	}
	if !haveTS || len(sigs) == 0 {
		return 0, nil, ErrInvalidInput
	}
	return ts, sigs, nil
}
