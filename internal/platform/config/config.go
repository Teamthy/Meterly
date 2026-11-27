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

	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/pots"
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

	PaymentProvider string // mock | paystack
	VendingProvider string // mock | vtpass

	PaystackSecretKey    string
	PaystackBaseURL      string
	PaystackCallbackURL  string
	VTPassAPIKey         string
	VTPassSecretKey      string
	VTPassPublicKey      string
	VTPassBaseURL        string
	VTPassDefaultPhone   string
	VTPassWalletMinMinor int64
	RemoteProvider       string // mock | none | http
	RemoteHESBaseURL     string
	RemoteHESHMACSecret  string
	MeterReadingProvider string // mock | none
	ProviderEnvironment  string // SANDBOX | PRODUCTION

	// Notification providers and gates (M5).
	SMSProvider                 string // mock | africastalking | none
	EmailProvider               string // mock | resend | none
	WhatsAppProvider            string // mock | whatsapp | none
	AfricasTalkingUsername      string
	AfricasTalkingAPIKey        string
	AfricasTalkingSenderID      string
	AfricasTalkingWebhookSecret string
	AfricasTalkingBaseURL       string
	ResendAPIKey                string
	ResendFrom                  string
	ResendWebhookSecret         string
	ResendBaseURL               string
	WhatsAppPhoneNumberID       string
	WhatsAppAccessToken         string
	WhatsAppAppSecret           string
	WhatsAppVerifyToken         string
	WhatsAppAPIVersion          string
	WhatsAppBaseURL             string
	FeatureGiftSMS              bool
	NotificationInterval        time.Duration
	NotificationHMACRetention   time.Duration
	RetentionOTP                time.Duration
	RetentionChallenge          time.Duration
	RetentionSession            time.Duration
	RetentionIdempotency        time.Duration
	RetentionRiskObservation    time.Duration
	RetentionSupport            time.Duration
	RetentionFinancial          time.Duration
	RetentionBatchSize          int
	RetentionDryRun             bool
	RetentionInterval           time.Duration

	// Authentication: "dev" (bearer token = subject) or "oidc".
	AuthMode        string
	OIDCIssuer      string
	OIDCAudience    string
	OIDCJWKSURL     string
	OIDCRolesClaim  string
	OIDCMFAValues   []string // staff roles require one of these in "amr"
	WebAuthnOrigins []string // browser origins allowed to register/login passkeys

	// Observability (M6).
	MetricsAddr            string        // internal Prometheus bind address (default ":9090", "off" disables)
	MetricsRefreshInterval time.Duration // gauge refresh period (default 1m)
	OTelEndpoint           string        // OTEL_EXPORTER_OTLP_ENDPOINT; empty = no-op tracer

	// Pay-for-Me shareable payment request links (M7).
	FeaturePayForMe      bool
	PublicBaseURL        string
	RatePublicPayPerMin  int
	RatePublicLinkPerMin int

	// Split the Meter Funding Pots (M8).
	FeatureSplitMeter bool
	PotMaxHold        time.Duration

	// AutoPower scheduled auto-recharge with spend guard (M9).
	FeatureAutoPower  bool
	AutoPowerInterval time.Duration

	// Landlord Power Desk organisations, unit meters & bulk top-ups (M10).
	FeaturePowerDesk bool

	// Meterly Offline USSD & WhatsApp purchase channels (M11).
	FeatureUSSD     bool
	FeatureWhatsApp bool
	// Support chat (agent + E2E human operators).
	FeatureSupport bool
	// Households: verified ownership + membership (FR-008).
	FeatureHouseholds bool
	// FeatureRiskEngine gates the P3 abuse-control engine (DENY/REVIEW).
	// Defaults on in MODE=dev and MODE=staging (pilot); off in production.
	FeatureRiskEngine bool

	// PilotMode restricts purchases to the audited allow-list (users, meters,
	// optional DisCos) plus daily caps. SERVICE_PAUSED is a global kill switch.
	PilotMode                bool
	ServicePaused            bool
	PilotDailyUserCapMinor   int64
	PilotDailyGlobalCapMinor int64
	// FeatureOwnershipNameMatch enables self-service meter ownership by
	// display-name match. Spoofable (display names are user-editable and
	// meter validation reveals the DisCo name): dev only by default.
	FeatureOwnershipNameMatch      bool
	USSDSharedSecret               string
	USSDAllowedIPs                 []string
	OfflineReverifyAfter           time.Duration
	OfflineUnverifiedDailyMaxMinor int64
	OfflineVerifiedDailyMaxMinor   int64

	// Rate limits (per minute) and client IP source.
	RateWritesPerMin       int
	RateAuthFailuresPerMin int
	RateWebhooksPerMin     int
	ClientIPHeader         string
	// RedisURL, when set (redis:// or rediss://), shares rate-limit buckets
	// across API replicas. Empty keeps in-process token buckets.
	RedisURL string

	Orchestrator      orchestrator.Config
	ResumeInterval    time.Duration
	OutboxInterval    time.Duration
	ReconcileInterval time.Duration
	// A nonzero cutoff opts in to held notification candidate collection.
	NotificationCandidatesSince time.Time
	ShutdownTimeout             time.Duration
}

