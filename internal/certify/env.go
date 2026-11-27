package certify

import (
	"net/http"
	"os"
	"strings"
	"time"
)

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }

func truthy(k string) bool {
	switch strings.ToLower(getenv(k)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// FromEnv reads certification settings from the process environment.
// Keys are never logged. Live (non-sandbox) credentials are refused unless
// CERTIFY_ALLOW_LIVE=true.
func FromEnv() Config {
	hc := &http.Client{Timeout: 30 * time.Second}
	allowLive := truthy("CERTIFY_ALLOW_LIVE")
	cfg := Config{AllowLive: allowLive}

	cfg.Paystack = PaystackConfig{
		SecretKey:           getenv("PAYSTACK_SECRET_KEY"),
		BaseURL:             getenv("PAYSTACK_BASE_URL"),
		Email:               getenv("CERTIFY_PAYSTACK_EMAIL"),
		HTTPClient:          hc,
		AllowLive:           allowLive,
		CapturedWebhookBody: []byte(os.Getenv("CERTIFY_PAYSTACK_WEBHOOK_BODY")),
		CapturedWebhookSig:  getenv("CERTIFY_PAYSTACK_WEBHOOK_SIGNATURE"),
	}
	cfg.VTPass = VTPassConfig{
		APIKey:     getenv("VTPASS_API_KEY"),
		SecretKey:  getenv("VTPASS_SECRET_KEY"),
		Phone:      getenv("VTPASS_DEFAULT_PHONE"),
		BaseURL:    getenv("VTPASS_BASE_URL"),
		HTTPClient: hc,
		AllowLive:  allowLive,
		SkipPay:    truthy("CERTIFY_VTPASS_SKIP_PAY"),
	}
	cfg.AfricasTalking = ATConfig{
		Username:      getenv("AFRICASTALKING_USERNAME"),
		APIKey:        getenv("AFRICASTALKING_API_KEY"),
		SenderID:      getenv("AFRICASTALKING_SENDER_ID"),
		WebhookSecret: getenv("AFRICASTALKING_WEBHOOK_SECRET"),
		BaseURL:       getenv("AFRICASTALKING_BASE_URL"),
		To:            getenv("CERTIFY_AT_TO"),
		HTTPClient:    hc,
		AllowLive:     allowLive,
	}
	cfg.Resend = ResendConfig{
		APIKey:        getenv("RESEND_API_KEY"),
		From:          getenv("RESEND_FROM"),
		WebhookSecret: getenv("RESEND_WEBHOOK_SECRET"),
		BaseURL:       getenv("RESEND_BASE_URL"),
		To:            getenv("CERTIFY_RESEND_TO"),
		HTTPClient:    hc,
	}
	cfg.WhatsApp = WhatsAppConfig{
		PhoneNumberID: getenv("WHATSAPP_PHONE_NUMBER_ID"),
		AccessToken:   getenv("WHATSAPP_ACCESS_TOKEN"),
		AppSecret:     getenv("WHATSAPP_APP_SECRET"),
		VerifyToken:   getenv("WHATSAPP_VERIFY_TOKEN"),
		APIVersion:    getenv("WHATSAPP_API_VERSION"),
		BaseURL:       getenv("WHATSAPP_BASE_URL"),
		To:            getenv("CERTIFY_WHATSAPP_TO"),
		Template:      getenv("CERTIFY_WHATSAPP_TEMPLATE"),
		HTTPClient:    hc,
	}
	cfg.OIDC = OIDCConfig{
		Issuer:     getenv("OIDC_ISSUER"),
		Audience:   getenv("OIDC_AUDIENCE"),
		JWKSURL:    getenv("OIDC_JWKS_URL"),
		RolesClaim: getenv("OIDC_ROLES_CLAIM"),
		StaffToken: getenv("CERTIFY_OIDC_STAFF_TOKEN"),
		HTTPClient: hc,
	}
	if v := getenv("OIDC_MFA_AMR"); v != "" && v != "none" {
		cfg.OIDC.MFAValues = splitCSV(v)
	}
	return cfg
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
