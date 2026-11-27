package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const opCreatePartnerPurchase = "create_partner_purchase"

type PartnerQuoteRequest struct {
	PartnerID   uuid.UUID
	MeterID     uuid.UUID
	MeterNumber string
	DiscoCode   string
	AmountMinor int64
	PackID      string
	Pref        DeliveryPreference
}

type PartnerPurchaseRequest struct {
	PartnerID      uuid.UUID
	QuoteID        uuid.UUID
	PartnerRef     string
	IdempotencyKey string
	RequestHash    string
}

func (o *Orchestrator) ensurePartnerActive(ctx context.Context, id uuid.UUID) (partners.Partner, error) {
	p, err := o.st.Repos().Partners().GetPartner(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return partners.Partner{}, partners.ErrNotFound
		}
		return partners.Partner{}, err
	}
	if p.Status == partners.StatusSuspended {
		return p, partners.ErrSuspended
	}
	if p.Status != partners.StatusActive {
		return p, partners.ErrNotActive
	}
	return p, nil
}

func (o *Orchestrator) partnerUser(ctx context.Context, partnerID uuid.UUID) (string, error) {
	return o.st.Repos().Users().EnsureByExternalID(ctx, partners.UserExternalID(partnerID))
}

func validPartnerRef(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 8 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == ':' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func validReceiptRef(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 4 && len(s) <= 128
}

func topupLedgerRef(receipt string) string {
	sum := sha256.Sum256([]byte("partner-float-topup:" + receipt))
	return "partner-float-topup:" + hex.EncodeToString(sum[:16])
}

func (o *Orchestrator) PartnerFloat(ctx context.Context, partnerID uuid.UUID) (partners.Float, error) {
	if _, err := o.st.Repos().Partners().GetPartner(ctx, partnerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return partners.Float{}, partners.ErrNotFound
		}
		return partners.Float{}, err
	}
	return o.st.Repos().Partners().GetFloat(ctx, partnerID)
}

func (o *Orchestrator) ProposePartnerFloatTopup(ctx context.Context, actor audit.Actor, partnerID uuid.UUID, receiptRef string, amountMinor int64, currency string) (partners.FloatTopup, error) {
	if err := checkActor(actor); err != nil {
		return partners.FloatTopup{}, err
	}
	receiptRef = strings.TrimSpace(receiptRef)
	if !validReceiptRef(receiptRef) {
		return partners.FloatTopup{}, ErrReceiptRef
	}
	if amountMinor <= 0 {
		return partners.FloatTopup{}, fmt.Errorf("%w: amount must be positive", ErrReceiptRef)
	}
	if currency == "" {
		currency = "NGN"
	}
	if currency != "NGN" {
		return partners.FloatTopup{}, partners.ErrInvalidInput
	}
	if _, err := o.ensurePartnerActive(ctx, partnerID); err != nil {
		return partners.FloatTopup{}, err
	}
	if existing, err := o.st.Repos().Partners().GetTopupByReceipt(ctx, receiptRef); err == nil {
		if existing.PartnerID == partnerID && existing.AmountMinor == amountMinor && existing.Currency == currency {
			return existing, nil
		}
		return partners.FloatTopup{}, partners.ErrReceiptConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return partners.FloatTopup{}, err
	}
	now := o.Now().UTC()
	t := partners.FloatTopup{
		ID: uuid.New(), PartnerID: partnerID, ReceiptRef: receiptRef,
		AmountMinor: amountMinor, Currency: currency, Status: partners.TopupPending,
		ProposedBy: actor.UserID, ProposedAt: now, Reason: actor.Reason,
		LedgerReference: topupLedgerRef(receiptRef),
	}
	err := o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Partners().CreateTopup(ctx, t); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event("partner.float.topup.proposed", "partner_float_topup", t.ID, map[string]any{
			"partner_id": partnerID.String(), "amount_minor": amountMinor,
		}))
	})
	if errors.Is(err, store.ErrDuplicate) {
		existing, gerr := o.st.Repos().Partners().GetTopupByReceipt(ctx, receiptRef)
		if gerr == nil && existing.PartnerID == partnerID && existing.AmountMinor == amountMinor && existing.Currency == currency {
			return existing, nil
		}
		return partners.FloatTopup{}, partners.ErrReceiptConflict
	}
	return t, err
}

