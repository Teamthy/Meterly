//go:build certify

package certify_test

import (
	"context"
	"testing"

	"github.com/teamthy/meterly/internal/certify"
)

func TestLivePaystack(t *testing.T) {
	assertLive(t, certify.Paystack(context.Background(), certify.FromEnv().Paystack))
}
func TestLiveVTPass(t *testing.T) {
	assertLive(t, certify.VTPass(context.Background(), certify.FromEnv().VTPass))
}
func TestLiveAfricasTalking(t *testing.T) {
	assertLive(t, certify.AfricasTalking(context.Background(), certify.FromEnv().AfricasTalking))
}
func TestLiveResend(t *testing.T) {
	assertLive(t, certify.Resend(context.Background(), certify.FromEnv().Resend))
}
func TestLiveWhatsApp(t *testing.T) {
	assertLive(t, certify.WhatsApp(context.Background(), certify.FromEnv().WhatsApp))
}
func TestLiveOIDC(t *testing.T) {
	assertLive(t, certify.OIDC(context.Background(), certify.FromEnv().OIDC))
}

func assertLive(t *testing.T, results []certify.Result) {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("no scenarios")
	}
	skipped := 0
	reason := ""
	for _, r := range results {
		switch r.Status {
		case certify.StatusSkipped:
			skipped++
			if reason == "" {
				reason = r.Detail
			}
		case certify.StatusFail:
			t.Errorf("%s/%s: %s", r.Provider, r.Scenario, r.Error)
		}
	}
	if skipped == len(results) {
		if reason == "" {
			reason = "provider keys not set"
		}
		t.Skip(reason)
	}
}
