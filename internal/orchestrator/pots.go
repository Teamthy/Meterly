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
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

var (
	ErrPotNotFound       = errors.New("funding pot not found")
	ErrPotExpired        = errors.New("funding pot has expired")
	ErrPotCancelled      = errors.New("funding pot has been cancelled")
	ErrPotAlreadyFunded  = errors.New("funding pot is already funded")
	ErrPotClosed         = errors.New("funding pot is closed")
	ErrPotNotCancellable = errors.New("funding pot can no longer be cancelled")
	ErrPotUnusable       = errors.New("funding pot meter is no longer accessible")
)

const (
	opPotContribute = "pot_contribute"
	resourcePot     = "pot"
)

// CreatePotParams configures a new Split-the-Meter Funding Pot.
type CreatePotParams struct {
	OwnerID         string
	MeterID         uuid.UUID
	TargetMinor     int64 // includes the flat convenience fee
	SuggestedShares int
	Deadline        time.Time
	Note            string
	PublicBaseURL   string
}

// PotView is the common summary representation of a Funding Pot.
type PotView struct {
	ID                     uuid.UUID      `json:"id"`
	Kind                   pots.Kind      `json:"kind"`
	MeterID                uuid.UUID      `json:"meter_id"`
	MaskedMeterNumber      string         `json:"masked_meter_number"`
	Disco                  string         `json:"disco"`
	TargetMinor            int64          `json:"target_minor"`
	ElectricityAmountMinor int64          `json:"electricity_amount_minor"`
	FeeMinor               int64          `json:"fee_minor"`
	FundedMinor            int64          `json:"funded_minor"`
	RemainingMinor         int64          `json:"remaining_minor"`
	RefundedMinor          int64          `json:"refunded_minor"`
	PaidCount              int            `json:"paid_count"`
	SuggestedShares        int            `json:"suggested_shares,omitempty"`
	SuggestedShareMinor    int64          `json:"suggested_share_minor,omitempty"`
	Currency               money.Currency `json:"currency"`
	Note                   string         `json:"note,omitempty"`
	Status                 pots.Status    `json:"status"`
	ReviewReason           string         `json:"review_reason,omitempty"`
	Deadline               time.Time      `json:"deadline"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

// PotContributionOwnerView exposes masked contributor status to the organiser
// without revealing contributor email addresses, phone numbers or full names.
type PotContributionOwnerView struct {
	ID                uuid.UUID               `json:"id"`
	Initials          string                  `json:"initials"`
	AmountMinor       int64                   `json:"amount_minor"`
	RetainedMinor     int64                   `json:"retained_minor"`
	RefundAmountMinor int64                   `json:"refund_amount_minor,omitempty"`
	RefundedMinor     int64                   `json:"refunded_minor,omitempty"`
	Status            pots.ContributionStatus `json:"status"`
	PaidAt            *time.Time              `json:"paid_at,omitempty"`
	CreatedAt         time.Time               `json:"created_at"`
}

// PotAllocationView links a funded pot to the resulting electricity transaction.
type PotAllocationView struct {
	ID             uuid.UUID             `json:"id"`
	TransactionID  uuid.UUID             `json:"transaction_id"`
	MeterID        uuid.UUID             `json:"meter_id"`
	AmountMinor    int64                 `json:"amount_minor"`
	Status         pots.AllocationStatus `json:"status"`
	OverallStatus  tx.Overall            `json:"overall_status,omitempty"`
	DeliveryMethod tx.DeliveryMethod     `json:"delivery_method,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
}

// PotDetailView is the organiser-facing full detail response.
type PotDetailView struct {
	PotView
	Contributions []PotContributionOwnerView `json:"contributions"`
	Allocations   []PotAllocationView        `json:"allocations"`
}

// CreatedPot includes the one-time shareable code, URL and QR PNG.
type CreatedPot struct {
	PotView
	Code        string `json:"code"`
	ShareURL    string `json:"share_url"`
	QRPNGBase64 string `json:"qr_png_base64"`
}

// PublicContributorView is the privacy-preserving public entry for a paid
// contribution: initials and amount only.
type PublicContributorView struct {
	Initials      string                  `json:"initials"`
	AmountMinor   int64                   `json:"amount_minor"`
	RetainedMinor int64                   `json:"retained_minor"`
	Status        pots.ContributionStatus `json:"status"`
	PaidAt        *time.Time              `json:"paid_at,omitempty"`
}

// PublicPotView is the unauthenticated view of a Split-the-Meter pot.
// It never exposes full names, emails, phones, addresses, customer_name or tokens.
type PublicPotView struct {
	OrganiserDisplayName   string                  `json:"organiser_display_name"`
	MaskedMeterNumber      string                  `json:"masked_meter_number"`
	Disco                  string                  `json:"disco"`
	TargetMinor            int64                   `json:"target_minor"`
	ElectricityAmountMinor int64                   `json:"electricity_amount_minor"`
	FeeMinor               int64                   `json:"fee_minor"`
	FundedMinor            int64                   `json:"funded_minor"`
	RemainingMinor         int64                   `json:"remaining_minor"`
	PaidCount              int                     `json:"paid_count"`
	SuggestedShares        int                     `json:"suggested_shares,omitempty"`
	SuggestedShareMinor    int64                   `json:"suggested_share_minor,omitempty"`
	Currency               money.Currency          `json:"currency"`
	Note                   string                  `json:"note,omitempty"`
	Status                 pots.Status             `json:"status"`
	Deadline               time.Time               `json:"deadline"`
	Contributors           []PublicContributorView `json:"contributors"`
}

// ContributePotRequest initiates a contributor Paystack checkout against a pot.
type ContributePotRequest struct {
	Code              string
	AmountMinor       int64
	ContributorUserID string // optional; set when authenticated
	Email             string // sensitive; never logged or stored in plaintext
	Name              string // optional; never logged or stored in plaintext
	Phone             string // optional E.164 when OptInSMS is true; encrypted at rest
	OptInSMS          bool
	IdempotencyKey    string
}

// ContributePotResult returns the created contribution and Paystack checkout URL.
type ContributePotResult struct {
	ContributionID uuid.UUID               `json:"contribution_id"`
	PotCode        string                  `json:"pot_code"`
	AmountMinor    int64                   `json:"amount_minor"`
	Currency       money.Currency          `json:"currency"`
	Status         pots.ContributionStatus `json:"status"`
	CheckoutURL    string                  `json:"checkout_url"`
	ProviderRef    string                  `json:"provider_ref,omitempty"`
}

func (o *Orchestrator) potMaxHold() time.Duration {
	if o.cfg.PotMaxHold <= 0 {
		return pots.DefaultMaxHold
	}
	if o.cfg.PotMaxHold > pots.HardMaxHold {
		return pots.HardMaxHold
	}
	return o.cfg.PotMaxHold
}