func (o *Orchestrator) ApprovePartnerFloatTopup(ctx context.Context, actor audit.Actor, topupID uuid.UUID) (partners.FloatTopup, error) {
	if err := checkActor(actor); err != nil {
		return partners.FloatTopup{}, err
	}
	var out partners.FloatTopup
	err := o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Partners().GetTopupForUpdate(ctx, topupID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return partners.ErrNotFound
			}
			return err
		}
		if t.Status == partners.TopupApproved {
			out = t
			return nil
		}
		if t.Status != partners.TopupPending {
			return partners.ErrNotPendingTopup
		}
		if t.ProposedBy != "" && t.ProposedBy == actor.UserID {
			return partners.ErrSameApprover
		}
		p, err := r.Partners().GetPartner(ctx, t.PartnerID)
		if err != nil {
			return err
		}
		if p.Status == partners.StatusSuspended {
			return partners.ErrSuspended
		}
		if p.Status != partners.StatusActive {
			return partners.ErrNotActive
		}
		now := o.Now().UTC()
		amt := money.New(t.AmountMinor, money.Currency(t.Currency))
		if _, err := r.Ledger().Post(ctx, ledger.Transaction{
			Reference: t.LedgerReference, SourceType: "partner_float_topup", SourceID: t.ID,
			Description: "Partner float top-up from bank receipt", PostedAt: now,
			Entries: []ledger.Entry{
				{Account: ledger.CashAtBank, Direction: ledger.Debit, Amount: amt},
				{Account: ledger.PartnerFloat, Direction: ledger.Credit, Amount: amt},
			},
		}); err != nil {
			return err
		}
		f, err := r.Partners().EnsureFloatForUpdate(ctx, t.PartnerID, now)
		if err != nil {
			return err
		}
		f.AvailableMinor += t.AmountMinor
		f.UpdatedAt = now
		if err := r.Partners().UpdateFloat(ctx, f); err != nil {
			return err
		}
		t.Status = partners.TopupApproved
		t.ApprovedBy = actor.UserID
		t.ApprovedAt = &now
		if actor.Reason != "" {
			t.Reason = actor.Reason
		}
		if err := r.Partners().UpdateTopup(ctx, t); err != nil {
			return err
		}
		if err := r.Audit().Add(ctx, actor.Event("partner.float.topup.approved", "partner_float_topup", t.ID, map[string]any{
			"partner_id": t.PartnerID.String(), "amount_minor": t.AmountMinor,
		})); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

func (o *Orchestrator) RejectPartnerFloatTopup(ctx context.Context, actor audit.Actor, topupID uuid.UUID) (partners.FloatTopup, error) {
	if err := checkActor(actor); err != nil {
		return partners.FloatTopup{}, err
	}
	var out partners.FloatTopup
	err := o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Partners().GetTopupForUpdate(ctx, topupID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return partners.ErrNotFound
			}
			return err
		}
		if t.Status == partners.TopupRejected {
			out = t
			return nil
		}
		if t.Status != partners.TopupPending {
			return partners.ErrNotPendingTopup
		}
		now := o.Now().UTC()
		t.Status = partners.TopupRejected
		t.RejectedBy = actor.UserID
		t.RejectedAt = &now
		if actor.Reason != "" {
			t.Reason = actor.Reason
		}
		if err := r.Partners().UpdateTopup(ctx, t); err != nil {
			return err
		}
		if err := r.Audit().Add(ctx, actor.Event("partner.float.topup.rejected", "partner_float_topup", t.ID, map[string]any{
			"partner_id": t.PartnerID.String(),
		})); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

func (o *Orchestrator) IssuePartnerQuote(ctx context.Context, req PartnerQuoteRequest) (*tx.Quote, error) {
	if _, err := o.ensurePartnerActive(ctx, req.PartnerID); err != nil {
		return nil, err
	}
	meterID := req.MeterID
	if meterID == uuid.Nil {
		if strings.TrimSpace(req.MeterNumber) == "" || strings.TrimSpace(req.DiscoCode) == "" {
			return nil, ErrPartnerMeter
		}
		m, err := o.Meters.Validate(ctx, req.MeterNumber, req.DiscoCode)
		if err != nil {
			return nil, err
		}
		meterID = m.ID
	}
	uid, err := o.partnerUser(ctx, req.PartnerID)
	if err != nil {
		return nil, err
	}
	return o.IssueQuote(ctx, QuoteSpec{
		CustomerID: uid, MeterID: meterID, AmountMinor: req.AmountMinor, PackID: req.PackID, Pref: req.Pref,
		PartnerID: req.PartnerID, SkipAuthorize: true,
	})
}

