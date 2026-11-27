package certify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/vtpass"
	"github.com/teamthy/meterly/internal/providers/vtpass/vtpasstest"
)

type VTPassConfig struct {
	APIKey     string
	SecretKey  string
	Phone      string
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
	AllowLive  bool
	SkipPay    bool
}

func (c VTPassConfig) skipReason() string {
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.SecretKey) == "" {
		return "VTPASS_API_KEY/VTPASS_SECRET_KEY not set"
	}
	if strings.TrimSpace(c.Phone) == "" {
		return "VTPASS_DEFAULT_PHONE not set"
	}
	base := strings.ToLower(strings.TrimRight(c.BaseURL, "/"))
	if !c.AllowLive && base != "" && strings.Contains(base, "vtpass.com") && !strings.Contains(base, "sandbox") {
		return "refusing live VTpass URL (set CERTIFY_ALLOW_LIVE=true to override)"
	}
	return ""
}

func VTPass(ctx context.Context, cfg VTPassConfig) []Result {
	const prov = "vtpass"
	scenarios := []string{
		"merchant_verify_prepaid",
		"merchant_verify_postpaid",
		"pay_prepaid_token",
		"pay_postpaid",
		"requery_delivered",
		"requery_pending",
		"requery_not_found",
		"duplicate_request_id",
		"invalid_request_id_date",
		"low_balance",
	}
	if reason := cfg.skipReason(); reason != "" {
		out := make([]Result, 0, len(scenarios))
		for _, s := range scenarios {
			out = append(out, skipped(prov, s, reason))
		}
		return out
	}

	client, err := vtpass.New(vtpass.Config{
		APIKey: cfg.APIKey, SecretKey: cfg.SecretKey, DefaultPhone: cfg.Phone,
		BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, Now: cfg.Now,
	})
	if err != nil {
		out := make([]Result, 0, len(scenarios))
		for _, s := range scenarios {
			out = append(out, failed(prov, s, err.Error()))
		}
		return out
	}

	amount := money.New(50_000, money.NGN) // ₦500, the documented sandbox floor
	out := []Result{}

	out = append(out, timed(prov, "merchant_verify_prepaid", func() ([]string, error) {
		res, err := client.ValidateMeter(ctx, providers.MeterValidationRequest{
			MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
		})
		if err != nil {
			return nil, err
		}
		if !res.Valid || !strings.EqualFold(res.MeterType, "PREPAID") {
			return nil, fmt.Errorf("prepaid verify: valid=%v type=%s", res.Valid, res.MeterType)
		}
		return nil, nil
	}))

	out = append(out, timed(prov, "merchant_verify_postpaid", func() ([]string, error) {
		res, err := client.ValidateMeter(ctx, providers.MeterValidationRequest{
			MeterNumber: vtpasstest.PostpaidMeter, DiscoCode: "EKEDC",
		})
		if err != nil {
			return nil, err
		}
		if !res.Valid || !strings.EqualFold(res.MeterType, "POSTPAID") {
			return nil, fmt.Errorf("postpaid verify: valid=%v type=%s", res.Valid, res.MeterType)
		}
		return nil, nil
	}))

	if cfg.SkipPay {
		for _, s := range []string{"pay_prepaid_token", "pay_postpaid", "requery_delivered", "requery_pending", "duplicate_request_id"} {
			out = append(out, skipped(prov, s, "CERTIFY_VTPASS_SKIP_PAY=true"))
		}
	} else {
		var prepaidKey string
		out = append(out, timed(prov, "pay_prepaid_token", func() ([]string, error) {
			key := client.NewVendKey("certify-pre-"+newRef("v"), time.Now())
			res, err := client.Vend(ctx, providers.VendRequest{
				TransactionID: "certify-pre", MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
				Amount: amount, IdempotencyKey: key, MeterType: "PREPAID", Phone: cfg.Phone,
			})
			if err != nil {
				return []string{key}, err
			}
			if res.Status != providers.VendSucceeded {
				return []string{key}, fmt.Errorf("prepaid vend status=%s code=%s", res.Status, res.FailureCode)
			}
			if res.Token == "" {
				return []string{key}, errors.New("prepaid vend succeeded without an STS token")
			}
			prepaidKey = key
			return []string{key, res.ProviderRef}, nil
		}))

		out = append(out, timed(prov, "pay_postpaid", func() ([]string, error) {
			key := client.NewVendKey("certify-post-"+newRef("v"), time.Now())
			res, err := client.Vend(ctx, providers.VendRequest{
				TransactionID: "certify-post", MeterNumber: vtpasstest.PostpaidMeter, DiscoCode: "EKEDC",
				Amount: amount, IdempotencyKey: key, MeterType: "POSTPAID", Phone: cfg.Phone,
			})
			if err != nil {
				return []string{key}, err
			}
			if res.Status != providers.VendSucceeded {
				return []string{key}, fmt.Errorf("postpaid vend status=%s code=%s", res.Status, res.FailureCode)
			}
			return []string{key, res.ProviderRef}, nil
		}))

		out = append(out, timed(prov, "requery_delivered", func() ([]string, error) {
			if prepaidKey == "" {
				return nil, errors.New("no prepaid vend to requery")
			}
			res, err := client.GetVendStatus(ctx, prepaidKey)
			if err != nil {
				return []string{prepaidKey}, err
			}
			if res.Status != providers.VendSucceeded || res.Token == "" {
				return []string{prepaidKey}, fmt.Errorf("requery delivered: status=%s token_present=%v", res.Status, res.Token != "")
			}
			return []string{prepaidKey}, nil
		}))

		out = append(out, timed(prov, "requery_pending", func() ([]string, error) {
			key := client.NewVendKey("certify-pend-"+newRef("v"), time.Now())
			res, err := client.Vend(ctx, providers.VendRequest{
				TransactionID: "certify-pend", MeterNumber: vtpasstest.PendingMeter, DiscoCode: "IKEDC",
				Amount: amount, IdempotencyKey: key, MeterType: "PREPAID", Phone: cfg.Phone,
			})
			if err != nil {
				return []string{key}, err
			}
			if res.Status != providers.VendPending {
				return []string{key}, fmt.Errorf("pending meter status=%s", res.Status)
			}
			q, err := client.GetVendStatus(ctx, key)
			if err != nil {
				return []string{key}, err
			}
			if q.Status != providers.VendPending {
				return []string{key}, fmt.Errorf("requery pending: status=%s", q.Status)
			}
			return []string{key}, nil
		}))

		out = append(out, timed(prov, "duplicate_request_id", func() ([]string, error) {
			if prepaidKey == "" {
				return nil, errors.New("no prepaid vend to replay")
			}
			res, err := client.Vend(ctx, providers.VendRequest{
				TransactionID: "certify-pre", MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
				Amount: amount, IdempotencyKey: prepaidKey, MeterType: "PREPAID", Phone: cfg.Phone,
			})
			if err != nil {
				return []string{prepaidKey}, err
			}
			// 014 must be reconciled by requery, not treated as a new pay.
			if res.Status != providers.VendSucceeded && res.Status != providers.VendPending {
				return []string{prepaidKey}, fmt.Errorf("duplicate request_id status=%s", res.Status)
			}
			return []string{prepaidKey}, nil
		}))
	}

	out = append(out, timed(prov, "requery_not_found", func() ([]string, error) {
		key := vtpass.FormatRequestID("certify-missing", time.Now())
		res, err := client.GetVendStatus(ctx, key)
		if err != nil {
			return []string{key}, err
		}
		if res.Status != providers.VendNotFound {
			return []string{key}, fmt.Errorf("missing request_id status=%s (want NOT_FOUND)", res.Status)
		}
		return []string{key}, nil
	}))

	out = append(out, timed(prov, "invalid_request_id_date", func() ([]string, error) {
		// 12 digits that are not a real YYYYMMDDHHII → VTpass 085.
		key := "999999999999certify"
		res, err := client.Vend(ctx, providers.VendRequest{
			TransactionID: "certify-085", MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
			Amount: amount, IdempotencyKey: key, MeterType: "PREPAID", Phone: cfg.Phone,
		})
		if err != nil {
			// A definitive rejection is acceptable.
			return []string{key}, nil
		}
		if res.Status == providers.VendSucceeded {
			return []string{key}, errors.New("invalid request_id date was accepted")
		}
		if res.FailureCode != "" && res.FailureCode != "085" && res.Status != providers.VendFailed && res.Status != providers.VendPending {
			return []string{key}, fmt.Errorf("unexpected 085 mapping: status=%s code=%s", res.Status, res.FailureCode)
		}
		return []string{key}, nil
	}))

	out = append(out, timed(prov, "low_balance", func() ([]string, error) {
		bal, err := client.WalletBalance(ctx)
		if err != nil {
			return nil, err
		}
		if bal.Minor > 0 {
			return nil, errSkip{reason: fmt.Sprintf("wallet funded (%d kobo); low-balance path not exercised", bal.Minor)}
		}
		key := client.NewVendKey("certify-low-"+newRef("v"), time.Now())
		res, err := client.Vend(ctx, providers.VendRequest{
			TransactionID: "certify-low", MeterNumber: vtpasstest.PrepaidMeter, DiscoCode: "IKEDC",
			Amount: amount, IdempotencyKey: key, MeterType: "PREPAID", Phone: cfg.Phone,
		})
		if err == nil && res.Status == providers.VendSucceeded {
			return []string{key}, errors.New("vend succeeded against an empty wallet")
		}
		return []string{key}, nil
	}))

	return out
}

type errSkip struct{ reason string }

func (e errSkip) Error() string { return "skip: " + e.reason }
