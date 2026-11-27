// Package certify runs live-sandbox certification scenarios against Meterly's
// provider adapters.
//
// Official docs cited by each scenario file:
//
//   - Paystack: https://paystack.com/docs/payments/test-payments
//     and https://paystack.com/docs/api/charge
//   - VTpass: https://www.vtpass.com/documentation/
//     and https://www.vtpass.com/documentation/response-codes/
//   - Africa's Talking: https://developers.africastalking.com/docs/sms/sending/bulk
//   - Resend: https://resend.com/docs/api-reference/emails/send-email
//     and https://resend.com/docs/dashboard/webhooks/verify-webhooks-requests
//   - WhatsApp Cloud API: https://developers.facebook.com/docs/whatsapp/cloud-api/reference/messages
//     and https://developers.facebook.com/docs/graph-api/webhooks/getting-started
//
// Live scenarios are Go tests behind the `certify` build tag
// (`go test -tags certify ./internal/certify/...`) and are skipped unless
// that provider's keys are set. They never run in normal CI.
//
// Reports (certify-report.json) contain provider, scenario, pass/fail/skipped,
// latency and provider request/reference IDs. No secrets, tokens or meter
// numbers.
package certify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"
)

type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

type Result struct {
	Provider    string   `json:"provider"`
	Scenario    string   `json:"scenario"`
	Status      Status   `json:"status"`
	LatencyMS   int64    `json:"latency_ms"`
	ProviderIDs []string `json:"provider_ids,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type Summary struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type Report struct {
	GeneratedAt string   `json:"generated_at"`
	Results     []Result `json:"results"`
	Summary     Summary  `json:"summary"`
}

// Config is assembled from the process environment (FromEnv) or by tests
// pointing adapters at in-repo fakes.
type Config struct {
	AllowLive      bool
	Paystack       PaystackConfig
	VTPass         VTPassConfig
	AfricasTalking ATConfig
	Resend         ResendConfig
	WhatsApp       WhatsAppConfig
	OIDC           OIDCConfig
}

func Run(ctx context.Context, cfg Config) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	var results []Result
	results = append(results, Paystack(ctx, cfg.Paystack)...)
	results = append(results, VTPass(ctx, cfg.VTPass)...)
	results = append(results, AfricasTalking(ctx, cfg.AfricasTalking)...)
	results = append(results, Resend(ctx, cfg.Resend)...)
	results = append(results, WhatsApp(ctx, cfg.WhatsApp)...)
	results = append(results, OIDC(ctx, cfg.OIDC)...)
	return assemble(results)
}

func assemble(results []Result) Report {
	rep := Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Results: make([]Result, 0, len(results))}
	for _, r := range results {
		r = r.clean()
		rep.Results = append(rep.Results, r)
		switch r.Status {
		case StatusPass:
			rep.Summary.Passed++
		case StatusFail:
			rep.Summary.Failed++
		default:
			rep.Summary.Skipped++
		}
	}
	return rep
}

func WriteFile(path string, rep Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func (r Result) clean() Result {
	r.Detail = Sanitize(r.Detail)
	r.Error = Sanitize(r.Error)
	return r
}

func skipped(provider, scenario, reason string) Result {
	return Result{Provider: provider, Scenario: scenario, Status: StatusSkipped, Detail: reason}
}

func failed(provider, scenario, err string) Result {
	return Result{Provider: provider, Scenario: scenario, Status: StatusFail, Error: err}
}

func timed(provider, scenario string, fn func() ([]string, error)) (r Result) {
	r.Provider, r.Scenario = provider, scenario
	start := time.Now()
	defer func() {
		r.LatencyMS = time.Since(start).Milliseconds()
		if rec := recover(); rec != nil {
			r.Status = StatusFail
			r.Error = "panic"
		}
	}()
	ids, err := fn()
	r.ProviderIDs = ids
	if err != nil {
		var es errSkip
		if errors.As(err, &es) {
			r.Status = StatusSkipped
			r.Detail = es.reason
			return r
		}
		r.Status = StatusFail
		r.Error = err.Error()
		return r
	}
	r.Status = StatusPass
	return r
}

var (
	secretRe = regexp.MustCompile(`(?i)(sk_live_|sk_test_|pk_live_|pk_test_|re_|whsec_|Bearer |AUTH_|api-key|secret-key)[^\s",]*`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// STS tokens are 20 digits; meter numbers are typically 11–13. Leave
	// shorter ids (Paystack refs, VTpass dated request_ids) alone when they
	// appear in provider_ids; this runs only on detail/error text.
	digitRe = regexp.MustCompile(`\d{11,}`)
)

// Sanitize strips secrets, emails, tokens and meter-like digit runs from
// free-text fields so a report can be committed or attached to a PR.
func Sanitize(s string) string {
	s = secretRe.ReplaceAllString(s, "[redacted]")
	s = emailRe.ReplaceAllString(s, "[redacted-email]")
	s = digitRe.ReplaceAllString(s, "[redacted-digits]")
	return strings.TrimSpace(s)
}
