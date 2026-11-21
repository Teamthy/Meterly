package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

const hexKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestDevDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.RunWorker || c.DatabaseURL != "" || c.HTTPAddr != ":8080" || c.Orchestrator.PaymentExpiry != 2*time.Hour {
		t.Fatalf("%+v", c)
	}
}

func TestModeRequired(t *testing.T) {
	if _, err := Load(env(nil)); err == nil || !strings.Contains(err.Error(), "MODE is required") {
		t.Fatalf("got %v", err)
	}
}

func TestProductionFailsFast(t *testing.T) {
	_, err := Load(env(map[string]string{"MODE": "production"}))
	for _, want := range []string{"DATABASE_URL is required", "TOKEN_ENC_KEY and METER_HMAC_KEY", "mock providers"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestBadValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"MODE": "dev", "TOKEN_ENC_KEY": "abc", "PAYMENT_EXPIRY": "soon", "RESUME_STALE_AFTER": "1s",
		"PAYMENT_PROVIDER": "acme", "RUN_WORKER": "maybe",
	}))
	for _, want := range []string{"TOKEN_ENC_KEY", "PAYMENT_EXPIRY", "RESUME_STALE_AFTER", "PAYMENT_PROVIDER", "RUN_WORKER"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestMemoryStoreNeedsInProcessWorker(t *testing.T) {
	_, err := Load(env(map[string]string{"MODE": "dev", "RUN_WORKER": "false"}))
	if err == nil || !strings.Contains(err.Error(), "in-memory store") {
		t.Fatalf("got %v", err)
	}
	c, err := Load(env(map[string]string{"MODE": "dev", "RUN_WORKER": "false", "DATABASE_URL": "postgres://x",
		"TOKEN_ENC_KEY": hexKey, "METER_HMAC_KEY": hexKey}))
	if err != nil || c.RunWorker || len(c.TokenEncKey) != 32 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestPaystackKeyMustMatchMode(t *testing.T) {
	base := map[string]string{"MODE": "dev", "PAYMENT_PROVIDER": "paystack"}
	if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "sk_test_") {
		t.Fatalf("missing key: %v", err)
	}
	base["PAYSTACK_SECRET_KEY"] = "sk_live_abc"
	if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "sk_test_") {
		t.Fatalf("live key in dev must be rejected: %v", err)
	}
	base["PAYSTACK_SECRET_KEY"] = "sk_test_abc"
	if c, err := Load(env(base)); err != nil || c.PaymentProvider != "paystack" {
		t.Fatalf("%v", err)
	}
	prod := map[string]string{"MODE": "production", "PAYMENT_PROVIDER": "paystack", "PAYSTACK_SECRET_KEY": "sk_test_abc", "PROVIDER_ENVIRONMENT": "PRODUCTION"}
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "sk_live_") {
		t.Fatalf("test key in production must be rejected: %v", err)
	}
}

func TestAuthConfig(t *testing.T) {
	c, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil || c.AuthMode != "dev" || len(c.OIDCMFAValues) == 0 {
		t.Fatalf("%v %+v", err, c)
	}
	_, err = Load(env(map[string]string{"MODE": "staging", "AUTH_MODE": "dev"}))
	if err == nil || !strings.Contains(err.Error(), "AUTH_MODE=dev is only allowed") {
		t.Fatalf("dev auth outside dev: %v", err)
	}
	_, err = Load(env(map[string]string{"MODE": "staging"}))
	if err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
		t.Fatalf("oidc is the default outside dev and needs config: %v", err)
	}
	_, err = Load(env(map[string]string{"MODE": "staging", "OIDC_ISSUER": "https://i", "OIDC_AUDIENCE": "a", "OIDC_MFA_AMR": "none"}))
	if err == nil || !strings.Contains(err.Error(), "MFA") {
		t.Fatalf("MFA must stay on outside dev: %v", err)
	}
}
