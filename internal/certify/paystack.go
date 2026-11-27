package certify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/paystack"
)

// Documented Paystack test cards (https://paystack.com/docs/payments/test-payments).
const (
	PaystackTestCardSuccess = "4084084084084081"
	PaystackTestCardCVV     = "408"
	PaystackTestCardPIN     = "0000"
	PaystackTestCardOTP     = "123456"
	// A PAN that is not a documented success card; the fake and live sandbox
	// should refuse or fail it (failed-card path).
	PaystackTestCardDeclined = "0000000000000000"
)

type PaystackConfig struct {
	SecretKey           string
	BaseURL             string
	Email               string
	HTTPClient          *http.Client
	AllowLive           bool
	CapturedWebhookBody []byte
	CapturedWebhookSig  string
}

func (c PaystackConfig) skipReason() string {
	key := strings.TrimSpace(c.SecretKey)
	if key == "" {
		return "PAYSTACK_SECRET_KEY not set"
	}
	if strings.HasPrefix(key, "sk_live_") && !c.AllowLive {
		return "refusing sk_live_ (set CERTIFY_ALLOW_LIVE=true to override)"
	}
	return ""
}

func Paystack(ctx context.Context, cfg PaystackConfig) []Result {
	const prov = "paystack"
	scenarios := []string{
		"initialize_verify_success",
		"failed_card",
		"refund_and_list",
		"webhook_signature",
		"webhook_captured_payload",
		"charge_authorization",
		"reference_format",
	}
	if reason := cfg.skipReason(); reason != "" {
		out := make([]Result, 0, len(scenarios))
		for _, s := range scenarios {
			out = append(out, skipped(prov, s, reason))
		}
		return out
	}

	email := cfg.Email
	if email == "" {
		email = "certify@example.com"
	}
	client, err := paystack.New(paystack.Config{
		SecretKey:  cfg.SecretKey,
		BaseURL:    cfg.BaseURL,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		out := make([]Result, 0, len(scenarios))
		for _, s := range scenarios {
			out = append(out, failed(prov, s, err.Error()))
		}
		return out
	}

	var (
		paidRef string
		auth    *providers.CardAuthorization
		amount  = money.New(10_000, money.NGN) // ₦100; Paystack test minimum
	)

	out := []Result{}
	out = append(out, timed(prov, "initialize_verify_success", func() ([]string, error) {
		initRes, err := client.InitializePayment(ctx, providers.PaymentInit{
			Reference: newRef("certify"), Amount: amount, Email: email,
		})
		if err != nil {
			return nil, err
		}
		if initRes.ProviderRef == "" || initRes.CheckoutURL == "" {
			return nil, errors.New("initialize returned no reference or checkout URL")
		}
		if err := chargeTestCard(ctx, cfg, email, amount.Minor, initRes.ProviderRef, PaystackTestCardSuccess, true); err != nil {
			return []string{initRes.ProviderRef}, err
		}
		v, err := client.VerifyPayment(ctx, initRes.ProviderRef)
		if err != nil {
			return []string{initRes.ProviderRef}, err
		}
		if v.Status != providers.PaymentSucceeded || v.Amount != amount {
			return []string{initRes.ProviderRef}, fmt.Errorf("verify after test-card charge: status=%s amount=%d", v.Status, v.Amount.Minor)
		}
		paidRef = initRes.ProviderRef
		auth = v.Authorization
		return []string{initRes.ProviderRef}, nil
	}))

	out = append(out, timed(prov, "failed_card", func() ([]string, error) {
		initRes, err := client.InitializePayment(ctx, providers.PaymentInit{
			Reference: newRef("certifyf"), Amount: amount, Email: email,
		})
		if err != nil {
			return nil, err
		}
		err = chargeTestCard(ctx, cfg, email, amount.Minor, initRes.ProviderRef, PaystackTestCardDeclined, true)
		if err == nil {
			v, vErr := client.VerifyPayment(ctx, initRes.ProviderRef)
			if vErr == nil && v.Status == providers.PaymentSucceeded {
				return []string{initRes.ProviderRef}, errors.New("declined test card was charged successfully")
			}
			return []string{initRes.ProviderRef}, nil
		}
		return []string{initRes.ProviderRef}, nil
	}))

	out = append(out, timed(prov, "refund_and_list", func() ([]string, error) {
		if paidRef == "" {
			return nil, errors.New("no successful charge to refund")
		}
		id, err := client.RefundPayment(ctx, providers.RefundRequest{
			ProviderRef: paidRef, Amount: amount, Reference: "refund-" + paidRef,
		})
		if err != nil {
			return []string{paidRef}, err
		}
		if id == "" {
			return []string{paidRef}, errors.New("refund returned empty id")
		}
		// A second call must reuse the listed refund (adapter lists first).
		id2, err := client.RefundPayment(ctx, providers.RefundRequest{
			ProviderRef: paidRef, Amount: amount, Reference: "refund-" + paidRef,
		})
		if err != nil {
			return []string{paidRef, id}, err
		}
		if id2 != id {
			return []string{paidRef, id, id2}, errors.New("refund listing did not reuse the existing refund")
		}
		return []string{paidRef, id}, nil
	}))

	out = append(out, timed(prov, "webhook_signature", func() ([]string, error) {
		body := []byte(`{"event":"charge.success","data":{"reference":"certify.sig"}}`)
		h := http.Header{}
		h.Set("X-Paystack-Signature", paystack.Sign(cfg.SecretKey, body))
		evt, err := client.ParseWebhook(h, body)
		if err != nil || !evt.Relevant || evt.Reference != "certify.sig" {
			return nil, fmt.Errorf("valid signature: relevant=%v ref=%s err=%v", evt.Relevant, evt.Reference, err)
		}
		hBad := http.Header{}
		hBad.Set("X-Paystack-Signature", paystack.Sign("sk_test_wrong", body))
		if _, err := client.ParseWebhook(hBad, body); !errors.Is(err, providers.ErrBadSignature) {
			return nil, fmt.Errorf("forged signature: %v", err)
		}
		return []string{"certify.sig"}, nil
	}))

	if len(bytes.TrimSpace(cfg.CapturedWebhookBody)) == 0 || cfg.CapturedWebhookSig == "" {
		out = append(out, skipped(prov, "webhook_captured_payload", "CERTIFY_PAYSTACK_WEBHOOK_BODY/SIGNATURE not set"))
	} else {
		out = append(out, timed(prov, "webhook_captured_payload", func() ([]string, error) {
			h := http.Header{}
			h.Set("X-Paystack-Signature", cfg.CapturedWebhookSig)
			evt, err := client.ParseWebhook(h, cfg.CapturedWebhookBody)
			if err != nil {
				return nil, err
			}
			ids := []string{}
			if evt.Reference != "" {
				ids = append(ids, evt.Reference)
			}
			return ids, nil
		}))
	}

	out = append(out, timed(prov, "charge_authorization", func() ([]string, error) {
		if auth == nil || !auth.Reusable || auth.AuthorizationCode == "" {
			return nil, errors.New("no reusable authorization from the test-card charge")
		}
		ref := newRef("apchg")
		chg, err := client.ChargeSaved(ctx, providers.ChargeRequest{
			AuthorizationRef: auth.AuthorizationCode,
			Email:            email,
			Amount:           amount,
			Reference:        ref,
		})
		if err != nil {
			return []string{ref}, err
		}
		if chg.Status != providers.PaymentSucceeded {
			return []string{ref}, fmt.Errorf("charge_authorization status=%s", chg.Status)
		}
		v, err := client.VerifyPayment(ctx, ref)
		if err != nil || v.Status != providers.PaymentSucceeded {
			return []string{ref}, fmt.Errorf("verify charged ref: %v status=%s", err, v.Status)
		}
		return []string{ref}, nil
	}))

	out = append(out, timed(prov, "reference_format", func() ([]string, error) {
		_, err := client.InitializePayment(ctx, providers.PaymentInit{
			Reference: "bad:colon", Amount: amount, Email: email,
		})
		if err == nil {
			return nil, errors.New("Paystack accepted a reference containing ':'")
		}
		return nil, nil
	}))

	return out
}

func newRef(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

type cardCharge struct {
	Number      string `json:"number"`
	CVV         string `json:"cvv"`
	ExpiryMonth string `json:"expiry_month"`
	ExpiryYear  string `json:"expiry_year"`
}

type chargeAPIData struct {
	Status    string `json:"status"`
	Reference string `json:"reference"`
	Message   string `json:"message"`
}

// chargeTestCard uses Paystack's documented Charge API
// (https://paystack.com/docs/api/charge) with a test card, submitting PIN/OTP
// when the sandbox asks. This is certify-only: production never sends raw
// card numbers.
func chargeTestCard(ctx context.Context, cfg PaystackConfig, email string, amountMinor int64, ref, pan string, followAuth bool) error {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = paystack.DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	body := map[string]any{
		"email":     email,
		"amount":    fmt.Sprintf("%d", amountMinor),
		"currency":  "NGN",
		"reference": ref,
		"card": cardCharge{
			Number: pan, CVV: PaystackTestCardCVV,
			ExpiryMonth: "12", ExpiryYear: "30",
		},
	}
	data, err := paystackPOST(ctx, hc, cfg.SecretKey, base+"/charge", body)
	if err != nil {
		return err
	}
	for i := 0; followAuth && i < 5; i++ {
		switch strings.ToLower(data.Status) {
		case "success", "failed", "reversed":
			if data.Status == "success" && pan == PaystackTestCardDeclined {
				return errors.New("declined card reported success")
			}
			if data.Status != "success" && pan == PaystackTestCardSuccess {
				return fmt.Errorf("success card status=%s", data.Status)
			}
			return nil
		case "send_pin":
			data, err = paystackPOST(ctx, hc, cfg.SecretKey, base+"/charge/submit_pin", map[string]any{
				"pin": PaystackTestCardPIN, "reference": ref,
			})
		case "send_otp":
			data, err = paystackPOST(ctx, hc, cfg.SecretKey, base+"/charge/submit_otp", map[string]any{
				"otp": PaystackTestCardOTP, "reference": ref,
			})
		default:
			return fmt.Errorf("charge status %q", data.Status)
		}
		if err != nil {
			return err
		}
	}
	if data.Status == "success" {
		return nil
	}
	if pan == PaystackTestCardDeclined {
		return nil
	}
	return fmt.Errorf("charge unfinished, status=%s", data.Status)
}

func paystackPOST(ctx context.Context, hc *http.Client, secret, url string, body any) (chargeAPIData, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return chargeAPIData{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return chargeAPIData{}, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return chargeAPIData{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return chargeAPIData{}, err
	}
	var env struct {
		Status  bool          `json:"status"`
		Message string        `json:"message"`
		Data    chargeAPIData `json:"data"`
	}
	if json.Unmarshal(b, &env) != nil {
		if res.StatusCode >= 400 {
			return chargeAPIData{}, fmt.Errorf("paystack charge HTTP %d", res.StatusCode)
		}
		return chargeAPIData{}, errors.New("paystack charge: unparseable response")
	}
	if res.StatusCode >= 400 || !env.Status {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return chargeAPIData{Status: "failed", Message: msg}, fmt.Errorf("paystack charge rejected: %s", msg)
	}
	if env.Data.Reference == "" {
		return env.Data, nil
	}
	return env.Data, nil
}
