// Package config loads and validates process configuration from the
// environment. Invalid or incomplete configuration fails at startup.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/orchestrator"
)

type Mode string

const (
	Dev        Mode = "dev"
	Staging    Mode = "staging"
	Production Mode = "production"
)

type Config struct {
	Mode     Mode
	HTTPAddr string
	// DatabaseURL empty means the in-memory store (dev only).
	DatabaseURL string
	// RunWorker runs the resume and outbox loops inside cmd/api.
	RunWorker bool

	// Keys are hex in the environment; empty in dev means ephemeral.
	TokenEncKey  []byte
	MeterHMACKey []byte
	KeyVersion   string

	PaymentProvider     string // mock
	VendingProvider     string // mock
	RemoteProvider      string // mock | none
	ProviderEnvironment string // SANDBOX | PRODUCTION

	Orchestrator    orchestrator.Config
	ResumeInterval  time.Duration
	OutboxInterval  time.Duration
	ShutdownTimeout time.Duration
}

// UsesMocks reports whether any provider is an in-process mock.
func (c Config) UsesMocks() bool {
	return c.PaymentProvider == "mock" || c.VendingProvider == "mock" || c.RemoteProvider == "mock"
}

// Load reads configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	dur := func(k string, def time.Duration) time.Duration {
		v := getenv(k)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", k, v))
			return def
		}
		return d
	}
	integer := func(k string, def int64) int64 {
		v := getenv(k)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: invalid non-negative integer %q", k, v))
			return def
		}
		return n
	}
	key := func(k string) []byte {
		v := getenv(k)
		if v == "" {
			return nil
		}
		b, err := hex.DecodeString(v)
		if err != nil || len(b) != 32 {
			errs = append(errs, fmt.Errorf("%s: must be 64 hex characters (32 bytes)", k))
			return nil
		}
		return b
	}

	c := Config{
		Mode:                Mode(get("MODE", "")),
		HTTPAddr:            ":" + get("PORT", "8080"),
		DatabaseURL:         getenv("DATABASE_URL"),
		TokenEncKey:         key("TOKEN_ENC_KEY"),
		MeterHMACKey:        key("METER_HMAC_KEY"),
		KeyVersion:          get("KEY_VERSION", "v1"),
		PaymentProvider:     get("PAYMENT_PROVIDER", "mock"),
		VendingProvider:     get("VENDING_PROVIDER", "mock"),
		RemoteProvider:      get("REMOTE_PROVIDER", "mock"),
		ProviderEnvironment: get("PROVIDER_ENVIRONMENT", "SANDBOX"),
		ResumeInterval:      dur("RESUME_INTERVAL", 15*time.Second),
		OutboxInterval:      dur("OUTBOX_INTERVAL", 2*time.Second),
		ShutdownTimeout:     dur("SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	o := orchestrator.DefaultConfig()
	o.FeeMinor = integer("FEE_MINOR", o.FeeMinor)
	o.MinAmountMinor = integer("MIN_AMOUNT_MINOR", o.MinAmountMinor)
	o.MaxAmountMinor = integer("MAX_AMOUNT_MINOR", o.MaxAmountMinor)
	o.QuoteTTL = dur("QUOTE_TTL", o.QuoteTTL)
	o.ProviderTimeout = dur("PROVIDER_TIMEOUT", o.ProviderTimeout)
	o.PaymentExpiry = dur("PAYMENT_EXPIRY", o.PaymentExpiry)
	o.ResumeStaleAfter = dur("RESUME_STALE_AFTER", o.ResumeStaleAfter)
	o.ResumeLease = dur("RESUME_LEASE", o.ResumeLease)
	o.ResumeBaseBackoff = dur("RESUME_BASE_BACKOFF", o.ResumeBaseBackoff)
	o.ResumeMaxBackoff = dur("RESUME_MAX_BACKOFF", o.ResumeMaxBackoff)
	o.ResumeMaxAttempts = int(integer("RESUME_MAX_ATTEMPTS", int64(o.ResumeMaxAttempts)))
	c.Orchestrator = o

	switch c.Mode {
	case Dev, Staging, Production:
	case "":
		errs = append(errs, errors.New("MODE is required (dev, staging or production)"))
	default:
		errs = append(errs, fmt.Errorf("MODE: unknown value %q", c.Mode))
	}
	if o.MinAmountMinor <= 0 || o.MinAmountMinor > o.MaxAmountMinor {
		errs = append(errs, errors.New("MIN_AMOUNT_MINOR must be positive and not above MAX_AMOUNT_MINOR"))
	}
	if o.ResumeMaxAttempts < 1 {
		errs = append(errs, errors.New("RESUME_MAX_ATTEMPTS must be at least 1"))
	}
	if o.ResumeStaleAfter <= o.ProviderTimeout {
		errs = append(errs, errors.New("RESUME_STALE_AFTER must exceed PROVIDER_TIMEOUT, or the worker could race an in-flight call"))
	}
	for k, v := range map[string]string{"PAYMENT_PROVIDER": c.PaymentProvider, "VENDING_PROVIDER": c.VendingProvider} {
		if v != "mock" {
			errs = append(errs, fmt.Errorf("%s: unsupported value %q (available: mock)", k, v))
		}
	}
	if c.RemoteProvider != "mock" && c.RemoteProvider != "none" {
		errs = append(errs, fmt.Errorf("REMOTE_PROVIDER: unsupported value %q (available: mock, none)", c.RemoteProvider))
	}
	if c.ProviderEnvironment != "SANDBOX" && c.ProviderEnvironment != "PRODUCTION" {
		errs = append(errs, fmt.Errorf("PROVIDER_ENVIRONMENT: must be SANDBOX or PRODUCTION"))
	}

	runWorker := strings.ToLower(get("RUN_WORKER", ""))
	switch runWorker {
	case "":
		c.RunWorker = c.Mode == Dev
	case "true", "1":
		c.RunWorker = true
	case "false", "0":
	default:
		errs = append(errs, fmt.Errorf("RUN_WORKER: must be true or false"))
	}

	if c.Mode != Dev {
		if c.DatabaseURL == "" {
			errs = append(errs, errors.New("DATABASE_URL is required outside dev"))
		}
		if c.TokenEncKey == nil || c.MeterHMACKey == nil {
			errs = append(errs, errors.New("TOKEN_ENC_KEY and METER_HMAC_KEY are required outside dev"))
		}
		if c.UsesMocks() {
			errs = append(errs, errors.New("mock providers are only allowed in MODE=dev; no real provider adapters are configured yet"))
		}
	}
	if c.DatabaseURL == "" && !c.RunWorker {
		errs = append(errs, errors.New("the in-memory store needs RUN_WORKER=true (there is no shared state for a separate worker)"))
	}
	return c, errors.Join(errs...)
}