// CreatePot creates a Split-the-Meter Funding Pot for a meter on which the
// organiser holds PURCHASE permission.
func (o *Orchestrator) CreatePot(ctx context.Context, p CreatePotParams) (*CreatedPot, error) {
	if p.MeterID == uuid.Nil {
		return nil, meters.ErrNotFound
	}
	note, err := payrequests.ValidateNote(p.Note)
	if err != nil {
		return nil, err
	}
	if err := pots.ValidateShares(p.SuggestedShares); err != nil {
		return nil, err
	}
	elecMinor := p.TargetMinor - o.cfg.FeeMinor
	if elecMinor < o.cfg.MinAmountMinor || elecMinor > o.cfg.MaxAmountMinor {
		return nil, fmt.Errorf("%w: target must cover [%d, %d] minor units of electricity plus %d fee",
			ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor, o.cfg.FeeMinor)
	}
	now := o.Now().UTC()
	deadline := p.Deadline.UTC()
	maxHold := o.potMaxHold()
	if !deadline.After(now) || deadline.Sub(now) > maxHold {
		return nil, pots.ErrInvalidDeadline
	}

	code, err := pots.GenerateCode()
	if err != nil {
		return nil, err
	}
	codeHMAC, _ := pots.CodeHMAC(o.box, code)
	potID := uuid.New()
	noteCT, noteKV, err := pots.SealPotNote(o.box, potID, note)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(strings.TrimSpace(p.PublicBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://meterly.ng"
	}
	shareURL := baseURL + "/pot/" + code
	qrB64, err := payrequests.EncodeQRPNGBase64(shareURL)
	if err != nil {
		return nil, err
	}

	pot := &pots.Pot{
		ID:              potID,
		Kind:            pots.KindSplitMeter,
		OwnerID:         p.OwnerID,
		MeterID:         p.MeterID,
		TargetMinor:     p.TargetMinor,
		FeeMinor:        o.cfg.FeeMinor,
		Currency:        money.NGN,
		SuggestedShares: p.SuggestedShares,
		NoteCiphertext:  noteCT,
		NoteKeyVersion:  noteKV,
		Status:          pots.StatusOpen,
		CodeHMAC:        codeHMAC,
		Deadline:        deadline,
		NextCheckAt:     deadline,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	var m *meters.Meter
	err = o.st.InTx(ctx, func(r store.Repos) error {
		var authErr error
		m, authErr = registry.AuthorizeForUpdateIn(ctx, r, p.OwnerID, p.MeterID, meters.PermPurchase)
		if authErr != nil {
			return authErr
		}
		if err := r.Pots().Create(ctx, pot); err != nil {
			return err
		}
		actor := audit.Actor{UserID: p.OwnerID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("pot.created", resourcePot, pot.ID, map[string]any{
			"meter_id":     pot.MeterID,
			"target_minor": pot.TargetMinor,
			"deadline":     pot.Deadline,
		}))
	})
	if err != nil {
		return nil, err
	}

	return &CreatedPot{
		PotView:     o.buildPotView(pot, m, note, pots.Summary{}, now),
		Code:        code,
		ShareURL:    shareURL,
		QRPNGBase64: qrB64,
	}, nil
}

// GetPotForOwner returns the full organiser view of a Split-the-Meter Funding Pot.
func (o *Orchestrator) GetPotForOwner(ctx context.Context, ownerID string, potID uuid.UUID) (*PotDetailView, error) {
	pot, err := o.st.Repos().Pots().Get(ctx, potID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrPotNotFound
	}
	if err != nil {
		return nil, err
	}
	if pot.Kind != pots.KindSplitMeter || pot.OwnerID != ownerID {
		return nil, ErrPotNotFound
	}
	return o.loadPotDetailView(ctx, pot)
}

// CancelPot cancels an OPEN Split-the-Meter pot on the organiser's request and initiates
// automatic refunds for all paid contributions. Refused once funded/allocating/settled.
func (o *Orchestrator) CancelPot(ctx context.Context, ownerID string, potID uuid.UUID) (*PotDetailView, error) {
	now := o.Now().UTC()
	var needSweep bool
	err := o.st.InTx(ctx, func(r store.Repos) error {
		pot, err := r.Pots().GetForUpdate(ctx, potID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrPotNotFound
		}
		if err != nil {
			return err
		}
		if pot.Kind != pots.KindSplitMeter || pot.OwnerID != ownerID {
			return ErrPotNotFound
		}
		switch pot.Status {
		case pots.StatusCancelled, pots.StatusRefunding, pots.StatusRefunded:
			needSweep = pot.Status != pots.StatusRefunded
			return nil
		case pots.StatusOpen:
		default:
			return ErrPotNotCancellable
		}

		contribs, err := r.Pots().ListContributionsForUpdate(ctx, pot.ID)
		if err != nil {
			return err
		}
		if err := pot.Transition(pots.StatusCancelled, now); err != nil {
			return err
		}
		if err := pot.Transition(pots.StatusRefunding, now); err != nil {
			return err
		}
		pot.NextCheckAt = now
		pot.CheckAttempts = 0

		hasPendingWork, err := o.queuePotContributorRefundsInTx(ctx, r, pot, contribs, "pot cancelled by organiser", now)
		if err != nil {
			return err
		}
		if !hasPendingWork {
			if err := pot.Transition(pots.StatusRefunded, now); err != nil {
				return err
			}
		} else {
			needSweep = true
		}
		if err := r.Pots().Update(ctx, pot); err != nil {
			return err
		}
		actor := audit.Actor{UserID: ownerID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		return r.Audit().Add(ctx, actor.Event("pot.cancelled", resourcePot, pot.ID, map[string]any{
			"meter_id": pot.MeterID,
		}))
	})
	if err != nil {
		return nil, err
	}
	if needSweep {
		_, _ = o.AdvancePot(ctx, potID)
	}
	return o.GetPotForOwner(ctx, ownerID, potID)
}

// GetPublicPot returns the privacy-preserving public view of a Funding Pot.
func (o *Orchestrator) GetPublicPot(ctx context.Context, rawCode string) (*PublicPotView, error) {
	codeHMAC, ok := pots.CodeHMAC(o.box, rawCode)
	if !ok {
		return nil, ErrPotNotFound
	}
	pot, err := o.st.Repos().Pots().GetByCodeHMAC(ctx, codeHMAC)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrPotNotFound
	}
	if err != nil {
		return nil, err
	}
	if pot.Kind != pots.KindSplitMeter {
		return nil, ErrPotNotFound
	}
	m, err := o.st.Repos().Meters().Get(ctx, pot.MeterID)
	if err != nil {
		return nil, err
	}
	note, err := pots.OpenPotNote(o.box, pot.ID, pot.NoteCiphertext)
	if err != nil {
		return nil, err
	}
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, pot.ID)
	if err != nil {
		return nil, err
	}
	sum := pots.SummarizeContributions(contribs)

	organiserName := "Meterly customer"
	if c, err := o.st.Repos().Users().Contacts(ctx, pot.OwnerID); err == nil && strings.TrimSpace(c.DisplayName) != "" {
		organiserName = c.DisplayName
	}

	now := o.Now().UTC()
	effStatus := pot.Status
	if effStatus == pots.StatusOpen && !now.Before(pot.Deadline) {
		effStatus = pots.StatusExpired
	}
	remaining := pot.TargetMinor - sum.FundedMinor
	if remaining < 0 {
		remaining = 0
	}

	pubContribs := make([]PublicContributorView, 0, len(contribs))
	for _, c := range contribs {
		if c.Status == pots.ContribPending || c.Status == pots.ContribFailed {
			continue
		}
		init := c.Initials
		if strings.TrimSpace(init) == "" {
			init = "A"
		}
		pubContribs = append(pubContribs, PublicContributorView{
			Initials:      init,
			AmountMinor:   c.AmountMinor,
			RetainedMinor: c.RetainedMinor,
			Status:        c.Status,
			PaidAt:        c.PaidAt,
		})
	}

	return &PublicPotView{
		OrganiserDisplayName:   organiserName,
		MaskedMeterNumber:      m.Masked,
		Disco:                  m.DiscoCode,
		TargetMinor:            pot.TargetMinor,
		ElectricityAmountMinor: pot.ElectricityAmountMinor(),
		FeeMinor:               pot.FeeMinor,
		FundedMinor:            sum.FundedMinor,
		RemainingMinor:         remaining,
		PaidCount:              sum.PaidCount,
		SuggestedShares:        pot.SuggestedShares,
		SuggestedShareMinor:    pot.SuggestedShareMinor(),
		Currency:               pot.Currency,
		Note:                   note,
		Status:                 effStatus,
		Deadline:               pot.Deadline,
		Contributors:           pubContribs,
	}, nil
}

// ContributeToPot creates an idempotent Paystack checkout for a pot contribution.
func (o *Orchestrator) ContributeToPot(ctx context.Context, req ContributePotRequest) (*ContributePotResult, bool, error) {
	codeHMAC, ok := pots.CodeHMAC(o.box, req.Code)
	if !ok {
		return nil, false, ErrPotNotFound
	}
	if req.AmountMinor <= 0 || req.AmountMinor > o.cfg.MaxAmountMinor+o.cfg.FeeMinor {
		return nil, false, ErrAmountOutOfRange
	}

	emailInput := strings.TrimSpace(req.Email)
	nameInput := strings.TrimSpace(req.Name)
	phoneInput := strings.TrimSpace(req.Phone)

	// Authenticated contributors may omit email/name/phone when already on file.
	if req.ContributorUserID != "" {
		if c, err := o.st.Repos().Users().Contacts(ctx, req.ContributorUserID); err == nil {
			if emailInput == "" && len(c.EmailCiphertext) > 0 {
				if dec, dErr := notifications.OpenUserEmail(o.box, req.ContributorUserID, c.EmailCiphertext); dErr == nil {
					emailInput = dec
				}
			}
			if nameInput == "" && strings.TrimSpace(c.DisplayName) != "" {
				nameInput = c.DisplayName
			}
			if req.OptInSMS && phoneInput == "" && len(c.PhoneCiphertext) > 0 {
				if dec, dErr := notifications.OpenUserPhone(o.box, req.ContributorUserID, c.PhoneCiphertext); dErr == nil {
					phoneInput = dec
				}
			}
		}
	}

	normEmail, err := notifications.NormalizeEmail(emailInput)
	if err != nil {
		return nil, false, err
	}
	contribName, err := payrequests.ValidatePayerName(nameInput)
	if err != nil {
		return nil, false, err
	}
	var normPhone string
	if req.OptInSMS {
		normPhone, err = notifications.NormalizePhone(phoneInput)
		if err != nil {
			return nil, false, err
		}
	}

	idemScope := "pot:" + codeHMAC
	reqHash := o.box.Hash(fmt.Sprintf("pot-contribute:%s:%s:%d:%s:%s:%t:%s",
		codeHMAC, req.ContributorUserID, req.AmountMinor, normEmail, contribName, req.OptInSMS, normPhone))

	now := o.Now().UTC()
	var (
		contrib       *pots.Contribution
		replayed      bool
		lostAccessPot uuid.UUID
	)

	err = o.st.InTx(ctx, func(r store.Repos) error {
		if req.IdempotencyKey != "" {
			existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
				Scope:       idemScope,
				Operation:   opPotContribute,
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
				cid, err := uuid.Parse(existing.ResultRef)
				if err != nil {
					return err
				}
				contrib, err = r.Pots().GetContribution(ctx, cid)
				if err != nil {
					return err
				}
				replayed = true
				return nil
			}
		}

		pot, err := r.Pots().GetByCodeHMACForUpdate(ctx, codeHMAC)
		if errors.Is(err, store.ErrNotFound) {
			return ErrPotNotFound
		}
		if err != nil {
			return err
		}
		if pot.Kind != pots.KindSplitMeter {
			return ErrPotNotFound
		}

		switch pot.Status {
		case pots.StatusCancelled:
			return ErrPotCancelled
		case pots.StatusExpired:
			return ErrPotExpired
		case pots.StatusFunded, pots.StatusAllocating, pots.StatusSettled:
			return ErrPotAlreadyFunded
		case pots.StatusRefunding, pots.StatusRefunded:
			return ErrPotClosed
		case pots.StatusOpen:
			if !now.Before(pot.Deadline) {
				return ErrPotExpired
			}
		}

		// Re-verify under meter lock that the organiser still holds PURCHASE.
		if _, err := registry.AuthorizeForUpdateIn(ctx, r, pot.OwnerID, pot.MeterID, meters.PermPurchase); err != nil {
			if errors.Is(err, meters.ErrForbidden) || errors.Is(err, meters.ErrNotFound) {
				lostAccessPot = pot.ID
				return ErrPotUnusable
			}
			return err
		}

		cid := uuid.New()
		emailCT, emailKV, err := pots.SealContributorEmail(o.box, cid, normEmail)
		if err != nil {
			return err
		}
		nameCT, nameKV, err := pots.SealContributorName(o.box, cid, contribName)
		if err != nil {
			return err
		}
		var (
			phoneCT []byte
			phoneKV string
		)
		if req.OptInSMS && normPhone != "" {
			phoneCT, phoneKV, err = pots.SealContributorPhone(o.box, cid, normPhone)
			if err != nil {
				return err
			}
		}

		contrib = &pots.Contribution{
			ID:               cid,
			PotID:            pot.ID,
			PayerUserID:      req.ContributorUserID,
			EmailCiphertext:  emailCT,
			EmailKeyVersion:  emailKV,
			NameCiphertext:   nameCT,
			NameKeyVersion:   nameKV,
			Initials:         pots.FormatInitials(contribName),
			PhoneCiphertext:  phoneCT,
			PhoneKeyVersion:  phoneKV,
			OptInSMS:         req.OptInSMS,
			AmountMinor:      req.AmountMinor,
			Currency:         pot.Currency,
			Status:           pots.ContribPending,
			RefundClaimUntil: time.Unix(0, 0).UTC(),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := r.Pots().CreateContribution(ctx, contrib); err != nil {
			return err
		}

		// Ensure sweeper checks stale pending contributions even before pot deadline.
		expiryCheck := now.Add(o.cfg.PaymentExpiry)
		if expiryCheck.Before(pot.NextCheckAt) {
			pot.NextCheckAt = expiryCheck
			pot.UpdatedAt = now
			if err := r.Pots().Update(ctx, pot); err != nil {
				return err
			}
		}

		if req.IdempotencyKey != "" {
			if err := r.Idempotency().Complete(ctx, idemScope, opPotContribute, req.IdempotencyKey, contrib.ID.String()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrPotUnusable) && lostAccessPot != uuid.Nil {
			o.cancelPotForLostAccess(ctx, lostAccessPot)
			_, _ = o.AdvancePot(ctx, lostAccessPot)
		}
		return nil, false, err
	}

	if contrib.Status == pots.ContribPending && contrib.CheckoutURL == "" {
		contrib, err = o.EnsureContributionPaymentInitialised(ctx, contrib.ID, normEmail)
		if err != nil {
			return nil, replayed, err
		}
	}
	return &ContributePotResult{
		ContributionID: contrib.ID,
		PotCode:        strings.TrimSpace(req.Code),
		AmountMinor:    contrib.AmountMinor,
		Currency:       contrib.Currency,
		Status:         contrib.Status,
		CheckoutURL:    contrib.CheckoutURL,
		ProviderRef:    contrib.PaymentProviderRef,
	}, replayed, nil
}

// EnsureContributionPaymentInitialised creates the Paystack checkout for a
// PENDING contribution if not already initialised.
func (o *Orchestrator) EnsureContributionPaymentInitialised(ctx context.Context, contribID uuid.UUID, email string) (*pots.Contribution, error) {
	c, err := o.st.Repos().Pots().GetContribution(ctx, contribID)
	if err != nil || c.Status != pots.ContribPending || c.CheckoutURL != "" {
		return c, err
	}
	if email == "" {
		email, err = pots.OpenContributorEmail(o.box, c.ID, c.EmailCiphertext)
		if err != nil {
			return c, err
		}
	}
	if err := o.requireEnabled(ctx, o.payments.Name()); err != nil {
		return c, err
	}
	pctx, cancel := o.pctx(ctx)
	res, err := o.payments.InitializePayment(pctx, providers.PaymentInit{
		Reference: c.ID.String(),
		Amount:    money.New(c.AmountMinor, c.Currency),
		Email:     email,
	})
	cancel()
	if err != nil {
		return c, fmt.Errorf("initialise contribution payment: %w", err)
	}
	now := o.Now().UTC()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Pots().GetContributionForUpdate(ctx, contribID)
		if err != nil {
			return err
		}
		if cur.Status != pots.ContribPending || cur.CheckoutURL != "" {
			c = cur
			return nil
		}
		cur.PaymentProviderName = o.payments.Name()
		cur.PaymentProviderRef = res.ProviderRef
		cur.CheckoutURL = res.CheckoutURL
		cur.UpdatedAt = now
		c = cur
		return r.Pots().UpdateContribution(ctx, cur)
	})
	return c, err
}

func (o *Orchestrator) cancelPotForLostAccess(ctx context.Context, potID uuid.UUID) {
	now := o.Now().UTC()
	_ = o.st.InTx(ctx, func(r store.Repos) error {
		pot, err := r.Pots().GetForUpdate(ctx, potID)
		if err != nil || pot.Status != pots.StatusOpen {
			return err
		}
		contribs, err := r.Pots().ListContributionsForUpdate(ctx, pot.ID)
		if err != nil {
			return err
		}
		if err := pot.Transition(pots.StatusCancelled, now); err != nil {
			return err
		}
		if err := pot.Transition(pots.StatusRefunding, now); err != nil {
			return err
		}
		pot.NextCheckAt = now
		pot.CheckAttempts = 0
		hasPending, err := o.queuePotContributorRefundsInTx(ctx, r, pot, contribs, "organiser lost meter PURCHASE access", now)
		if err != nil {
			return err
		}
		if !hasPending {
			if err := pot.Transition(pots.StatusRefunded, now); err != nil {
				return err
			}
		}
		if err := r.Pots().Update(ctx, pot); err != nil {
			return err
		}
		actor := audit.Actor{Type: audit.ActorSystem, CorrelationID: audit.CorrelationFrom(ctx), Reason: "organiser lost meter PURCHASE access"}
		return r.Audit().Add(ctx, actor.Event("pot.cancelled", resourcePot, pot.ID, map[string]any{"meter_id": pot.MeterID}))
	})
}

// handlePotContributionPayment handles Paystack webhooks and pings whose
// provider reference belongs to a pot contribution.
func (o *Orchestrator) handlePotContributionPayment(ctx context.Context, providerRef string) (*tx.Transaction, bool, error) {
	c, err := o.st.Repos().Pots().ContributionByPaymentRef(ctx, providerRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if c.Status == pots.ContribPending || c.Status == pots.ContribFailed {
		if err := o.verifyContributionPayment(ctx, c); err != nil {
			return nil, true, err
		}
	}
	_, advErr := o.AdvancePot(ctx, c.PotID)
	allocs, err := o.st.Repos().Pots().ListAllocations(ctx, c.PotID)
	if err == nil && len(allocs) > 0 {
		if t, tErr := o.Get(ctx, allocs[len(allocs)-1].TransactionID); tErr == nil {
			return t, true, advErr
		}
	}
	return nil, true, advErr
}

func (o *Orchestrator) verifyContributionPayment(ctx context.Context, c *pots.Contribution) error {
	if c.PaymentProviderRef == "" {
		now := o.Now().UTC()
		if now.Sub(c.CreatedAt) < o.cfg.PaymentExpiry {
			return nil
		}
		return o.st.InTx(ctx, func(r store.Repos) error {
			cur, err := r.Pots().GetContributionForUpdate(ctx, c.ID)
			if err != nil || cur.Status != pots.ContribPending || cur.PaymentProviderRef != "" {
				return err
			}
			if err := cur.Transition(pots.ContribFailed, now); err != nil {
				return err
			}
			return r.Pots().UpdateContribution(ctx, cur)
		})
	}

	pctx, cancel := o.pctx(ctx)
	v, err := o.payments.VerifyPayment(pctx, c.PaymentProviderRef)
	cancel()
	if err != nil {
		return fmt.Errorf("verify contribution payment: %w", err)
	}

	now := o.Now().UTC()
	var mismatch bool
	err = o.st.InTx(ctx, func(r store.Repos) error {
		pot, err := r.Pots().GetForUpdate(ctx, c.PotID)
		if err != nil {
			return err
		}
		contribs, err := r.Pots().ListContributionsForUpdate(ctx, pot.ID)
		if err != nil {
			return err
		}
		var cur *pots.Contribution
		for _, item := range contribs {
			if item.ID == c.ID {
				cur = item
				break
			}
		}
		if cur == nil {
			return store.ErrNotFound
		}

		if cur.Status == pots.ContribFailed {
			if v.Status != providers.PaymentSucceeded {
				return nil
			}
			// Late payment arrived after expiry/abandonment: accept into POT_FUNDS_HELD
			// and immediately queue a 100% refund back to the contributor.
			return o.recordPaidContributionInTx(ctx, r, pot, contribs, cur, v, now, &mismatch)
		}
		if cur.Status != pots.ContribPending {
			return nil
		}

		switch v.Status {
		case providers.PaymentPending:
			if now.Sub(cur.CreatedAt) < o.cfg.PaymentExpiry && pot.Status == pots.StatusOpen && now.Before(pot.Deadline) {
				return nil
			}
			if err := cur.Transition(pots.ContribFailed, now); err != nil {
				return err
			}
			return r.Pots().UpdateContribution(ctx, cur)
		case providers.PaymentFailed:
			if err := cur.Transition(pots.ContribFailed, now); err != nil {
				return err
			}
			return r.Pots().UpdateContribution(ctx, cur)
		case providers.PaymentSucceeded:
			return o.recordPaidContributionInTx(ctx, r, pot, contribs, cur, v, now, &mismatch)
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	if mismatch {
		return ErrPaymentMismatch
	}
	return nil
}

func (o *Orchestrator) recordPaidContributionInTx(
	ctx context.Context,
	r store.Repos,
	pot *pots.Pot,
	contribs []*pots.Contribution,
	cur *pots.Contribution,
	v providers.PaymentVerification,
	now time.Time,
	mismatch *bool,
) error {
	expected := money.New(cur.AmountMinor, cur.Currency)
	// Always record actual money received at PSP in the double-entry ledger.
	if err := o.post(ctx, r, "pot-contrib:"+cur.ID.String(), cur.ID, "Pot contribution received",
		ledger.Entry{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: v.Amount},
		ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Credit, Amount: v.Amount},
	); err != nil {
		return err
	}
	cur.PaidAt = &now

	if v.Amount != expected {
		*mismatch = true
		pot.FlagForReview(fmt.Sprintf("contribution %s amount mismatch: expected %d %s, got %d %s",
			cur.ID, expected.Minor, expected.Currency, v.Amount.Minor, v.Amount.Currency), now)
		cur.AmountMinor = v.Amount.Minor
		cur.RetainedMinor = v.Amount.Minor
		if err := cur.Transition(pots.ContribPaid, now); err != nil {
			return err
		}
		if err := r.Pots().UpdateContribution(ctx, cur); err != nil {
			return err
		}
		return r.Pots().Update(ctx, pot)
	}

	// If the pot was OPEN but its deadline already passed, expire it first so
	// both prior contributions and this late contribution are refunded.
	if pot.Status == pots.StatusOpen && !now.Before(pot.Deadline) {
		if err := pot.Transition(pots.StatusExpired, now); err != nil {
			return err
		}
		if err := pot.Transition(pots.StatusRefunding, now); err != nil {
			return err
		}
		pot.NextCheckAt = now
		pot.CheckAttempts = 0
		if _, err := o.queuePotContributorRefundsInTx(ctx, r, pot, contribs, "pot expired before reaching target", now); err != nil {
			return err
		}
	}

	// Late payment on an expired, cancelled, refunding, refunded or already-funded pot:
	// retain 0 and queue an immediate 100% refund.
	if pot.Status != pots.StatusOpen {
		if pot.Status == pots.StatusRefunded {
			if err := pot.Transition(pots.StatusRefunding, now); err != nil {
				return err
			}
			pot.NextCheckAt = now
			pot.CheckAttempts = 0
		}
		cur.RetainedMinor = 0
		cur.RefundAmountMinor = cur.AmountMinor
		cur.RefundStep = 1
		if err := cur.Transition(pots.ContribRefundPending, now); err != nil {
			return err
		}
		if err := o.post(ctx, r, fmt.Sprintf("pot-refund-due:%s:late", cur.ID), cur.ID, "Late/excess pot contribution refund owed",
			ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: money.New(cur.AmountMinor, cur.Currency)},
			ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: money.New(cur.AmountMinor, cur.Currency)},
		); err != nil {
			return err
		}
		if err := r.Pots().UpdateContribution(ctx, cur); err != nil {
			return err
		}
		return r.Pots().Update(ctx, pot)
	}

	var retainedBefore int64
	for _, item := range contribs {
		if item.ID != cur.ID {
			retainedBefore += item.RetainedMinor
		}
	}
	needed := pot.TargetMinor - retainedBefore
	if needed < 0 {
		needed = 0
	}
	retain := cur.AmountMinor
	if retain > needed {
		retain = needed
	}
	excess := cur.AmountMinor - retain
	cur.RetainedMinor = retain

	if excess > 0 {
		cur.RefundAmountMinor = excess
		cur.RefundStep = 1
		if err := cur.Transition(pots.ContribRefundPending, now); err != nil {
			return err
		}
		if err := o.post(ctx, r, fmt.Sprintf("pot-refund-due:%s:excess", cur.ID), cur.ID, "Overfunded pot contribution excess refund owed",
			ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: money.New(excess, cur.Currency)},
			ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: money.New(excess, cur.Currency)},
		); err != nil {
			return err
		}
	} else {
		if err := cur.Transition(pots.ContribPaid, now); err != nil {
			return err
		}
	}
	if err := r.Pots().UpdateContribution(ctx, cur); err != nil {
		return err
	}

	if retainedBefore+retain >= pot.TargetMinor {
		if err := pot.Transition(pots.StatusFunded, now); err != nil {
			return err
		}
		pot.NextCheckAt = now
		pot.CheckAttempts = 0
	} else {
		pot.UpdatedAt = now
	}
	return r.Pots().Update(ctx, pot)
}

