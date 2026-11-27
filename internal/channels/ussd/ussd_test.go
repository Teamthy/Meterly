package ussd_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/providers"
)

func TestUSSDAdapterAuthIPAllowlistAnd182Limit(t *testing.T) {
	adp, err := ussd.New(ussd.Config{
		SharedSecret: "at_ussd_secret_123",
		AllowedCIDRs: []string{"196.201.214.0/24", "127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"sessionId":   {"ATUid_abc123"},
		"serviceCode": {"*347*88#"},
		"phoneNumber": {"+2348031234567"},
		"text":        {"1*5000"},
	}
	body := []byte(form.Encode())

	// 1. Disallowed source IP rejected even with valid signature.
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/x-www-form-urlencoded")
	hdr.Set("X-USSD-Signature", ussd.Sign("at_ussd_secret_123", body))
	if _, err := adp.ParseRequest("203.0.113.9:443", hdr, body); !errors.Is(err, ussd.ErrForbiddenIP) {
		t.Fatalf("expected ErrForbiddenIP, got %v", err)
	}

	// 2. Bad signature from allowed IP rejected with ErrBadSignature.
	hdr.Set("X-USSD-Signature", "sha256=deadbeef")
	if _, err := adp.ParseRequest("196.201.214.42:443", hdr, body); !errors.Is(err, providers.ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}

	// 3. Valid IP + valid signature succeeds.
	hdr.Set("X-USSD-Signature", ussd.Sign("at_ussd_secret_123", body))
	in, err := adp.ParseRequest("196.201.214.42:443", hdr, body)
	if err != nil || in.SessionID != "ATUid_abc123" || in.PhoneNumber != "+2348031234567" || in.RawText != "1*5000" {
		t.Fatalf("unexpected ParseRequest output: %+v (%v)", in, err)
	}

	// 4. WriteReply enforces <= 182 characters.
	rr := httptest.NewRecorder()
	ussd.WriteReply(rr, "CON "+strings.Repeat("VeryLongLine ", 30))
	if rr.Code != http.StatusOK || len(rr.Body.String()) > conversation.MaxUSSDChars {
		t.Fatalf("WriteReply exceeded 182 chars: len=%d body=%q", len(rr.Body.String()), rr.Body.String())
	}
}
