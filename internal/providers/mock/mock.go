// Package mock provides deterministic in-memory providers for development
// and tests. Behaviour is driven by the meter number so every failure path
// can be exercised:
//
//	suffix 0000 -> vend fails definitively
//	suffix 9999 -> vend times out, but the provider DID vend (lost response)
//	suffix 8888 -> vend times out and the provider did NOT vend
//	prefix 7    -> meter supports remote credit
//	prefix 77   -> remote credit command times out after the meter was credited
//	prefix 76   -> remote credit accepted but meter later reports failure
//	prefix 0    -> meter validation fails
package mock

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/providers"
)

// ---- Payment ---------------------------------------------------------------

type Payments struct {
	mu       sync.Mutex
	pays     map[string]*providers.PaymentVerification
	byOurRef map[string]string
	Refunds  []providers.RefundRequest
}

func NewPayments() *Payments {
	return &Payments{pays: map[string]*providers.PaymentVerification{}, byOurRef: map[string]string{}}
}

func (p *Payments) Name() string { return "mockpay" }

func (p *Payments) InitializePayment(_ context.Context, req providers.PaymentInit) (providers.PaymentInitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ref, ok := p.byOurRef[req.Reference]; ok {
		return providers.PaymentInitResult{ProviderRef: ref, CheckoutURL: "https://checkout.mock/" + ref}, nil
	}
	ref := "mp_" + uuid.NewString()[:12]
	p.byOurRef[req.Reference] = ref
	p.pays[ref] = &providers.PaymentVerification{ProviderRef: ref, Status: providers.PaymentPending, Amount: req.Amount}
	return providers.PaymentInitResult{ProviderRef: ref, CheckoutURL: "https://checkout.mock/" + ref}, nil
}

// Complete simulates the customer finishing checkout. amountOverride lets
// tests simulate an underpayment (0 = pay the expected amount).
func (p *Payments) Complete(ref string, success bool, amountOverride int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.pays[ref]
	if !ok {
		return fmt.Errorf("mockpay: unknown ref %s", ref)
	}
	if success {
		v.Status = providers.PaymentSucceeded
		now := time.Now().UTC()
		v.PaidAt = &now
		if amountOverride > 0 {
			v.Amount.Minor = amountOverride
		}
	} else {
		v.Status = providers.PaymentFailed
	}
	return nil
}

func (p *Payments) VerifyPayment(_ context.Context, ref string) (providers.PaymentVerification, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.pays[ref]
	if !ok {
		return providers.PaymentVerification{}, fmt.Errorf("mockpay: unknown ref %s", ref)
	}
	return *v, nil
}

func (p *Payments) RefundPayment(_ context.Context, req providers.RefundRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.Refunds {
		if r.Reference == req.Reference {
			return "rf_" + req.Reference, nil
		}
	}
	p.Refunds = append(p.Refunds, req)
	return "rf_" + req.Reference, nil
}

// ParseWebhook accepts a fixed dev signature and a {"reference": "..."}
// body. Real adapters verify the provider's HMAC over the raw body.
func (p *Payments) ParseWebhook(h http.Header, body []byte) (providers.WebhookEvent, error) {
	if h.Get("X-Mock-Signature") != "dev-signature" {
		return providers.WebhookEvent{}, providers.ErrBadSignature
	}
	var evt struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.Reference == "" {
		return providers.WebhookEvent{}, fmt.Errorf("mockpay: reference missing")
	}
	return providers.WebhookEvent{Type: "payment", Reference: evt.Reference, Relevant: true}, nil
}

// ---- Vending ---------------------------------------------------------------

type Vending struct {
	mu    sync.Mutex
	vends map[string]providers.VendResult // by idempotency key
	Calls int
}

func NewVending() *Vending { return &Vending{vends: map[string]providers.VendResult{}} }

func (v *Vending) Name() string { return "mockvend" }

func (v *Vending) ValidateMeter(_ context.Context, req providers.MeterValidationRequest) (providers.MeterValidationResult, error) {
	n := req.MeterNumber
	if len(n) < 11 || len(n) > 13 || strings.HasPrefix(n, "0") || !digits(n) {
		return providers.MeterValidationResult{Valid: false}, nil
	}
	return providers.MeterValidationResult{
		Valid:                  true,
		MeterType:              "PREPAID",
		CustomerName:           "TEST CUSTOMER " + n[len(n)-4:],
		Address:                "12 Test Street, Ikeja, Lagos",
		RemoteCreditSupported:  strings.HasPrefix(n, "7"),
		RemoteReadingSupported: strings.HasPrefix(n, "7"),
	}, nil
}