func (o *Orchestrator) CreatePartnerPurchase(ctx context.Context, req PartnerPurchaseRequest) (t *tx.Transaction, replayed bool, err error) {
	req.PartnerRef = strings.TrimSpace(req.PartnerRef)
	if !validPartnerRef(req.PartnerRef) {
		return nil, false, ErrPartnerRef
	}
	if _, err := o.ensurePartnerActive(ctx, req.PartnerID); err != nil {
		return nil, false, err
	}
	if err := o.ensurePurchasesOpen(ctx); err != nil {
		return nil, false, err
	}
	uid, err := o.partnerUser(ctx, req.PartnerID)
	if err != nil {
		return nil, false, err
	}
	now := o.Now()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		if req.IdempotencyKey != "" {
			existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
				Scope: req.PartnerID.String(), Operation: opCreatePartnerPurchase, Key: req.IdempotencyKey, RequestHash: req.RequestHash,
				CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL),
			})
			if err != nil {
				return err
			}
			if existing != nil {
				if existing.RequestHash != req.RequestHash {
					return idempotency.ErrConflict
				}
				if existing.ResultRef == "" {
					return idempotency.ErrInProgress
				}
				id, err := uuid.Parse(existing.ResultRef)
				if err != nil {
					return err
				}
				t, err = r.Transactions().Get(ctx, id)
				replayed = true
				return err
			}
		}
		q, err := r.Quotes().GetForUpdate(ctx, req.QuoteID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrQuoteNotFound
		}
		if err != nil {
			return err
		}
		if q.PartnerID != req.PartnerID && q.CustomerID != uid {
			return ErrQuoteNotFound
		}
		if q.Used || now.After(q.ExpiresAt) {
			return ErrQuoteExpired
		}
		if err := r.Quotes().MarkUsed(ctx, q.ID); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ErrQuoteExpired
			}
			return err
		}
		p, err := r.Partners().GetPartner(ctx, req.PartnerID)
		if err != nil {
			return err
		}
		f, err := r.Partners().GetFloatForUpdate(ctx, req.PartnerID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrInsufficientFloat
		}
		if err != nil {
			return err
		}
		if f.AvailableMinor < q.Total.Minor {
			return ErrInsufficientFloat
		}
		day := timeUTCDate(now)
		u, err := r.Partners().GetUsage(ctx, req.PartnerID, day)
		if err != nil {
			return err
		}
		if p.DailyValueCapMinor > 0 && u.ValueMinor+q.Total.Minor > p.DailyValueCapMinor {
			return ErrPartnerDailyCap
		}
		t = tx.NewPartnerFloatFunded(uid, q, req.PartnerID, req.PartnerRef, now)
		if err := r.Transactions().Create(ctx, t); err != nil {
			if errors.Is(err, store.ErrDuplicate) {
				return ErrPartnerRefConflict
			}
			return err
		}
		f.AvailableMinor -= q.Total.Minor
		f.UpdatedAt = now.UTC()
		if err := r.Partners().UpdateFloat(ctx, f); err != nil {
			return err
		}
		if err := r.Partners().IncrementUsage(ctx, req.PartnerID, day, 0, q.Total.Minor); err != nil {
			return err
		}
		if err := o.post(ctx, r, "partner-float:"+t.ID.String(), t.ID, "Partner float applied to electricity purchase",
			ledger.Entry{Account: ledger.PartnerFloat, Direction: ledger.Debit, Amount: q.Total},
			ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: q.Total},
		); err != nil {
			return err
		}
		if err := emit(ctx, r, "electricity.transaction.created", t.ID, map[string]any{
			"funding_source": string(tx.FundingPartnerFloat),
			"partner_id":     req.PartnerID.String(),
		}); err != nil {
			return err
		}
		if err := emit(ctx, r, "payment.confirmed", t.ID, map[string]any{
			"funding_source": string(tx.FundingPartnerFloat),
			"partner_id":     req.PartnerID.String(),
		}); err != nil {
			return err
		}
		if req.IdempotencyKey != "" {
			return r.Idempotency().Complete(ctx, req.PartnerID.String(), opCreatePartnerPurchase, req.IdempotencyKey, t.ID.String())
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if replayed {
		return t, true, nil
	}
	t, err = o.advance(ctx, t.ID)
	return t, false, err
}