// queuePotContributorRefundsInTx moves every retained contribution on a pot into
// REFUND_PENDING and posts POT_FUNDS_HELD -> REFUND_PAYABLE. Returns true if any
// contribution still requires PSP verification or refund execution.
func (o *Orchestrator) queuePotContributorRefundsInTx(
	ctx context.Context,
	r store.Repos,
	pot *pots.Pot,
	contribs []*pots.Contribution,
	reason string,
	now time.Time,
) (bool, error) {
	hasPendingWork := false
	for _, c := range contribs {
		if c.Status == pots.ContribPending {
			if c.PaymentProviderRef == "" {
				if err := c.Transition(pots.ContribFailed, now); err != nil {
					return false, err
				}
				if err := r.Pots().UpdateContribution(ctx, c); err != nil {
					return false, err
				}
			} else {
				// Must verify with Paystack outside InTx before marking the pot REFUNDED.
				hasPendingWork = true
			}
			continue
		}
		if c.RetainedMinor > 0 {
			toQueue := c.RetainedMinor
			c.RetainedMinor = 0
			c.RefundAmountMinor += toQueue
			if c.Status != pots.ContribRefundPending {
				c.RefundStep++
				if err := c.Transition(pots.ContribRefundPending, now); err != nil {
					return false, err
				}
			}
			if err := o.post(ctx, r, fmt.Sprintf("pot-refund-due:%s:principal", c.ID), c.ID, "Pot contributor refund owed: "+reason,
				ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: money.New(toQueue, c.Currency)},
				ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: money.New(toQueue, c.Currency)},
			); err != nil {
				return false, err
			}
			if err := r.Pots().UpdateContribution(ctx, c); err != nil {
				return false, err
			}
		}
		if c.Status == pots.ContribRefundPending && c.PendingRefundMinor() > 0 {
			hasPendingWork = true
		}
	}
	return hasPendingWork, nil
}