func (v *Vending) Vend(_ context.Context, req providers.VendRequest) (providers.VendResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Calls++
	if r, ok := v.vends[req.IdempotencyKey]; ok {
		return r, nil // provider-side idempotency
	}
	n := req.MeterNumber
	switch {
	case strings.HasSuffix(n, "0000"):
		r := providers.VendResult{ProviderRef: "mv_" + uuid.NewString()[:12], Status: providers.VendFailed, FailureCode: "METER_BLOCKED"}
		v.vends[req.IdempotencyKey] = r
		return r, nil
	case strings.HasSuffix(n, "8888"):
		return providers.VendResult{}, providers.ErrAmbiguous
	}
	r := providers.VendResult{
		ProviderRef: "mv_" + uuid.NewString()[:12],
		Status:      providers.VendSucceeded,
		Token:       stsLikeToken(),
		UnitsKWh:    fmt.Sprintf("%.1f", float64(req.Amount.Minor)/100/209.5), // display only
	}
	v.vends[req.IdempotencyKey] = r
	if strings.HasSuffix(n, "9999") {
		return providers.VendResult{}, providers.ErrAmbiguous // vended, response lost
	}
	return r, nil
}

func (v *Vending) GetVendStatus(_ context.Context, key string) (providers.VendResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r, ok := v.vends[key]; ok {
		return r, nil
	}
	return providers.VendResult{Status: providers.VendNotFound}, nil
}

// ---- Remote credit --------------------------------------------------------

type RemoteCredit struct {
	mu      sync.Mutex
	credits map[string]providers.RemoteCreditResult
	Calls   int
}

func NewRemoteCredit() *RemoteCredit {
	return &RemoteCredit{credits: map[string]providers.RemoteCreditResult{}}
}

func (r *RemoteCredit) Name() string { return "mockhes" }

func (r *RemoteCredit) CheckEligibility(_ context.Context, meter string) (bool, *bool, error) {
	ok := strings.HasPrefix(meter, "7")
	return ok, &ok, nil
}

func (r *RemoteCredit) InitiateCredit(_ context.Context, req providers.RemoteCreditRequest) (providers.RemoteCreditResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls++
	if res, ok := r.credits[req.IdempotencyKey]; ok {
		return res, nil
	}
	if !strings.HasPrefix(req.MeterNumber, "7") {
		return providers.RemoteCreditResult{Status: providers.CreditFailed, FailureCode: "NOT_ELIGIBLE"}, nil
	}
	res := providers.RemoteCreditResult{ProviderRef: "mh_" + uuid.NewString()[:12], Status: providers.CreditAccepted, At: time.Now().UTC()}
	r.credits[req.IdempotencyKey] = res
	if strings.HasPrefix(req.MeterNumber, "77") {
		// Meter gets credited but our acknowledgement is lost.
		res.Status = providers.CreditConfirmed
		r.credits[req.IdempotencyKey] = res
		return providers.RemoteCreditResult{}, providers.ErrAmbiguous
	}
	return res, nil
}

// Settle simulates the meter acknowledging (or rejecting) queued commands.
func (r *RemoteCredit) Settle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, res := range r.credits {
		if res.Status == providers.CreditAccepted {
			res.Status = providers.CreditConfirmed
			res.At = time.Now().UTC()
			r.credits[k] = res
		}
	}
}

// FailPending simulates meters rejecting queued commands (prefix 76).
func (r *RemoteCredit) FailPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, res := range r.credits {
		if res.Status == providers.CreditAccepted {
			res.Status = providers.CreditFailed
			res.FailureCode = "METER_REJECTED"
			r.credits[k] = res
		}
	}
}

func (r *RemoteCredit) GetCreditStatus(_ context.Context, key string) (providers.RemoteCreditResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res, ok := r.credits[key]; ok {
		return res, nil
	}
	return providers.RemoteCreditResult{Status: providers.CreditNotFound}, nil
}

// ---- helpers ---------------------------------------------------------------

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func stsLikeToken() string {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteByte(byte('0' + n.Int64()))
	}
	return b.String()
}
