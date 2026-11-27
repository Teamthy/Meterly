package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/receipts"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

var (
	ErrLinkNotFound    = errors.New("payment request not found")
	ErrLinkAlreadyPaid = errors.New("payment request has already been paid")
	ErrLinkExpired     = errors.New("payment request has expired")
	ErrLinkCancelled   = errors.New("payment request has been cancelled")
	ErrLinkUnusable    = errors.New("payment request meter is no longer accessible")
)

const opPublicPayCheckout = "public_pay_checkout"

// CreatePayRequestParams configures a new shareable Pay-for-Me link.
type CreatePayRequestParams struct {
	RequesterID    string
	MeterID        uuid.UUID
	AmountMode     payrequests.AmountMode
	AmountMinor    int64
	MinAmountMinor int64
	MaxAmountMinor int64
	Note           string
	TTL            time.Duration // 0 uses payrequests.DefaultTTL (72h); max 7d
	MaxPayments    int           // 0 defaults to 1
	PublicBaseURL  string
}

// PayRequestView is the requester-facing representation of a payment request.
type PayRequestView struct {
	ID                uuid.UUID              `json:"id"`
	MeterID           uuid.UUID              `json:"meter_id"`
	MaskedMeterNumber string                 `json:"masked_meter_number"`
	Disco             string                 `json:"disco"`
	AmountMode        payrequests.AmountMode `json:"amount_mode"`
	AmountMinor       int64                  `json:"amount_minor,omitempty"`
	MinAmountMinor    int64                  `json:"min_amount_minor,omitempty"`
	MaxAmountMinor    int64                  `json:"max_amount_minor,omitempty"`
	Currency          money.Currency         `json:"currency"`
	Note              string                 `json:"note,omitempty"`
	Status            payrequests.Status     `json:"status"`
	MaxPayments       int                    `json:"max_payments"`
	PaymentsCount     int                    `json:"payments_count"`
	ExpiresAt         time.Time              `json:"expires_at"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

// CreatedPayRequest includes the one-time shareable code, URL and QR PNG.
type CreatedPayRequest struct {
	PayRequestView
	Code        string `json:"code"`
	ShareURL    string `json:"share_url"`
	QRPNGBase64 string `json:"qr_png_base64"`
}

// PublicPayLinkView is the privacy-preserving public view of a Pay-for-Me link.
// It never exposes meter address, DisCo validation customer_name, or full meter numbers.
type PublicPayLinkView struct {
	RequesterDisplayName string                 `json:"requester_display_name"`
	MaskedMeterNumber    string                 `json:"masked_meter_number"`
	Disco                string                 `json:"disco"`
	AmountMode           payrequests.AmountMode `json:"amount_mode"`
	AmountMinor          int64                  `json:"amount_minor,omitempty"`
	MinAmountMinor       int64                  `json:"min_amount_minor,omitempty"`
	MaxAmountMinor       int64                  `json:"max_amount_minor,omitempty"`
	FeeMinor             int64                  `json:"fee_minor"`
	Currency             money.Currency         `json:"currency"`
	Note                 string                 `json:"note,omitempty"`
	Status               payrequests.Status     `json:"status"`
	MaxPayments          int                    `json:"max_payments"`
	PaymentsCount        int                    `json:"payments_count"`
	ExpiresAt            time.Time              `json:"expires_at"`
}

// PublicCheckoutRequest initiates a guest checkout against a Pay-for-Me link.
type PublicCheckoutRequest struct {
	Code           string
	AmountMinor    int64
	Email          string // sensitive; never logged or stored in plaintext
	PayerName      string // optional; never logged or stored in plaintext
	IdempotencyKey string
	IP             string
	UserAgent      string
}

// PublicCheckoutResult returns the transaction checkout and guest status code.
type PublicCheckoutResult struct {
	Transaction *tx.Transaction
	StatusCode  string
}

// PublicPayStatusView is the token-free guest status and receipt response.
type PublicPayStatusView struct {
	Status         tx.Overall        `json:"status"`
	PaymentStatus  tx.PaymentState   `json:"payment_status"`
	VendingStatus  tx.VendingState   `json:"vending_status"`
	DeliveryStatus tx.DeliveryState  `json:"delivery_status"`
	DeliveryMethod tx.DeliveryMethod `json:"delivery_method"`
	Receipt        receipts.Receipt  `json:"receipt"`
}

func (o *Orchestrator) CreatePaymentRequest(ctx context.Context, p CreatePayRequestParams) (*CreatedPayRequest, error) {
	note, err := payrequests.ValidateNote(p.Note)
	if err != nil {
		return nil, err
	}
	ttl := p.TTL
	if ttl == 0 {
		ttl = payrequests.DefaultTTL
	}
	if ttl < 0 || ttl > payrequests.MaxTTL {
		return nil, payrequests.ErrInvalidTTL
	}
	maxPays := p.MaxPayments
	if maxPays == 0 {
		maxPays = 1
	}
	if maxPays < 1 || maxPays > payrequests.MaxPaymentsCap {
		return nil, payrequests.ErrInvalidMaxPays
	}

	switch p.AmountMode {
	case payrequests.ModeFixed:
		if p.MinAmountMinor != 0 || p.MaxAmountMinor != 0 {
			return nil, payrequests.ErrInvalidMode
		}
		if p.AmountMinor < o.cfg.MinAmountMinor || p.AmountMinor > o.cfg.MaxAmountMinor {
			return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
		}
	case payrequests.ModeRange:
		if p.AmountMinor != 0 || p.MinAmountMinor > p.MaxAmountMinor {
			return nil, payrequests.ErrInvalidMode
		}
		if p.MinAmountMinor < o.cfg.MinAmountMinor || p.MaxAmountMinor > o.cfg.MaxAmountMinor {
			return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
		}
	default:
		return nil, payrequests.ErrInvalidMode
	}

	code, err := payrequests.GenerateCode()
	if err != nil {
		return nil, err
	}
	codeHMAC, ok := payrequests.LinkCodeHMAC(o.box, code)
	if !ok {
		return nil, errors.New("payrequests: failed to derive link code HMAC")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(p.PublicBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://meterly.ng"
	}
	shareURL := baseURL + "/p/" + code
	qrB64, err := payrequests.EncodeQRPNGBase64(shareURL)
	if err != nil {
		return nil, err
	}

	now := o.Now().UTC()
	reqID := uuid.New()
	noteCT, noteKV, err := payrequests.SealNote(o.box, reqID, note)
	if err != nil {
		return nil, err
	}

	pr := &payrequests.PaymentRequest{
		ID:             reqID,
		RequesterID:    p.RequesterID,
		MeterID:        p.MeterID,
		AmountMode:     p.AmountMode,
		AmountMinor:    p.AmountMinor,
		MinAmountMinor: p.MinAmountMinor,
		MaxAmountMinor: p.MaxAmountMinor,
		Currency:       money.NGN,
		NoteCiphertext: noteCT,
		NoteKeyVersion: noteKV,
		Status:         payrequests.StatusOpen,
		MaxPayments:    maxPays,
		PaymentsCount:  0,
		CodeHMAC:       codeHMAC,
		ExpiresAt:      now.Add(ttl),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	var m *meters.Meter
	err = o.st.InTx(ctx, func(r store.Repos) error {
		var authErr error
		m, authErr = registry.AuthorizeForUpdateIn(ctx, r, p.RequesterID, p.MeterID, meters.PermPurchase)
		if authErr != nil {
			return authErr
		}
		if err := r.PayRequests().Create(ctx, pr); err != nil {
			return err
		}
		actor := audit.Actor{UserID: p.RequesterID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("payment_request.created", "payment_request", pr.ID, map[string]any{
			"meter_id":     pr.MeterID,
			"amount_mode":  pr.AmountMode,
			"max_payments": pr.MaxPayments,
			"expires_at":   pr.ExpiresAt,
		}))
	})
	if err != nil {
		return nil, err
	}

	return &CreatedPayRequest{
		PayRequestView: o.buildPayRequestView(pr, m, note, now),
		Code:           code,
		ShareURL:       shareURL,
		QRPNGBase64:    qrB64,
	}, nil
}

func (o *Orchestrator) ListPaymentRequests(ctx context.Context, requesterID string) ([]PayRequestView, error) {
	list, err := o.st.Repos().PayRequests().ListForRequester(ctx, requesterID)
	if err != nil {
		return nil, err
	}
	now := o.Now().UTC()
	out := make([]PayRequestView, 0, len(list))
	for _, pr := range list {
		m, err := o.st.Repos().Meters().Get(ctx, pr.MeterID)
		if err != nil {
			return nil, err
		}
		note, err := payrequests.OpenNote(o.box, pr.ID, pr.NoteCiphertext)
		if err != nil {
			return nil, err
		}
		out = append(out, o.buildPayRequestView(pr, m, note, now))
	}
	return out, nil
}

func (o *Orchestrator) CancelPaymentRequest(ctx context.Context, requesterID string, id uuid.UUID) (*PayRequestView, error) {
	now := o.Now().UTC()
	var (
		pr   *payrequests.PaymentRequest
		m    *meters.Meter
		note string
	)
	err := o.st.InTx(ctx, func(r store.Repos) error {
		var err error
		pr, err = r.PayRequests().GetForUpdate(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && pr.RequesterID != requesterID) {
			return ErrLinkNotFound
		}
		if err != nil {
			return err
		}
		m, err = r.Meters().Get(ctx, pr.MeterID)
		if err != nil {
			return err
		}
		note, err = payrequests.OpenNote(o.box, pr.ID, pr.NoteCiphertext)
		if err != nil {
			return err
		}
		switch pr.EffectiveStatus(now) {
		case payrequests.StatusCancelled:
			return nil
		case payrequests.StatusPaid:
			return ErrLinkAlreadyPaid
		case payrequests.StatusExpired:
			return ErrLinkExpired
		}
		pr.Status = payrequests.StatusCancelled
		pr.UpdatedAt = now
		if err := r.PayRequests().Update(ctx, pr); err != nil {
			return err
		}
		actor := audit.Actor{UserID: requesterID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("payment_request.cancelled", "payment_request", pr.ID, map[string]any{
			"meter_id": pr.MeterID,
		}))
	})
	if err != nil {
		return nil, err
	}
	v := o.buildPayRequestView(pr, m, note, now)
	return &v, nil
}

func (o *Orchestrator) GetPublicPayLink(ctx context.Context, rawCode string) (*PublicPayLinkView, error) {
	codeHMAC, ok := payrequests.LinkCodeHMAC(o.box, rawCode)
	if !ok {
		return nil, ErrLinkNotFound
	}
	pr, err := o.st.Repos().PayRequests().GetByCodeHMAC(ctx, codeHMAC)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	m, err := o.st.Repos().Meters().Get(ctx, pr.MeterID)
	if err != nil {
		return nil, err
	}
	note, err := payrequests.OpenNote(o.box, pr.ID, pr.NoteCiphertext)
	if err != nil {
		return nil, err
	}

	displayName := "Meterly customer"
	if c, err := o.st.Repos().Users().Contacts(ctx, pr.RequesterID); err == nil && strings.TrimSpace(c.DisplayName) != "" {
		displayName = c.DisplayName
	}

	now := o.Now().UTC()
	return &PublicPayLinkView{
		RequesterDisplayName: displayName,
		MaskedMeterNumber:    m.Masked,
		Disco:                m.DiscoCode,
		AmountMode:           pr.AmountMode,
		AmountMinor:          pr.AmountMinor,
		MinAmountMinor:       pr.MinAmountMinor,
		MaxAmountMinor:       pr.MaxAmountMinor,
		FeeMinor:             o.cfg.FeeMinor,
		Currency:             pr.Currency,
		Note:                 note,
		Status:               pr.EffectiveStatus(now),
		MaxPayments:          pr.MaxPayments,
		PaymentsCount:        pr.PaymentsCount,
		ExpiresAt:            pr.ExpiresAt,
	}, nil
}

func (o *Orchestrator) CheckoutPublicPayLink(ctx context.Context, req PublicCheckoutRequest) (res *PublicCheckoutResult, replayed bool, err error) {
	codeHMAC, ok := payrequests.LinkCodeHMAC(o.box, req.Code)
	if !ok {
		return nil, false, ErrLinkNotFound
	}
	normEmail, err := notifications.NormalizeEmail(req.Email)
	if err != nil {
		return nil, false, err
	}
	payerName, err := payrequests.ValidatePayerName(req.PayerName)
	if err != nil {
		return nil, false, err
	}

	idemScope := "paylink:" + codeHMAC
	reqHash := o.box.Hash(fmt.Sprintf("public-checkout:%s:%d:%s:%s", codeHMAC, req.AmountMinor, normEmail, payerName))

	if err := o.ensurePurchasesOpen(ctx); err != nil {
		return nil, false, err
	}

	now := o.Now().UTC()
	var (
		t          *tx.Transaction
		statusCode string
	)
	err = o.st.InTx(ctx, func(r store.Repos) error {
		if req.IdempotencyKey != "" {
			existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
				Scope:       idemScope,
				Operation:   opPublicPayCheckout,
				Key:         req.IdempotencyKey,
				RequestHash: reqHash,
				CreatedAt:   now,
				ExpiresAt:   now.Add(o.cfg.IdempotencyTTL),
			})
			if err != nil {
				return err
			}
			if existing != nil {
				if existing.RequestHash != reqHash {
					return idempotency.ErrConflict
				}
				if existing.ResultRef == "" {
					return idempotency.ErrInProgress
				}
				txnID, err := uuid.Parse(existing.ResultRef)
				if err != nil {
					return err
				}
				t, err = r.Transactions().Get(ctx, txnID)
				if err != nil {
					return err
				}
				payer, err := r.PayRequests().GetPayer(ctx, txnID)
				if err != nil {
					return err
				}
				statusCode, err = payrequests.OpenStatusCode(o.box, txnID, payer.StatusCodeCiphertext)
				if err != nil {
					return err
				}
				replayed = true
				return nil
			}
		}

		pr, err := r.PayRequests().GetByCodeHMACForUpdate(ctx, codeHMAC)
		if errors.Is(err, store.ErrNotFound) {
			return ErrLinkNotFound
		}
		if err != nil {
			return err
		}

		switch {
		case pr.Status == payrequests.StatusCancelled:
			return ErrLinkCancelled
		case pr.Status == payrequests.StatusPaid || pr.PaymentsCount >= pr.MaxPayments:
			return ErrLinkAlreadyPaid
		case pr.EffectiveStatus(now) == payrequests.StatusExpired:
			return ErrLinkExpired
		}

		// Re-verify under the meter lock that the requester still holds PURCHASE.
		m, err := registry.AuthorizeForUpdateIn(ctx, r, pr.RequesterID, pr.MeterID, meters.PermPurchase)
		if errors.Is(err, meters.ErrForbidden) || errors.Is(err, meters.ErrNotFound) {
			return ErrLinkUnusable
		}
		if err != nil {
			return err
		}

		var chosenMinor int64
		switch pr.AmountMode {
		case payrequests.ModeFixed:
			if req.AmountMinor != 0 && req.AmountMinor != pr.AmountMinor {
				return fmt.Errorf("%w: fixed link requires %d kobo", ErrAmountOutOfRange, pr.AmountMinor)
			}
			chosenMinor = pr.AmountMinor
		case payrequests.ModeRange:
			if req.AmountMinor < pr.MinAmountMinor || req.AmountMinor > pr.MaxAmountMinor {
				return fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, pr.MinAmountMinor, pr.MaxAmountMinor)
			}
			chosenMinor = req.AmountMinor
		default:
			return payrequests.ErrInvalidMode
		}
		if chosenMinor < o.cfg.MinAmountMinor || chosenMinor > o.cfg.MaxAmountMinor {
			return fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
		}
		if err := o.ensurePilot(ctx, pr.RequesterID, pr.MeterID, chosenMinor); err != nil {
			return err
		}

		totalMinor := chosenMinor + o.cfg.FeeMinor
		d, err := o.evaluateRisk(ctx, r, risk.Request{
			Surface: risk.SurfacePayForMe, UserID: pr.RequesterID, MeterID: pr.MeterID, LinkID: pr.ID,
			AmountMinor: totalMinor, EmailHMAC: o.riskHash("email", normEmail),
			IPHMAC: o.riskHash("ip", req.IP), DeviceHMAC: o.riskHash("device", req.UserAgent),
			IP: req.IP,
		})
		if err != nil {
			return err
		}

		options := []tx.DeliveryMethod{tx.MethodToken}
		method := tx.MethodToken
		if o.remote != nil && m.Capabilities.RemoteCreditUsable() {
			options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
			method = tx.MethodRemote
		}

		amt := money.New(chosenMinor, money.NGN)
		fee := money.New(o.cfg.FeeMinor, money.NGN)
		total, _ := amt.Add(fee)
		q := &tx.Quote{
			ID:              uuid.New(),
			CustomerID:      pr.RequesterID,
			MeterID:         pr.MeterID,
			Amount:          amt,
			Fee:             fee,
			Total:           total,
			DeliveryMethod:  method,
			DeliveryOptions: options,
			Used:            true,
			ExpiresAt:       now.Add(o.cfg.QuoteTTL),
			CreatedAt:       now,
		}
		if err := r.Quotes().Create(ctx, q); err != nil {
			return err
		}

		t = tx.New(pr.RequesterID, q, false, "", now)
		if err := r.Transactions().Create(ctx, t); err != nil {
			return err
		}
		if d.ID != uuid.Nil {
			if err := r.Risk().BindDecisionTxn(ctx, d.ID, t.ID); err != nil && !errors.Is(err, risk.ErrNotFound) {
				return err
			}
		}

		statusCode, err = payrequests.GenerateCode()
		if err != nil {
			return err
		}
		statusHMAC, ok := payrequests.StatusCodeHMAC(o.box, statusCode)
		if !ok {
			return errors.New("payrequests: failed to derive status code HMAC")
		}
		emailCT, emailKV, err := payrequests.SealPayerEmail(o.box, t.ID, normEmail)
		if err != nil {
			return err
		}
		nameCT, nameKV, err := payrequests.SealPayerName(o.box, t.ID, payerName)
		if err != nil {
			return err
		}
		scCT, scKV, err := payrequests.SealStatusCode(o.box, t.ID, statusCode)
		if err != nil {
			return err
		}

		if err := r.PayRequests().CreatePayer(ctx, payrequests.Payer{
			TransactionID:        t.ID,
			PaymentRequestID:     pr.ID,
			EmailCiphertext:      emailCT,
			EmailKeyVersion:      emailKV,
			PayerNameCiphertext:  nameCT,
			PayerNameKeyVersion:  nameKV,
			StatusCodeHMAC:       statusHMAC,
			StatusCodeCiphertext: scCT,
			StatusCodeKeyVersion: scKV,
			CreatedAt:            now,
		}); err != nil {
			return err
		}

		pr.PaymentsCount++
		if pr.PaymentsCount >= pr.MaxPayments {
			pr.Status = payrequests.StatusPaid
		}
		pr.UpdatedAt = now
		if err := r.PayRequests().Update(ctx, pr); err != nil {
			return err
		}

		if err := emit(ctx, r, "electricity.transaction.created", t.ID, map[string]any{
			"gift":               false,
			"pay_for_me":         true,
			"payment_request_id": pr.ID,
		}); err != nil {
			return err
		}

		if req.IdempotencyKey != "" {
			return r.Idempotency().Complete(ctx, idemScope, opPublicPayCheckout, req.IdempotencyKey, t.ID.String())
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	if t.Payment == tx.PayCreated {
		t, err = o.EnsurePaymentInitialised(ctx, t.ID, normEmail)
	}
	return &PublicCheckoutResult{
		Transaction: t,
		StatusCode:  statusCode,
	}, replayed, err
}

func (o *Orchestrator) GetPublicPayStatus(ctx context.Context, rawStatusCode string) (*PublicPayStatusView, error) {
	statusHMAC, ok := payrequests.StatusCodeHMAC(o.box, rawStatusCode)
	if !ok {
		return nil, ErrLinkNotFound
	}
	payer, err := o.st.Repos().PayRequests().GetPayerByStatusHMAC(ctx, statusHMAC)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrLinkNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := o.st.Repos().Transactions().Get(ctx, payer.TransactionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrLinkNotFound
		}
		return nil, err
	}
	m, err := o.st.Repos().Meters().Get(ctx, t.MeterID)
	if err != nil {
		return nil, err
	}
	rec, err := receipts.Build(t, m)
	if err != nil {
		return nil, err
	}
	return &PublicPayStatusView{
		Status:         t.Status(),
		PaymentStatus:  t.Payment,
		VendingStatus:  t.Vending,
		DeliveryStatus: t.Delivery,
		DeliveryMethod: t.DeliveryMethod,
		Receipt:        rec,
	}, nil
}

// releasePayRequestSlotInTx reopens a Pay-for-Me payment slot atomically when
// an unpaid guest checkout fails or expires. No-op for regular transactions or
// already-released slots.
func (o *Orchestrator) releasePayRequestSlotInTx(ctx context.Context, r store.Repos, txnID uuid.UUID, now time.Time) error {
	payer, err := r.PayRequests().GetPayerForUpdate(ctx, txnID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && payer.SlotReleased) {
		return nil
	}
	if err != nil {
		return err
	}
	pr, err := r.PayRequests().GetForUpdate(ctx, payer.PaymentRequestID)
	if err != nil {
		return err
	}
	if pr.PaymentsCount > 0 {
		pr.PaymentsCount--
	}
	if pr.Status == payrequests.StatusPaid && pr.PaymentsCount < pr.MaxPayments {
		if now.After(pr.ExpiresAt) {
			pr.Status = payrequests.StatusExpired
		} else {
			pr.Status = payrequests.StatusOpen
		}
	}
	pr.UpdatedAt = now.UTC()
	if err := r.PayRequests().Update(ctx, pr); err != nil {
		return err
	}
	return r.PayRequests().MarkPayerSlotReleased(ctx, txnID)
}

func (o *Orchestrator) buildPayRequestView(pr *payrequests.PaymentRequest, m *meters.Meter, note string, now time.Time) PayRequestView {
	return PayRequestView{
		ID:                pr.ID,
		MeterID:           pr.MeterID,
		MaskedMeterNumber: m.Masked,
		Disco:             m.DiscoCode,
		AmountMode:        pr.AmountMode,
		AmountMinor:       pr.AmountMinor,
		MinAmountMinor:    pr.MinAmountMinor,
		MaxAmountMinor:    pr.MaxAmountMinor,
		Currency:          pr.Currency,
		Note:              note,
		Status:            pr.EffectiveStatus(now),
		MaxPayments:       pr.MaxPayments,
		PaymentsCount:     pr.PaymentsCount,
		ExpiresAt:         pr.ExpiresAt,
		CreatedAt:         pr.CreatedAt,
		UpdatedAt:         pr.UpdatedAt,
	}
}