// AdvancePot drives a Funding Pot forward from its last committed state:
//   - expires overdue OPEN pots and queues contributor refunds
//   - allocates FUNDED pots into a POT-funded electricity transaction and vends
//   - reconciles ALLOCATING pots and settles them once delivery is confirmed
//   - verifies pending checkouts and executes due contributor refunds via PSP
func (o *Orchestrator) AdvancePot(ctx context.Context, potID uuid.UUID) (*pots.Pot, error) {
	pot, err := o.st.Repos().Pots().Get(ctx, potID)
	if err != nil {
		return nil, err
	}
	if pot.ReviewReason != "" || pot.Status == pots.StatusPendingApproval {
		return pot, nil
	}
	now := o.Now().UTC()

	// 1. Verify stale pending contributions on OPEN pots, or all pending checkouts on REFUNDING/EXPIRED/CANCELLED pots.
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, potID)
	if err != nil {
		return pot, err
	}
	for _, c := range contribs {
		if c.Status != pots.ContribPending {
			continue
		}
		shouldCheck := pot.Status != pots.StatusOpen || !now.Before(pot.Deadline) || now.Sub(c.CreatedAt) >= o.cfg.PaymentExpiry
		if shouldCheck {
			if vErr := o.verifyContributionPayment(ctx, c); vErr != nil {
				return pot, vErr
			}
		}
	}

	// Reload pot after any contribution verification updates.
	pot, err = o.st.Repos().Pots().Get(ctx, potID)
	if err != nil || pot.ReviewReason != "" {
		return pot, err
	}
	now = o.Now().UTC()

	// 2. Expire overdue OPEN pot or advance EXPIRED/CANCELLED -> REFUNDING.
	if (pot.Status == pots.StatusOpen && !now.Before(pot.Deadline)) ||
		pot.Status == pots.StatusExpired || pot.Status == pots.StatusCancelled {
		err = o.st.InTx(ctx, func(r store.Repos) error {
			cur, err := r.Pots().GetForUpdate(ctx, potID)
			if err != nil || cur.ReviewReason != "" {
				return err
			}
			cs, err := r.Pots().ListContributionsForUpdate(ctx, cur.ID)
			if err != nil {
				return err
			}
			switch cur.Status {
			case pots.StatusOpen:
				if now.Before(cur.Deadline) {
					return nil
				}
				if err := cur.Transition(pots.StatusExpired, now); err != nil {
					return err
				}
				if err := cur.Transition(pots.StatusRefunding, now); err != nil {
					return err
				}
				cur.NextCheckAt = now
				cur.CheckAttempts = 0
			case pots.StatusExpired, pots.StatusCancelled:
				if err := cur.Transition(pots.StatusRefunding, now); err != nil {
					return err
				}
				cur.NextCheckAt = now
			default:
				return nil
			}
			if _, err := o.queuePotContributorRefundsInTx(ctx, r, cur, cs, "pot expired/cancelled", now); err != nil {
				return err
			}
			return r.Pots().Update(ctx, cur)
		})
		if err != nil {
			return pot, err
		}
		pot, err = o.st.Repos().Pots().Get(ctx, potID)
		if err != nil {
			return nil, err
		}
	}

	// 3. Allocate FUNDED pot into POT-funded electricity transaction(s) and vend.
	if pot.Status == pots.StatusFunded {
		if pot.Kind == pots.KindLandlordBulk {
			advErr := o.allocateAndVendLandlordBulkPot(ctx, potID, now)
			if advErr != nil {
				_ = o.executePendingContributionRefunds(ctx, potID)
				p2, _ := o.st.Repos().Pots().Get(ctx, potID)
				return p2, advErr
			}
			pot, err = o.st.Repos().Pots().Get(ctx, potID)
			if err != nil {
				return nil, err
			}
		} else {
			var allocatedTxnID uuid.UUID
			err = o.st.InTx(ctx, func(r store.Repos) error {
				cur, err := r.Pots().GetForUpdate(ctx, potID)
				if err != nil || cur.Status != pots.StatusFunded || cur.ReviewReason != "" {
					return err
				}
				cs, err := r.Pots().ListContributionsForUpdate(ctx, cur.ID)
				if err != nil {
					return err
				}
				// Re-verify organiser still holds PURCHASE on the target meter.
				m, authErr := registry.AuthorizeForUpdateIn(ctx, r, cur.OwnerID, cur.MeterID, meters.PermPurchase)
				if authErr != nil {
					if errors.Is(authErr, meters.ErrForbidden) || errors.Is(authErr, meters.ErrNotFound) {
						if err := cur.Transition(pots.StatusRefunding, now); err != nil {
							return err
						}
						cur.NextCheckAt = now
						cur.CheckAttempts = 0
						if _, err := o.queuePotContributorRefundsInTx(ctx, r, cur, cs, "organiser lost meter PURCHASE access", now); err != nil {
							return err
						}
						if err := r.Pots().Update(ctx, cur); err != nil {
							return err
						}
						actor := audit.Actor{Type: audit.ActorSystem, CorrelationID: audit.CorrelationFrom(ctx), Reason: "organiser lost meter PURCHASE access"}
						return r.Audit().Add(ctx, actor.Event("pot.cancelled", resourcePot, cur.ID, map[string]any{"meter_id": cur.MeterID}))
					}
					return authErr
				}

				options := []tx.DeliveryMethod{tx.MethodToken}
				method := tx.MethodToken
				if o.remote != nil && m.Capabilities.RemoteCreditUsable() {
					options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
					if ok, eErr := r.Providers().Enabled(ctx, o.remote.Name()); eErr == nil && ok {
						method = tx.MethodRemote
					}
				}
				elecAmount := money.New(cur.ElectricityAmountMinor(), cur.Currency)
				feeAmount := money.New(cur.FeeMinor, cur.Currency)
				totalAmount := money.New(cur.TargetMinor, cur.Currency)
				q := &tx.Quote{
					ID:              uuid.New(),
					CustomerID:      cur.OwnerID,
					MeterID:         cur.MeterID,
					Amount:          elecAmount,
					Fee:             feeAmount,
					Total:           totalAmount,
					DeliveryMethod:  method,
					DeliveryOptions: options,
					ExpiresAt:       now.Add(o.cfg.QuoteTTL),
					Used:            true,
					CreatedAt:       now,
				}
				if err := r.Quotes().Create(ctx, q); err != nil {
					return err
				}
				potTxn := tx.NewPotFunded(cur.OwnerID, q, now)
				if err := r.Transactions().Create(ctx, potTxn); err != nil {
					return err
				}
				alloc := &pots.Allocation{
					ID:            uuid.New(),
					PotID:         cur.ID,
					TransactionID: potTxn.ID,
					MeterID:       cur.MeterID,
					AmountMinor:   cur.TargetMinor,
					Currency:      cur.Currency,
					Status:        pots.AllocActive,
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if err := r.Pots().CreateAllocation(ctx, alloc); err != nil {
					return err
				}
				if err := o.post(ctx, r, "pot-alloc:"+alloc.ID.String(), potTxn.ID, "Allocated pot funds to electricity purchase",
					ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Debit, Amount: totalAmount},
					ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: totalAmount},
				); err != nil {
					return err
				}
				if err := cur.Transition(pots.StatusAllocating, now); err != nil {
					return err
				}
				cur.NextCheckAt = now
				cur.CheckAttempts = 0
				if err := r.Pots().Update(ctx, cur); err != nil {
					return err
				}
				if err := emit(ctx, r, "electricity.transaction.created", potTxn.ID, map[string]any{
					"funding_source": string(tx.FundingPot),
					"pot_id":         cur.ID,
				}); err != nil {
					return err
				}
				if err := emit(ctx, r, "payment.confirmed", potTxn.ID, map[string]any{
					"funding_source": string(tx.FundingPot),
					"pot_id":         cur.ID,
				}); err != nil {
					return err
				}
				allocatedTxnID = potTxn.ID
				return nil
			})
			if err != nil {
				return pot, err
			}
			if allocatedTxnID != uuid.Nil {
				if _, advErr := o.advance(ctx, allocatedTxnID); advErr != nil {
					// Return error so ResumeDue tracks provider outages/failures accurately;
					// first run any overfund excess refunds before returning if possible.
					_ = o.executePendingContributionRefunds(ctx, potID)
					p2, _ := o.st.Repos().Pots().Get(ctx, potID)
					return p2, advErr
				}
			}
			pot, err = o.st.Repos().Pots().Get(ctx, potID)
			if err != nil {
				return nil, err
			}
		}
	}

	// 4. Execute any due contributor refunds (both overfund excesses and cancelled/expired/vend-failed principal).
	refundErr := o.executePendingContributionRefunds(ctx, potID)

	// 5. Reconcile ALLOCATING pot -> SETTLED (never refunding while VendUnknown) or REFUNDING pot -> REFUNDED.
	var resumeTxnErr error
	pot, err = o.st.Repos().Pots().Get(ctx, potID)
	if err != nil {
		return nil, err
	}
	if pot.Status == pots.StatusAllocating {
		allocs, aErr := o.st.Repos().Pots().ListAllocations(ctx, potID)
		if aErr != nil {
			return pot, aErr
		}
		for _, al := range allocs {
			if al.Status != pots.AllocActive {
				continue
			}
			t, tErr := o.Get(ctx, al.TransactionID)
			if tErr != nil {
				return pot, tErr
			}
			// Only reconcile stale allocated transactions so a concurrent webhook
			// never races the worker currently executing o.advance(allocatedTxnID).
			if !t.Settled() && !o.Now().UTC().Before(t.UpdatedAt.Add(o.cfg.ResumeStaleAfter)) {
				if _, rErr := o.Resume(ctx, t.ID); rErr != nil && resumeTxnErr == nil {
					resumeTxnErr = rErr
				}
			}
		}
	}

	now = o.Now().UTC()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Pots().GetForUpdate(ctx, potID)
		if err != nil || cur.ReviewReason != "" {
			return err
		}
		cs, err := r.Pots().ListContributionsForUpdate(ctx, cur.ID)
		if err != nil {
			return err
		}
		switch cur.Status {
		case pots.StatusRefunding:
			for _, c := range cs {
				if c.Status == pots.ContribPending || c.Status == pots.ContribRefundPending || c.RetainedMinor > 0 || c.PendingRefundMinor() > 0 {
					return nil
				}
			}
			if err := cur.Transition(pots.StatusRefunded, now); err != nil {
				return err
			}
			return r.Pots().Update(ctx, cur)

		case pots.StatusAllocating:
			return o.settleAllocatingPotInTx(ctx, r, cur, cs, now)
		}
		return nil
	})
	if err != nil {
		return pot, err
	}
	pot, err = o.st.Repos().Pots().Get(ctx, potID)
	if refundErr != nil {
		return pot, refundErr
	}
	return pot, resumeTxnErr
}