// UsesMocks reports whether any provider is an in-process mock.
func (c Config) UsesMocks() bool {
	return c.PaymentProvider == "mock" ||
		c.VendingProvider == "mock" ||
		c.RemoteProvider == "mock" ||
		c.MeterReadingProvider == "mock" ||
		c.SMSProvider == "mock" ||
		c.EmailProvider == "mock" ||
		c.WhatsAppProvider == "mock"
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
		Mode:                   Mode(get("MODE", "")),
		HTTPAddr:               ":" + get("PORT", "8080"),
		MetricsAddr:            get("METRICS_ADDR", ":9090"),
		MetricsRefreshInterval: dur("METRICS_REFRESH_INTERVAL", time.Minute),
		OTelEndpoint:           strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		DatabaseURL:            getenv("DATABASE_URL"),
		TokenEncKey:            key("TOKEN_ENC_KEY"),
		MeterHMACKey:           key("METER_HMAC_KEY"),
		KeyVersion:             get("KEY_VERSION", "v1"),
		PaymentProvider:        get("PAYMENT_PROVIDER", "mock"),
		VendingProvider:        get("VENDING_PROVIDER", "mock"),
		RemoteProvider:         get("REMOTE_PROVIDER", map[bool]string{true: "mock", false: "none"}[Mode(get("MODE", "")) == Dev]),
		RemoteHESBaseURL:       strings.TrimSpace(getenv("REMOTE_HES_BASE_URL")),
		RemoteHESHMACSecret:    strings.TrimSpace(getenv("REMOTE_HES_HMAC_SECRET")),
		ProviderEnvironment:    get("PROVIDER_ENVIRONMENT", "SANDBOX"),
		PaystackSecretKey:      getenv("PAYSTACK_SECRET_KEY"),
		PaystackBaseURL:        get("PAYSTACK_BASE_URL", "https://api.paystack.co"),
		PaystackCallbackURL:    getenv("PAYSTACK_CALLBACK_URL"),
		VTPassAPIKey:           strings.TrimSpace(getenv("VTPASS_API_KEY")),
		VTPassSecretKey:        strings.TrimSpace(getenv("VTPASS_SECRET_KEY")),
		VTPassPublicKey:        strings.TrimSpace(getenv("VTPASS_PUBLIC_KEY")),
		VTPassBaseURL:          strings.TrimSpace(getenv("VTPASS_BASE_URL")),
		VTPassDefaultPhone:     strings.TrimSpace(getenv("VTPASS_DEFAULT_PHONE")),
		ResumeInterval:         dur("RESUME_INTERVAL", 15*time.Second),
		OutboxInterval:         dur("OUTBOX_INTERVAL", 2*time.Second),
		ReconcileInterval:      dur("RECONCILE_INTERVAL", time.Hour),
		ShutdownTimeout:        dur("SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	notifDefault := "none"
	if c.Mode == Dev {
		notifDefault = "mock"
	}
	c.MeterReadingProvider = get("METER_READING_PROVIDER", notifDefault)
	c.SMSProvider = get("SMS_PROVIDER", notifDefault)
	c.EmailProvider = get("EMAIL_PROVIDER", notifDefault)
	c.WhatsAppProvider = get("WHATSAPP_PROVIDER", notifDefault)
	c.AfricasTalkingUsername = strings.TrimSpace(getenv("AFRICASTALKING_USERNAME"))
	c.AfricasTalkingAPIKey = strings.TrimSpace(getenv("AFRICASTALKING_API_KEY"))
	c.AfricasTalkingSenderID = strings.TrimSpace(getenv("AFRICASTALKING_SENDER_ID"))
	c.AfricasTalkingWebhookSecret = strings.TrimSpace(getenv("AFRICASTALKING_WEBHOOK_SECRET"))
	c.AfricasTalkingBaseURL = strings.TrimSpace(getenv("AFRICASTALKING_BASE_URL"))
	c.ResendAPIKey = strings.TrimSpace(getenv("RESEND_API_KEY"))
	c.ResendFrom = strings.TrimSpace(getenv("RESEND_FROM"))
	c.ResendWebhookSecret = strings.TrimSpace(getenv("RESEND_WEBHOOK_SECRET"))
	c.ResendBaseURL = strings.TrimSpace(getenv("RESEND_BASE_URL"))
	c.WhatsAppPhoneNumberID = strings.TrimSpace(getenv("WHATSAPP_PHONE_NUMBER_ID"))
	c.WhatsAppAccessToken = strings.TrimSpace(getenv("WHATSAPP_ACCESS_TOKEN"))
	c.WhatsAppAppSecret = strings.TrimSpace(getenv("WHATSAPP_APP_SECRET"))
	c.WhatsAppVerifyToken = strings.TrimSpace(getenv("WHATSAPP_VERIFY_TOKEN"))
	c.WhatsAppAPIVersion = get("WHATSAPP_API_VERSION", "v21.0")
	c.WhatsAppBaseURL = strings.TrimSpace(getenv("WHATSAPP_BASE_URL"))
	c.NotificationInterval = dur("NOTIFICATION_INTERVAL", 2*time.Second)
	c.NotificationHMACRetention = dur("NOTIFICATION_HMAC_RETENTION", 365*24*time.Hour)
	c.RetentionOTP = dur("RETENTION_OTP", 7*24*time.Hour)
	c.RetentionChallenge = dur("RETENTION_CHALLENGE", 7*24*time.Hour)
	c.RetentionSession = dur("RETENTION_SESSION", 90*24*time.Hour)
	c.RetentionIdempotency = dur("RETENTION_IDEMPOTENCY", 30*24*time.Hour)
	c.RetentionRiskObservation = dur("RETENTION_RISK_OBSERVATION", 90*24*time.Hour)
	c.RetentionSupport = dur("RETENTION_SUPPORT", 730*24*time.Hour)
	c.RetentionFinancial = dur("RETENTION_FINANCIAL", 7*365*24*time.Hour)
	c.RetentionBatchSize = int(integer("RETENTION_BATCH_SIZE", 500))
	c.RetentionInterval = dur("RETENTION_INTERVAL", time.Hour)
	switch strings.ToLower(strings.TrimSpace(getenv("RETENTION_DRY_RUN"))) {
	case "", "false", "0":
		c.RetentionDryRun = false
	case "true", "1":
		c.RetentionDryRun = true
	default:
		errs = append(errs, fmt.Errorf("RETENTION_DRY_RUN: must be true or false"))
	}

	// FEATURE_GIFT_SMS defaults to false everywhere until legal sign-off.
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_GIFT_SMS"))) {
	case "", "false", "0":
		c.FeatureGiftSMS = false
	case "true", "1":
		c.FeatureGiftSMS = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_GIFT_SMS: must be true or false"))
	}

	// FEATURE_PAY_FOR_ME defaults to true in MODE=dev and false outside dev.
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_PAY_FOR_ME"))) {
	case "":
		c.FeaturePayForMe = c.Mode == Dev
	case "false", "0":
		c.FeaturePayForMe = false
	case "true", "1":
		c.FeaturePayForMe = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_PAY_FOR_ME: must be true or false"))
	}

	// FEATURE_SPLIT_METER defaults to true in MODE=dev and false outside dev
	// until Nigerian regulatory / PSP fund-holding sign-off (docs/DECISIONS.md).
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_SPLIT_METER"))) {
	case "":
		c.FeatureSplitMeter = c.Mode == Dev
	case "false", "0":
		c.FeatureSplitMeter = false
	case "true", "1":
		c.FeatureSplitMeter = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_SPLIT_METER: must be true or false"))
	}
	c.PotMaxHold = dur("POT_MAX_HOLD", pots.DefaultMaxHold)
	if c.PotMaxHold > pots.HardMaxHold {
		errs = append(errs, fmt.Errorf("POT_MAX_HOLD: cannot exceed %s", pots.HardMaxHold))
	}

	// FEATURE_AUTOPOWER defaults to false everywhere until explicitly enabled.
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_AUTOPOWER"))) {
	case "", "false", "0":
		c.FeatureAutoPower = false
	case "true", "1":
		c.FeatureAutoPower = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_AUTOPOWER: must be true or false"))
	}
	c.AutoPowerInterval = dur("AUTOPOWER_INTERVAL", 30*time.Second)

	// FEATURE_POWER_DESK defaults to false everywhere until explicitly enabled.
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_POWER_DESK"))) {
	case "", "false", "0":
		c.FeaturePowerDesk = false
	case "true", "1":
		c.FeaturePowerDesk = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_POWER_DESK: must be true or false"))
	}

	// FEATURE_USSD and FEATURE_WHATSAPP default to false everywhere until explicitly enabled.
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_USSD"))) {
	case "", "false", "0":
		c.FeatureUSSD = false
	case "true", "1":
		c.FeatureUSSD = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_USSD: must be true or false"))
	}
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_WHATSAPP"))) {
	case "", "false", "0":
		c.FeatureWhatsApp = false
	case "true", "1":
		c.FeatureWhatsApp = true
	default:
		errs = append(errs, fmt.Errorf("FEATURE_WHATSAPP: must be true or false"))
	}
	defUSSDSecret := ""
	defUSSDIPs := ""
	if c.Mode == Dev {
		defUSSDSecret = "dev-ussd-secret"
		defUSSDIPs = "127.0.0.1/32,::1/128"
	}
	c.USSDSharedSecret = get("USSD_SHARED_SECRET", defUSSDSecret)
	if rawIPs := get("USSD_ALLOWED_IPS", defUSSDIPs); rawIPs != "" {
		for _, item := range strings.Split(rawIPs, ",") {
			if s := strings.TrimSpace(item); s != "" {
				c.USSDAllowedIPs = append(c.USSDAllowedIPs, s)
			}
		}
		if _, err := ussd.ParseAllowlist(c.USSDAllowedIPs); err != nil {
			errs = append(errs, fmt.Errorf("USSD_ALLOWED_IPS: %w", err))
		}
	}
	if c.FeatureUSSD && c.Mode != Dev {
		if c.USSDSharedSecret == "" {
			errs = append(errs, errors.New("USSD_SHARED_SECRET is required when FEATURE_USSD=true outside dev"))
		}
		if len(c.USSDAllowedIPs) == 0 {
			errs = append(errs, errors.New("USSD_ALLOWED_IPS is required when FEATURE_USSD=true outside dev"))
		}
	}
	if c.FeatureWhatsApp && c.Mode != Dev {
		if strings.TrimSpace(c.WhatsAppAppSecret) == "" || strings.TrimSpace(c.WhatsAppVerifyToken) == "" {
			errs = append(errs, errors.New("WHATSAPP_APP_SECRET and WHATSAPP_VERIFY_TOKEN are required when FEATURE_WHATSAPP=true outside dev"))
		}
	}
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_SUPPORT"))) {
	case "", "true", "1":
		c.FeatureSupport = true
	case "false", "0":
		c.FeatureSupport = false
	default:
		errs = append(errs, fmt.Errorf("FEATURE_SUPPORT: must be true or false"))
	}
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_OWNERSHIP_NAME_MATCH"))) {
	case "":
		c.FeatureOwnershipNameMatch = c.Mode == Dev
	case "true", "1":
		c.FeatureOwnershipNameMatch = true
	case "false", "0":
		c.FeatureOwnershipNameMatch = false
	default:
		errs = append(errs, fmt.Errorf("FEATURE_OWNERSHIP_NAME_MATCH: must be true or false"))
	}
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_HOUSEHOLDS"))) {
	case "":
		c.FeatureHouseholds = c.Mode == Dev
	case "true", "1":
		c.FeatureHouseholds = true
	case "false", "0":
		c.FeatureHouseholds = false
	default:
		errs = append(errs, fmt.Errorf("FEATURE_HOUSEHOLDS: must be true or false"))
	}
	switch strings.ToLower(strings.TrimSpace(getenv("FEATURE_RISK_ENGINE"))) {
	case "":
		c.FeatureRiskEngine = c.Mode == Dev || c.Mode == Staging
	case "true", "1":
		c.FeatureRiskEngine = true
	case "false", "0":
		c.FeatureRiskEngine = false
	default:
		errs = append(errs, fmt.Errorf("FEATURE_RISK_ENGINE: must be true or false"))
	}

	switch strings.ToLower(strings.TrimSpace(getenv("PILOT_MODE"))) {
	case "", "false", "0":
	case "true", "1":
		c.PilotMode = true
	default:
		errs = append(errs, fmt.Errorf("PILOT_MODE: must be true or false"))
	}
	switch strings.ToLower(strings.TrimSpace(getenv("SERVICE_PAUSED"))) {
	case "", "false", "0":
	case "true", "1":
		c.ServicePaused = true
	default:
		errs = append(errs, fmt.Errorf("SERVICE_PAUSED: must be true or false"))
	}
	c.PilotDailyUserCapMinor = integer("PILOT_DAILY_USER_CAP_MINOR", 0)
	c.PilotDailyGlobalCapMinor = integer("PILOT_DAILY_GLOBAL_CAP_MINOR", 0)

	c.OfflineReverifyAfter = dur("OFFLINE_REVERIFY_AFTER", conversation.DefaultReverifyAfter)
	c.OfflineUnverifiedDailyMaxMinor = integer("OFFLINE_UNVERIFIED_DAILY_MAX_MINOR", conversation.DefaultUnverifiedDailyCapMinor)
	c.OfflineVerifiedDailyMaxMinor = integer("OFFLINE_VERIFIED_DAILY_MAX_MINOR", conversation.DefaultVerifiedDailyCapMinor)
	if c.OfflineUnverifiedDailyMaxMinor <= 0 || c.OfflineVerifiedDailyMaxMinor < c.OfflineUnverifiedDailyMaxMinor {
		errs = append(errs, errors.New("OFFLINE_UNVERIFIED_DAILY_MAX_MINOR must be positive and not exceed OFFLINE_VERIFIED_DAILY_MAX_MINOR"))
	}
	c.PublicBaseURL = strings.TrimRight(get("PUBLIC_BASE_URL", "https://meterly.ng"), "/")
	if c.Mode != Dev && !strings.HasPrefix(c.PublicBaseURL, "https://") {
		errs = append(errs, errors.New("PUBLIC_BASE_URL must be https outside dev"))
	}

	// No default: opting in requires an explicit fixed event-time boundary.
	if v := strings.TrimSpace(getenv("NOTIFICATION_CANDIDATES_SINCE")); v != "" {
		since, err := time.Parse(time.RFC3339Nano, v)
		if err != nil || since.IsZero() {
			errs = append(errs, errors.New("NOTIFICATION_CANDIDATES_SINCE must be a nonzero RFC3339 timestamp with timezone"))
		} else {
			c.NotificationCandidatesSince = since.UTC()
		}
	}

	c.AuthMode = get("AUTH_MODE", map[bool]string{true: "dev", false: "oidc"}[c.Mode == Dev])
	c.OIDCIssuer = getenv("OIDC_ISSUER")
	c.OIDCAudience = getenv("OIDC_AUDIENCE")
	c.OIDCJWKSURL = getenv("OIDC_JWKS_URL")
	c.OIDCRolesClaim = get("OIDC_ROLES_CLAIM", "roles")
	for _, p := range strings.Split(getenv("WEBAUTHN_ORIGINS"), ",") {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "https://") || (c.Mode == Dev && strings.HasPrefix(p, "http://")) {
			c.WebAuthnOrigins = append(c.WebAuthnOrigins, p)
			continue
		}
		errs = append(errs, errors.New("WEBAUTHN_ORIGINS entries must be https origins (http allowed in MODE=dev)"))
	}
	if v := get("OIDC_MFA_AMR", "mfa,otp,hwk,swk"); v != "none" {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				c.OIDCMFAValues = append(c.OIDCMFAValues, x)
			}
		}
	}
	c.RateWritesPerMin = int(integer("RATE_LIMIT_WRITES_PER_MIN", 30))
	c.RateAuthFailuresPerMin = int(integer("RATE_LIMIT_AUTH_FAILURES_PER_MIN", 20))
	c.VTPassWalletMinMinor = integer("VTPASS_WALLET_MIN_MINOR", 0)
	if c.VendingProvider == "vtpass" && strings.TrimSpace(getenv("VTPASS_WALLET_MIN_MINOR")) == "" {
		c.VTPassWalletMinMinor = 5_000_000 // ₦50,000
	}
	c.RateWebhooksPerMin = int(integer("RATE_LIMIT_WEBHOOKS_PER_MIN", 600))
	c.RatePublicPayPerMin = int(integer("RATE_LIMIT_PUBLIC_PAY_PER_MIN", 60))
	c.RatePublicLinkPerMin = int(integer("RATE_LIMIT_PUBLIC_LINK_PER_MIN", 10))
	c.ClientIPHeader = getenv("CLIENT_IP_HEADER")
	c.RedisURL = strings.TrimSpace(getenv("REDIS_URL"))

	o := orchestrator.DefaultConfig()
	o.FeeMinor = integer("FEE_MINOR", o.FeeMinor)
	o.MinAmountMinor = integer("MIN_AMOUNT_MINOR", o.MinAmountMinor)
	o.MaxAmountMinor = integer("MAX_AMOUNT_MINOR", o.MaxAmountMinor)
	o.QuoteTTL = dur("QUOTE_TTL", o.QuoteTTL)
	o.ProviderTimeout = dur("PROVIDER_TIMEOUT", o.ProviderTimeout)
	o.PaymentExpiry = dur("PAYMENT_EXPIRY", o.PaymentExpiry)
	o.PotMaxHold = c.PotMaxHold
	o.AutoPowerDailyMaxMinor = integer("AUTOPOWER_DAILY_MAX_MINOR", o.AutoPowerDailyMaxMinor)
	o.AutoPowerPollInterval = dur("AUTOPOWER_POLL_INTERVAL", o.AutoPowerPollInterval)
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
	switch c.PaymentProvider {
	case "mock":
	case "paystack":
		// Live keys only with live mode, so a sandbox can never move real money.
		wantPrefix, env := "sk_test_", "SANDBOX"
		if c.Mode == Production {
			wantPrefix, env = "sk_live_", "PRODUCTION"
		}
		if !strings.HasPrefix(c.PaystackSecretKey, wantPrefix) {
			errs = append(errs, fmt.Errorf("PAYSTACK_SECRET_KEY: must be set and start with %s in MODE=%s", wantPrefix, c.Mode))
		}
		if c.ProviderEnvironment != env {
			errs = append(errs, fmt.Errorf("PROVIDER_ENVIRONMENT must be %s for MODE=%s with Paystack", env, c.Mode))
		}
		if !strings.HasPrefix(c.PaystackBaseURL, "https://") && c.Mode != Dev {
			errs = append(errs, errors.New("PAYSTACK_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("PAYMENT_PROVIDER: unsupported value %q (available: mock, paystack)", c.PaymentProvider))
	}
	switch c.VendingProvider {
	case "mock":
	case "vtpass":
		if c.VTPassAPIKey == "" || c.VTPassSecretKey == "" {
			errs = append(errs, errors.New("VTPASS_API_KEY and VTPASS_SECRET_KEY are required when VENDING_PROVIDER=vtpass"))
		}
		if digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, c.VTPassDefaultPhone); len(digits) < 10 {
			errs = append(errs, errors.New("VTPASS_DEFAULT_PHONE is required when VENDING_PROVIDER=vtpass (VTpass pay needs a customer MSISDN)"))
		}
		if c.VTPassBaseURL == "" {
			if c.ProviderEnvironment == "PRODUCTION" {
				c.VTPassBaseURL = "https://vtpass.com/api"
			} else {
				c.VTPassBaseURL = "https://sandbox.vtpass.com/api"
			}
		}
		if c.Mode != Dev && !strings.HasPrefix(c.VTPassBaseURL, "https://") {
			errs = append(errs, errors.New("VTPASS_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("VENDING_PROVIDER: unsupported value %q (available: mock, vtpass)", c.VendingProvider))
	}
	switch c.RemoteProvider {
	case "mock", "none":
	case "http":
		if len(c.RemoteHESHMACSecret) < 16 {
			errs = append(errs, errors.New("REMOTE_HES_HMAC_SECRET is required (min 16 characters) when REMOTE_PROVIDER=http"))
		}
		if c.RemoteHESBaseURL == "" {
			errs = append(errs, errors.New("REMOTE_HES_BASE_URL is required when REMOTE_PROVIDER=http"))
		}
		if c.Mode != Dev && c.RemoteHESBaseURL != "" && !strings.HasPrefix(c.RemoteHESBaseURL, "https://") {
			errs = append(errs, errors.New("REMOTE_HES_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("REMOTE_PROVIDER: unsupported value %q (available: mock, none, http)", c.RemoteProvider))
	}
	if c.MeterReadingProvider != "mock" && c.MeterReadingProvider != "none" {
		errs = append(errs, fmt.Errorf("METER_READING_PROVIDER: unsupported value %q (available: mock, none)", c.MeterReadingProvider))
	}
	if o.AutoPowerDailyMaxMinor <= 0 {
		errs = append(errs, errors.New("AUTOPOWER_DAILY_MAX_MINOR must be positive"))
	}
	if c.ProviderEnvironment != "SANDBOX" && c.ProviderEnvironment != "PRODUCTION" {
		errs = append(errs, fmt.Errorf("PROVIDER_ENVIRONMENT: must be SANDBOX or PRODUCTION"))
	}

	switch c.SMSProvider {
	case "mock", "none":
	case "africastalking":
		if c.AfricasTalkingUsername == "" || c.AfricasTalkingAPIKey == "" {
			errs = append(errs, errors.New("AFRICASTALKING_USERNAME and AFRICASTALKING_API_KEY are required when SMS_PROVIDER=africastalking"))
		}
		if c.Mode == Production && c.AfricasTalkingUsername == "sandbox" {
			errs = append(errs, errors.New("AFRICASTALKING_USERNAME cannot be sandbox in MODE=production"))
		}
		if c.Mode != Dev && c.AfricasTalkingBaseURL != "" && !strings.HasPrefix(c.AfricasTalkingBaseURL, "https://") {
			errs = append(errs, errors.New("AFRICASTALKING_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("SMS_PROVIDER: unsupported value %q (available: mock, africastalking, none)", c.SMSProvider))
	}

	switch c.EmailProvider {
	case "mock", "none":
	case "resend":
		if c.ResendAPIKey == "" || c.ResendFrom == "" {
			errs = append(errs, errors.New("RESEND_API_KEY and RESEND_FROM are required when EMAIL_PROVIDER=resend"))
		}
		if c.Mode != Dev && c.ResendBaseURL != "" && !strings.HasPrefix(c.ResendBaseURL, "https://") {
			errs = append(errs, errors.New("RESEND_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("EMAIL_PROVIDER: unsupported value %q (available: mock, resend, none)", c.EmailProvider))
	}

	switch c.WhatsAppProvider {
	case "mock", "none":
	case "whatsapp":
		if c.WhatsAppPhoneNumberID == "" || c.WhatsAppAccessToken == "" {
			errs = append(errs, errors.New("WHATSAPP_PHONE_NUMBER_ID and WHATSAPP_ACCESS_TOKEN are required when WHATSAPP_PROVIDER=whatsapp"))
		}
		if c.Mode != Dev && c.WhatsAppAppSecret == "" {
			errs = append(errs, errors.New("WHATSAPP_APP_SECRET is required outside dev when WHATSAPP_PROVIDER=whatsapp"))
		}
		if c.Mode != Dev && c.WhatsAppBaseURL != "" && !strings.HasPrefix(c.WhatsAppBaseURL, "https://") {
			errs = append(errs, errors.New("WHATSAPP_BASE_URL must be https outside dev"))
		}
	default:
		errs = append(errs, fmt.Errorf("WHATSAPP_PROVIDER: unsupported value %q (available: mock, whatsapp, none)", c.WhatsAppProvider))
	}

	switch c.AuthMode {
	case "dev":
		if c.Mode != Dev {
			errs = append(errs, errors.New("AUTH_MODE=dev is only allowed in MODE=dev"))
		}
	case "oidc":
		if c.OIDCIssuer == "" || c.OIDCAudience == "" {
			errs = append(errs, errors.New("OIDC_ISSUER and OIDC_AUDIENCE are required with AUTH_MODE=oidc"))
		}
		if c.Mode != Dev && len(c.OIDCMFAValues) == 0 {
			errs = append(errs, errors.New("OIDC_MFA_AMR cannot be disabled outside dev: staff roles must require MFA"))
		}
	default:
		errs = append(errs, fmt.Errorf("AUTH_MODE: must be dev or oidc"))
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
			errs = append(errs, errors.New("mock providers are only allowed in MODE=dev"))
		}
	}
	switch strings.ToLower(c.MetricsAddr) {
	case "off", "none", "disabled":
		c.MetricsAddr = ""
	default:
		if c.MetricsAddr == c.HTTPAddr {
			errs = append(errs, errors.New("METRICS_ADDR must not equal the public HTTP listen address"))
		}
	}

	if c.DatabaseURL == "" && !c.RunWorker {
		errs = append(errs, errors.New("the in-memory store needs RUN_WORKER=true (there is no shared state for a separate worker)"))
	}
	return c, errors.Join(errs...)
}
