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
	if !c.FeaturePayForMe || c.PublicBaseURL != "https://meterly.ng" {
		t.Fatalf("unexpected M7 dev defaults: FeaturePayForMe=%v PublicBaseURL=%q", c.FeaturePayForMe, c.PublicBaseURL)
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

func TestNotificationCandidateCutoff(t *testing.T) {
	c, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil || !c.NotificationCandidatesSince.IsZero() {
		t.Fatalf("must default off: %+v %v", c, err)
	}
	for _, bad := range []string{"tomorrow", "2025-01-01T00:00:00", "0001-01-01T00:00:00Z"} {
		if _, err := Load(env(map[string]string{"MODE": "dev", "NOTIFICATION_CANDIDATES_SINCE": bad})); err == nil || !strings.Contains(err.Error(), "NOTIFICATION_CANDIDATES_SINCE") {
			t.Fatalf("bad cutoff %q: %v", bad, err)
		}
	}
	c, err = Load(env(map[string]string{"MODE": "dev", "NOTIFICATION_CANDIDATES_SINCE": "2025-03-01T03:30:00+01:00"}))
	if err != nil || !c.NotificationCandidatesSince.Equal(time.Date(2025, 3, 1, 2, 30, 0, 0, time.UTC)) || c.NotificationCandidatesSince.Location() != time.UTC {
		t.Fatalf("cutoff: %+v %v", c, err)
	}
}

func TestNotificationProvidersAndGiftSMSFlag(t *testing.T) {
	c, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.FeatureGiftSMS || c.SMSProvider != "mock" || c.EmailProvider != "mock" || c.WhatsAppProvider != "mock" || c.NotificationHMACRetention != 365*24*time.Hour {
		t.Fatalf("unexpected dev notification defaults: %+v", c)
	}

	if _, err := Load(env(map[string]string{"MODE": "dev", "FEATURE_GIFT_SMS": "yes"})); err == nil || !strings.Contains(err.Error(), "FEATURE_GIFT_SMS") {
		t.Fatalf("invalid FEATURE_GIFT_SMS must fail: %v", err)
	}
	if _, err := Load(env(map[string]string{"MODE": "dev", "SMS_PROVIDER": "africastalking"})); err == nil || !strings.Contains(err.Error(), "AFRICASTALKING_USERNAME") {
		t.Fatalf("missing AT credentials must fail: %v", err)
	}
	if _, err := Load(env(map[string]string{"MODE": "dev", "EMAIL_PROVIDER": "resend"})); err == nil || !strings.Contains(err.Error(), "RESEND_API_KEY") {
		t.Fatalf("missing Resend credentials must fail: %v", err)
	}
	if _, err := Load(env(map[string]string{"MODE": "dev", "WHATSAPP_PROVIDER": "whatsapp"})); err == nil || !strings.Contains(err.Error(), "WHATSAPP_PHONE_NUMBER_ID") {
		t.Fatalf("missing WhatsApp credentials must fail: %v", err)
	}
}

func TestObservabilityConfig(t *testing.T) {
	c, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetricsAddr != ":9090" || c.MetricsRefreshInterval != time.Minute || c.OTelEndpoint != "" {
		t.Fatalf("unexpected observability defaults: %+v", c)
	}

	offCfg, err := Load(env(map[string]string{"MODE": "dev", "METRICS_ADDR": "off", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"}))
	if err != nil || offCfg.MetricsAddr != "" || offCfg.OTelEndpoint != "http://127.0.0.1:4318" {
		t.Fatalf("off metrics / otel endpoint: err=%v cfg=%+v", err, offCfg)
	}

	if _, err := Load(env(map[string]string{"MODE": "dev", "PORT": "8080", "METRICS_ADDR": ":8080"})); err == nil || !strings.Contains(err.Error(), "METRICS_ADDR") {
		t.Fatalf("metrics sharing public HTTP port must fail: %v", err)
	}
}

func TestSplitMeterConfig(t *testing.T) {
	devCfg, err := Load(env(map[string]string{"MODE": "dev"}))
	if err != nil {
		t.Fatal(err)
	}
	if !devCfg.FeatureSplitMeter || devCfg.PotMaxHold != 72*time.Hour || devCfg.Orchestrator.PotMaxHold != 72*time.Hour {
		t.Fatalf("unexpected dev split-meter defaults: enabled=%v hold=%s", devCfg.FeatureSplitMeter, devCfg.PotMaxHold)
	}

	prodEnv := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"MODE":                 "production",
			"DATABASE_URL":         "postgres://user:pass@localhost:5432/meterly",
			"TOKEN_ENC_KEY":        strings.Repeat("ab", 32),
			"METER_HMAC_KEY":       strings.Repeat("cd", 32),
			"PAYMENT_PROVIDER":     "paystack",
			"PAYSTACK_SECRET_KEY":  "sk_live_12345",
			"PROVIDER_ENVIRONMENT": "PRODUCTION",
			"VENDING_PROVIDER":     "mock",
			"REMOTE_PROVIDER":      "none",
			"OIDC_ISSUER":          "https://auth.meterly.ng",
			"OIDC_AUDIENCE":        "meterly-api",
			"OIDC_JWKS_URL":        "https://auth.meterly.ng/.well-known/jwks.json",
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// Staging with real non-mock check: check FeatureSplitMeter default false outside dev even when config errors on mock vending.
	stgCfg, _ := Load(env(prodEnv(map[string]string{"MODE": "staging", "PAYSTACK_SECRET_KEY": "sk_test_123", "PROVIDER_ENVIRONMENT": "SANDBOX"})))
	if stgCfg.FeatureSplitMeter {
		t.Fatalf("FEATURE_SPLIT_METER must default to false outside dev")
	}

	if _, err := Load(env(map[string]string{"MODE": "dev", "POT_MAX_HOLD": "200h"})); err == nil || !strings.Contains(err.Error(), "POT_MAX_HOLD") {
		t.Fatalf("POT_MAX_HOLD above 168h must fail: %v", err)
	}
	if _, err := Load(env(map[string]string{"MODE": "dev", "FEATURE_SPLIT_METER": "maybe"})); err == nil || !strings.Contains(err.Error(), "FEATURE_SPLIT_METER") {
		t.Fatalf("invalid FEATURE_SPLIT_METER must fail: %v", err)
	}
}