func (o *Orchestrator) trySettleAllocatedPot(ctx context.Context, txnID uuid.UUID) {
	alloc, err := o.st.Repos().Pots().GetAllocationByTxn(ctx, txnID)
	if err != nil {
		return
	}
	now := o.Now().UTC()
	_ = o.st.InTx(ctx, func(r store.Repos) error {
		cur, err := r.Pots().GetForUpdate(ctx, alloc.PotID)
		if err != nil || cur.Status != pots.StatusAllocating || cur.ReviewReason != "" {
			return err
		}
		cs, err := r.Pots().ListContributionsForUpdate(ctx, cur.ID)
		if err != nil {
			return err
		}
		return o.settleAllocatingPotInTx(ctx, r, cur, cs, now)
	})
}

func (o *Orchestrator) settleAllocatingPotInTx(
	ctx context.Context,
	r store.Repos,
	cur *pots.Pot,
	cs []*pots.Contribution,
	now time.Time,
) error {
	for _, c := range cs {
		if c.Status == pots.ContribRefundPending || c.PendingRefundMinor() > 0 {
			return nil
		}
	}
	allocs, err := r.Pots().ListAllocations(ctx, cur.ID)
	if err != nil || len(allocs) == 0 {
		return err
	}
	delivered := 0
	returned := 0
	for _, al := range allocs {
		if al.Status == pots.AllocReturned {
			if cur.Kind != pots.KindLandlordBulk {
				return nil
			}
			returned++
			continue
		}
		if al.Status != pots.AllocActive {
			return nil
		}
		t, err := r.Transactions().Get(ctx, al.TransactionID)
		if err != nil {
			return err
		}
		if t.Delivery != tx.DelConfirmed {
			return nil
		}
		delivered++
	}
	if delivered > 0 {
		if err := cur.Transition(pots.StatusSettled, now); err != nil {
			return err
		}
		return r.Pots().Update(ctx, cur)
	}
	if cur.Kind == pots.KindLandlordBulk && returned == len(allocs) {
		if err := cur.Transition(pots.StatusRefunding, now); err != nil {
			return err
		}
		if err := cur.Transition(pots.StatusRefunded, now); err != nil {
			return err
		}
		return r.Pots().Update(ctx, cur)
	}
	return nil
}