func timeUTCDate(now time.Time) time.Time {
	n := now.UTC()
	return n.Truncate(24 * time.Hour)
}

func (o *Orchestrator) GetPartnerPurchase(ctx context.Context, partnerID, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.PartnerID != partnerID {
		return nil, tx.ErrNotFound
	}
	return t, nil
}

func (o *Orchestrator) GetPartnerPurchaseByRef(ctx context.Context, partnerID uuid.UUID, ref string) (*tx.Transaction, error) {
	t, err := o.st.Repos().Transactions().GetByPartnerRef(ctx, partnerID, strings.TrimSpace(ref))
	if errors.Is(err, store.ErrNotFound) {
		return nil, tx.ErrNotFound
	}
	return t, err
}

func (o *Orchestrator) PartnerToken(ctx context.Context, partnerID, id uuid.UUID) (string, *tx.Transaction, error) {
	t, err := o.GetPartnerPurchase(ctx, partnerID, id)
	if err != nil {
		return "", nil, err
	}
	if t.DeliveryMethod == tx.MethodRemote {
		return "", t, ErrCreditOnMeter
	}
	if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
		return "", t, ErrTokenUnavailable
	}
	tok, err := o.box.Decrypt(t.TokenCiphertext)
	return tok, t, err
}

func (o *Orchestrator) refundPartnerFloat(ctx context.Context, txnID uuid.UUID, reason string, actor *audit.Actor) (*tx.Transaction, error) {
	now := o.Now().UTC()
	var t *tx.Transaction
	err := o.st.InTx(ctx, func(r store.Repos) error {
		var err error
		t, err = r.Transactions().GetForUpdate(ctx, txnID)
		if err != nil {
			return err
		}
		if t.EffectiveFundingSource() != tx.FundingPartnerFloat {
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
			reason = "partner float returned"
		}
		t.RefundReason = reason
		t.RefundAmount = t.Total
		if t.Payment == tx.PayConfirmed {
			if err := t.SetPayment(tx.PayRefundPending, reason); err != nil {
				return err
			}
		}
		t.RefundProviderRef = "partner-float-return:" + t.PartnerID.String()
		if err := t.SetPayment(tx.PayRefunded, "funds returned to partner float"); err != nil {
			return err
		}
		if actor != nil {
			prev := t.ReviewReason
			t.ReviewReason = ""
			if err := r.Audit().Add(ctx, actor.Event("transaction.refund", resourceTxn, t.ID, map[string]any{
				"amount_minor":           t.Total.Minor,
				"funding_source":         string(tx.FundingPartnerFloat),
				"partner_id":             t.PartnerID,
				"previous_review_reason": prev,
			})); err != nil {
				return err
			}
		}
		if err := r.Transactions().Save(ctx, t); err != nil {
			return err
		}
		if err := o.post(ctx, r, "refund-partner-float:"+t.ID.String(), t.ID, "Returned partner-float purchase: "+reason,
			ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
			ledger.Entry{Account: ledger.PartnerFloat, Direction: ledger.Credit, Amount: t.Total},
		); err != nil {
			return err
		}
		f, err := r.Partners().EnsureFloatForUpdate(ctx, t.PartnerID, now)
		if err != nil {
			return err
		}
		f.AvailableMinor += t.Total.Minor
		f.UpdatedAt = now
		if err := r.Partners().UpdateFloat(ctx, f); err != nil {
			return err
		}
		return emit(ctx, r, "electricity.refund.completed", t.ID, map[string]any{
			"funding_source": string(tx.FundingPartnerFloat),
			"partner_id":     t.PartnerID.String(),
		})
	})
	if errors.Is(err, errSkip) {
		return o.Get(ctx, txnID)
	}
	return t, err
}
