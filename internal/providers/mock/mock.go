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

type mockSavedAuth struct {
	auth         providers.CardAuthorization
	chargeStatus providers.PaymentStatus // default PaymentSucceeded
	ambiguousN   int                     // next N ChargeSaved calls apply then return ErrAmbiguous
}

type Payments struct {
	mu          sync.Mutex
	pays        map[string]*providers.PaymentVerification
	emails      map[string]string
	byOurRef    map[string]string
	auths       map[string]*mockSavedAuth
	Refunds     []providers.RefundRequest
	ChargeCalls int
}

func NewPayments() *Payments {
	return &Payments{
		pays:     map[string]*providers.PaymentVerification{},
		emails:   map[string]string{},
		byOurRef: map[string]string{},
		auths:    map[string]*mockSavedAuth{},
	}
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
	p.emails[ref] = strings.ToLower(strings.TrimSpace(req.Email))
	p.pays[ref] = &providers.PaymentVerification{ProviderRef: ref, Status: providers.PaymentPending, Amount: req.Amount}
	return providers.PaymentInitResult{ProviderRef: ref, CheckoutURL: "https://checkout.mock/" + ref}, nil
}

// SetAuthorization overrides the card authorization attached to ref before or after Complete.
func (p *Payments) SetAuthorization(ref string, auth *providers.CardAuthorization) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.pays[ref]; ok {
		if auth == nil {
			v.Authorization = nil
			return
		}
		cp := *auth
		if cp.CustomerEmail == "" {
			cp.CustomerEmail = p.emails[ref]
		}
		v.Authorization = &cp
		if cp.AuthorizationCode != "" {
			p.auths[cp.AuthorizationCode] = &mockSavedAuth{auth: cp, chargeStatus: providers.PaymentSucceeded}
		}
	}
}

// SetSavedChargeOutcome configures future ChargeSaved calls for authCode (or "" for all known auths).
func (p *Payments) SetSavedChargeOutcome(authCode string, status providers.PaymentStatus, ambiguousNext int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for code, a := range p.auths {
		if authCode == "" || code == authCode {
			a.chargeStatus = status
			a.ambiguousN = ambiguousNext
		}
	}
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
		if v.Authorization == nil {
			code := "AUTH_mock_" + ref
			ca := providers.CardAuthorization{
				AuthorizationCode: code,
				Reusable:          true,
				CardBrand:         "visa",
				Last4:             "4242",
				ExpMonth:          12,
				ExpYear:           2030,
				Channel:           "card",
				Signature:         "SIG_mock_" + ref,
				CustomerEmail:     p.emails[ref],
			}
			v.Authorization = &ca
			p.auths[code] = &mockSavedAuth{auth: ca, chargeStatus: providers.PaymentSucceeded}
		}
	} else {
		v.Status = providers.PaymentFailed
	}
	return nil
}

func (p *Payments) ChargeSaved(_ context.Context, req providers.ChargeRequest) (providers.PaymentVerification, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ChargeCalls++
	if req.Reference == "" || req.AuthorizationRef == "" || req.Email == "" {
		return providers.PaymentVerification{}, fmt.Errorf("mockpay: invalid charge_saved request")
	}
	if _, dup := p.pays[req.Reference]; dup {
		return providers.PaymentVerification{}, fmt.Errorf("mockpay: duplicate reference %s", req.Reference)
	}
	sa, ok := p.auths[req.AuthorizationRef]
	if !ok || !sa.auth.Reusable {
		return providers.PaymentVerification{}, fmt.Errorf("mockpay: invalid or non-reusable authorization")
	}
	if sa.auth.CustomerEmail != "" && !strings.EqualFold(sa.auth.CustomerEmail, strings.TrimSpace(req.Email)) {
		return providers.PaymentVerification{}, fmt.Errorf("mockpay: customer email mismatch")
	}
	st := sa.chargeStatus
	if st == "" {
		st = providers.PaymentSucceeded
	}
	var paidAt *time.Time
	if st == providers.PaymentSucceeded {
		now := time.Now().UTC()
		paidAt = &now
	}
	cpAuth := sa.auth
	rec := &providers.PaymentVerification{
		ProviderRef:   req.Reference,
		Status:        st,
		Amount:        req.Amount,
		PaidAt:        paidAt,
		Authorization: &cpAuth,
	}
	p.pays[req.Reference] = rec
	if sa.ambiguousN > 0 {
		sa.ambiguousN--
		return providers.PaymentVerification{}, providers.ErrAmbiguous
	}
	out := *rec
	if rec.Authorization != nil {
		ac := *rec.Authorization
		out.Authorization = &ac
	}
	return out, nil
}

func (p *Payments) VerifyPayment(_ context.Context, ref string) (providers.PaymentVerification, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.pays[ref]
	if !ok {
		return providers.PaymentVerification{}, fmt.Errorf("%w: mockpay unknown ref %s", providers.ErrPaymentNotFound, ref)
	}
	out := *v
	if v.Authorization != nil {
		ac := *v.Authorization
		out.Authorization = &ac
	}
	return out, nil
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

func (v *Vending) NewVendKey(transactionID string, _ time.Time) string {
	return "vend:" + transactionID
}

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

// ---- Remote meter readings -------------------------------------------------

type readingEntry struct {
	kWh float64
	at  time.Time
}

type MeterReadings struct {
	mu       sync.Mutex
	readings map[string]readingEntry
	Calls    int
}

func NewMeterReadings() *MeterReadings {
	return &MeterReadings{readings: map[string]readingEntry{}}
}

func (m *MeterReadings) Name() string { return "mockreadings" }

func (m *MeterReadings) SetReading(meterNumber string, kWh float64, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	m.readings[meterNumber] = readingEntry{kWh: kWh, at: at.UTC()}
}

func (m *MeterReadings) LatestReading(_ context.Context, meterNumber string) (float64, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	r, ok := m.readings[meterNumber]
	if !ok {
		return 0, time.Time{}, providers.ErrReadingUnavailable
	}
	return r.kWh, r.at, nil
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