func (o *Orchestrator) executePendingContributionRefunds(ctx context.Context, potID uuid.UUID) error {
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, potID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range contribs {
		if item.Status != pots.ContribRefundPending || item.PendingRefundMinor() <= 0 {
			continue
		}
		if err := o.requireEnabled(ctx, o.payments.Name()); err != nil {
			return err
		}

		var (
			claimed     bool
			claimAmount int64
			claimStep   int
			claimRef    string
			provRef     string
			currency    money.Currency
		)
		claimNow := o.Now().UTC()
		err := o.st.InTx(ctx, func(r store.Repos) error {
			_, err := r.Pots().GetForUpdate(ctx, potID)
			if err != nil {
				return err
			}
			cur, err := r.Pots().GetContributionForUpdate(ctx, item.ID)
			if err != nil {
				return err
			}
			if cur.Status != pots.ContribRefundPending || cur.PendingRefundMinor() <= 0 {
				return nil
			}
			if claimNow.Before(cur.RefundClaimUntil) {
				return nil
			}
			if cur.RefundStep <= 0 {
				cur.RefundStep = 1
			}
			cur.RefundClaimUntil = claimNow.Add(o.cfg.ResumeLease)
			cur.UpdatedAt = claimNow
			if err := r.Pots().UpdateContribution(ctx, cur); err != nil {
				return err
			}
			claimed = true
			claimAmount = cur.PendingRefundMinor()
			claimStep = cur.RefundStep
			claimRef = cur.RefundReference()
			provRef = cur.PaymentProviderRef
			currency = cur.Currency
			return nil
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !claimed {
			continue
		}

		pctx, cancel := o.pctx(ctx)
		refundProvRef, rErr := o.payments.RefundPayment(pctx, providers.RefundRequest{
			ProviderRef: provRef,
			Amount:      money.New(claimAmount, currency),
			Reference:   claimRef,
		})
		cancel()
		if rErr != nil {
			o.log.Warn("pot contribution refund failed; stays REFUND_PENDING",
				"pot_id", potID, "contribution_id", item.ID, "err", rErr)
			if firstErr == nil {
				firstErr = fmt.Errorf("refund contribution %s: %w", item.ID, rErr)
			}
			// Release claim lease so subsequent backoff sweeps can retry cleanly.
			_ = o.st.InTx(ctx, func(r store.Repos) error {
				cur, gErr := r.Pots().GetContributionForUpdate(ctx, item.ID)
				if gErr != nil || cur.Status != pots.ContribRefundPending {
					return gErr
				}
				cur.RefundClaimUntil = time.Unix(0, 0).UTC()
				return r.Pots().UpdateContribution(ctx, cur)
			})
			continue
		}

		doneNow := o.Now().UTC()
		err = o.st.InTx(ctx, func(r store.Repos) error {
			_, err := r.Pots().GetForUpdate(ctx, potID)
			if err != nil {
				return err
			}
			cur, err := r.Pots().GetContributionForUpdate(ctx, item.ID)
			if err != nil {
				return err
			}
			if cur.Status != pots.ContribRefundPending || cur.RefundStep != claimStep {
				return nil
			}
			cur.RefundedMinor += claimAmount
			cur.RefundProviderRef = refundProvRef
			cur.RefundClaimUntil = time.Unix(0, 0).UTC()
			if err := o.post(ctx, r, fmt.Sprintf("pot-refund-paid:%s:%d", cur.ID, claimStep), cur.ID, "Pot contribution refund paid",
				ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Debit, Amount: money.New(claimAmount, currency)},
				ledger.Entry{Account: ledger.PSPSettlementReceivable, Direction: ledger.Credit, Amount: money.New(claimAmount, currency)},
			); err != nil {
				return err
			}
			switch {
			case cur.PendingRefundMinor() > 0:
				// Additional principal was queued while an earlier partial excess refund was in flight.
				cur.RefundStep++
				cur.UpdatedAt = doneNow
			case cur.RetainedMinor > 0:
				if err := cur.Transition(pots.ContribPaid, doneNow); err != nil {
					return err
				}
			default:
				if err := cur.Transition(pots.ContribRefunded, doneNow); err != nil {
					return err
				}
			}
			return r.Pots().UpdateContribution(ctx, cur)
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// refundPotFunded returns a POT-funded transaction's funds from
// CUSTOMER_FUNDS_CLEARING back to POT_FUNDS_HELD (never calling the PSP directly),
// marks its allocation RETURNED, moves the parent pot to REFUNDING and drives
// individual contributor PSP refunds.
func (o *Orchestrator) refundPotFunded(ctx context.Context, txnID uuid.UUID, reason string, actor *audit.Actor) (*tx.Transaction, error) {
	alloc, err := o.st.Repos().Pots().GetAllocationByTxn(ctx, txnID)
	if err != nil {
		return nil, err
	}
	now := o.Now().UTC()
	var (
		t       *tx.Transaction
		claimed bool
	)
	err = o.st.InTx(ctx, func(r store.Repos) error {
		pot, err := r.Pots().GetForUpdate(ctx, alloc.PotID)
		if err != nil {
			return err
		}
		contribs, err := r.Pots().ListContributionsForUpdate(ctx, pot.ID)
		if err != nil {
			return err
		}
		al, err := r.Pots().GetAllocationByTxnForUpdate(ctx, txnID)
		if err != nil {
			return err
		}
		t, err = r.Transactions().GetForUpdate(ctx, txnID)
		if err != nil {
			return err
		}
		if t.EffectiveFundingSource() != tx.FundingPot {
			return errSkip
		}
		if actor != nil {
			if t.ReviewReason == "" {
				return ErrNotInReview
			}
			if t.Vending != tx.VendNotStarted && t.Vending != tx.VendFailed {
				return ErrRefundNotSafe
			}
		} else {
			if t.Vending != tx.VendFailed || t.ReviewReason != "" {
				return errSkip
			}
		}
		if t.Payment == tx.PayRefunded {
			return nil
		}
		if t.Payment != tx.PayConfirmed && t.Payment != tx.PayRefundPending {
			return errSkip
		}
		if reason == "" {
			reason = "pot allocation returned"
		}
		t.RefundReason = reason
		t.RefundAmount = t.Total
		if t.Payment == tx.PayConfirmed {
			if err := t.SetPayment(tx.PayRefundPending, reason); err != nil {
				return err
			}
		}
		t.RefundProviderRef = "pot-return:" + pot.ID.String()
		if err := t.SetPayment(tx.PayRefunded, "funds returned to pot "+pot.ID.String()); err != nil {
			return err
		}
		if actor != nil {
			prev := t.ReviewReason
			t.ReviewReason = ""
			if err := r.Audit().Add(ctx, actor.Event("transaction.refund", resourceTxn, t.ID, map[string]any{
				"amount_minor":           t.Total.Minor,
				"funding_source":         string(tx.FundingPot),
				"pot_id":                 pot.ID,
				"previous_review_reason": prev,
			})); err != nil {
				return err
			}
		}
		if err := r.Transactions().Save(ctx, t); err != nil {
			return err
		}
		if err := o.post(ctx, r, "refund-pot:"+t.ID.String(), t.ID, "Returned POT-funded purchase to pot: "+reason,
			ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
			ledger.Entry{Account: ledger.PotFundsHeld, Direction: ledger.Credit, Amount: t.Total},
		); err != nil {
			return err
		}
		al.MarkReturned(now)
		if err := r.Pots().UpdateAllocation(ctx, al); err != nil {
			return err
		}

		// Check if all allocations on the pot have been returned; for LANDLORD_BULK,
		// queue an immediate partial contributor refund for this failed unit even
		// while sibling units succeed or remain UNKNOWN.
		allAllocs, err := r.Pots().ListAllocations(ctx, pot.ID)
		if err != nil {
			return err
		}
		allReturned := true
		for _, item := range allAllocs {
			if item.ID == al.ID {
				continue
			}
			if item.Status != pots.AllocReturned {
				allReturned = false
				break
			}
		}
		if pot.Kind == pots.KindLandlordBulk {
			hasPending, err := o.queuePotPartialRefundInTx(
				ctx, r, contribs, t.Total.Minor,
				"pot-item-refund-due:"+al.ID.String(),
				al.ID,
				"Bulk top-up failed unit refund owed: "+reason,
				now,
			)
			if err != nil {
				return err
			}
			if allReturned && pot.Status == pots.StatusAllocating {
				if err := pot.Transition(pots.StatusRefunding, now); err != nil {
					return err
				}
				pot.NextCheckAt = now
				pot.CheckAttempts = 0
				if !hasPending {
					if err := pot.Transition(pots.StatusRefunded, now); err != nil {
						return err
					}
				}
				if err := r.Pots().Update(ctx, pot); err != nil {
					return err
				}
			} else if pot.Status == pots.StatusAllocating {
				pot.NextCheckAt = now
				pot.CheckAttempts = 0
				if err := r.Pots().Update(ctx, pot); err != nil {
					return err
				}
			}
		} else if allReturned && pot.Status == pots.StatusAllocating {
			if err := pot.Transition(pots.StatusRefunding, now); err != nil {
				return err
			}
			pot.NextCheckAt = now
			pot.CheckAttempts = 0
			hasPending, err := o.queuePotContributorRefundsInTx(ctx, r, pot, contribs, reason, now)
			if err != nil {
				return err
			}
			if !hasPending {
				if err := pot.Transition(pots.StatusRefunded, now); err != nil {
					return err
				}
			}
			if err := r.Pots().Update(ctx, pot); err != nil {
				return err
			}
		}
		claimed = true
		return emit(ctx, r, "electricity.refund.completed", t.ID, map[string]any{
			"funding_source": string(tx.FundingPot),
			"pot_id":         pot.ID,
		})
	})
	if errors.Is(err, errSkip) {
		return o.Get(ctx, txnID)
	}
	if err != nil {
		return t, err
	}
	if claimed {
		_, _ = o.AdvancePot(ctx, alloc.PotID)
	}
	return t, nil
}

type potProgress struct {
	status         pots.Status
	review         string
	refundedMinor  int64
	pendingRefunds int
}

func (o *Orchestrator) snapshotPot(ctx context.Context, potID uuid.UUID) (potProgress, error) {
	pot, err := o.st.Repos().Pots().Get(ctx, potID)
	if err != nil {
		return potProgress{}, err
	}
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, potID)
	if err != nil {
		return potProgress{}, err
	}
	sum := pots.SummarizeContributions(contribs)
	return potProgress{
		status:         pot.Status,
		review:         pot.ReviewReason,
		refundedMinor:  sum.RefundedMinor,
		pendingRefunds: sum.RefundPendingCount,
	}, nil
}

func (o *Orchestrator) resumeDuePots(stopCtx, drainCtx context.Context) (processed, progressed int, err error) {
	if stopCtx.Err() != nil {
		return 0, 0, nil
	}
	if drainCtx == nil {
		drainCtx = stopCtx
	}
	now := o.Now().UTC()
	ids, err := o.st.Repos().Pots().ClaimDue(stopCtx, now, now.Add(-o.cfg.ResumeStaleAfter), o.cfg.ResumeLease, o.cfg.ResumeBatch)
	if err != nil {
		if stopCtx.Err() != nil && errors.Is(err, context.Canceled) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, id := range ids {
		if stopCtx.Err() != nil {
			break
		}
		itemCtx := drainCtx
		if audit.CorrelationFrom(itemCtx) == nil {
			itemCtx = audit.WithCorrelation(itemCtx, uuid.New())
		}
		before, bErr := o.snapshotPot(itemCtx, id)
		if bErr != nil {
			continue
		}
		_, aErr := o.AdvancePot(itemCtx, id)
		paused := errors.Is(aErr, ErrProviderDisabled)
		if aErr != nil && !paused {
			o.log.Warn("pot resume failed", "pot", id, "err", aErr)
		}
		after, fErr := o.snapshotPot(itemCtx, id)
		moved := fErr == nil && after != before
		if moved {
			progressed++
		}
		processed++
		if sErr := o.schedulePot(itemCtx, id, moved, paused); sErr != nil {
			o.log.Warn("pot schedule failed", "pot", id, "err", sErr)
		}
	}
	return processed, progressed, nil
}

func (o *Orchestrator) schedulePot(ctx context.Context, potID uuid.UUID, moved, paused bool) error {
	now := o.Now().UTC()
	var escalated bool
	err := o.st.InTx(ctx, func(r store.Repos) error {
		pot, err := r.Pots().GetForUpdate(ctx, potID)
		if err != nil {
			return err
		}
		if pot.Status.Terminal() || pot.ReviewReason != "" {
			return nil
		}
		if pot.Status == pots.StatusOpen && now.Before(pot.Deadline) {
			pot.CheckAttempts = 0
			pot.NextCheckAt = pot.Deadline
			return r.Pots().Update(ctx, pot)
		}
		if moved {
			pot.CheckAttempts = 0
			pot.NextCheckAt = now
			return r.Pots().Update(ctx, pot)
		}
		if paused {
			pot.NextCheckAt = now.Add(o.cfg.ResumeBaseBackoff)
			return r.Pots().Update(ctx, pot)
		}
		pot.CheckAttempts++
		pot.NextCheckAt = now.Add(o.backoff(pot.CheckAttempts))
		if pot.CheckAttempts >= o.cfg.ResumeMaxAttempts {
			escalated = true
			pot.FlagForReview(fmt.Sprintf("pot reconciliation exhausted after %d attempts", pot.CheckAttempts), now)
		}
		return r.Pots().Update(ctx, pot)
	})
	if err == nil && escalated {
		o.metrics.IncResumeEscalations()
		o.log.Warn("funding pot escalated to manual review", "pot", potID, "metric", "resume_escalations_total")
	}
	return err
}

func (o *Orchestrator) buildPotView(pot *pots.Pot, m *meters.Meter, note string, sum pots.Summary, now time.Time) PotView {
	remaining := pot.TargetMinor - sum.FundedMinor
	if remaining < 0 {
		remaining = 0
	}
	masked := ""
	disco := ""
	if m != nil {
		masked = m.Masked
		disco = m.DiscoCode
	}
	return PotView{
		ID:                     pot.ID,
		Kind:                   pot.Kind,
		MeterID:                pot.MeterID,
		MaskedMeterNumber:      masked,
		Disco:                  disco,
		TargetMinor:            pot.TargetMinor,
		ElectricityAmountMinor: pot.ElectricityAmountMinor(),
		FeeMinor:               pot.FeeMinor,
		FundedMinor:            sum.FundedMinor,
		RemainingMinor:         remaining,
		RefundedMinor:          sum.RefundedMinor,
		PaidCount:              sum.PaidCount,
		SuggestedShares:        pot.SuggestedShares,
		SuggestedShareMinor:    pot.SuggestedShareMinor(),
		Currency:               pot.Currency,
		Note:                   note,
		Status:                 pot.Status,
		ReviewReason:           pot.ReviewReason,
		Deadline:               pot.Deadline,
		CreatedAt:              pot.CreatedAt,
		UpdatedAt:              pot.UpdatedAt,
	}
}

func (o *Orchestrator) loadPotDetailView(ctx context.Context, pot *pots.Pot) (*PotDetailView, error) {
	var m *meters.Meter
	if pot.MeterID != uuid.Nil {
		var err error
		m, err = o.st.Repos().Meters().Get(ctx, pot.MeterID)
		if err != nil {
			return nil, err
		}
	}
	note, err := pots.OpenPotNote(o.box, pot.ID, pot.NoteCiphertext)
	if err != nil {
		return nil, err
	}
	contribs, err := o.st.Repos().Pots().ListContributions(ctx, pot.ID)
	if err != nil {
		return nil, err
	}
	sum := pots.SummarizeContributions(contribs)
	allocs, err := o.st.Repos().Pots().ListAllocations(ctx, pot.ID)
	if err != nil {
		return nil, err
	}

	cViews := make([]PotContributionOwnerView, 0, len(contribs))
	for _, c := range contribs {
		init := c.Initials
		if strings.TrimSpace(init) == "" {
			init = "A"
		}
		cViews = append(cViews, PotContributionOwnerView{
			ID:                c.ID,
			Initials:          init,
			AmountMinor:       c.AmountMinor,
			RetainedMinor:     c.RetainedMinor,
			RefundAmountMinor: c.RefundAmountMinor,
			RefundedMinor:     c.RefundedMinor,
			Status:            c.Status,
			PaidAt:            c.PaidAt,
			CreatedAt:         c.CreatedAt,
		})
	}

	aViews := make([]PotAllocationView, 0, len(allocs))
	for _, a := range allocs {
		av := PotAllocationView{
			ID:            a.ID,
			TransactionID: a.TransactionID,
			MeterID:       a.MeterID,
			AmountMinor:   a.AmountMinor,
			Status:        a.Status,
			CreatedAt:     a.CreatedAt,
		}
		if t, tErr := o.Get(ctx, a.TransactionID); tErr == nil {
			av.OverallStatus = t.Status()
			av.DeliveryMethod = t.DeliveryMethod
		}
		aViews = append(aViews, av)
	}

	return &PotDetailView{
		PotView:       o.buildPotView(pot, m, note, sum, o.Now().UTC()),
		Contributions: cViews,
		Allocations:   aViews,
	}, nil
}
