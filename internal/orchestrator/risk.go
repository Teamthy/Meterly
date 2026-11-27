package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (o *Orchestrator) payRequestID(ctx context.Context, r store.Repos, txnID uuid.UUID) uuid.UUID {
	p, err := r.PayRequests().GetPayer(ctx, txnID)
	if err != nil {
		return uuid.Nil
	}
	return p.PaymentRequestID
}

func (o *Orchestrator) SetFeatureRiskEngine(v bool) { o.featureRiskEngine = v }

func (o *Orchestrator) riskHash(kind, v string) string {
	v = strings.TrimSpace(v)
	if o.box == nil || v == "" {
		return ""
	}
	return o.box.Hash("risk-" + kind + ":" + strings.ToLower(v))
}

func (o *Orchestrator) evaluateRisk(ctx context.Context, r store.Repos, req risk.Request) (risk.Decision, error) {
	if o == nil || !o.featureRiskEngine {
		return risk.Decision{Outcome: risk.Allow}, nil
	}
	if req.UserID != "" {
		if err := r.Users().LockForUpdate(ctx, req.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return risk.Decision{}, err
		}
	}
	d, err := risk.NewEngine(r.Risk(), o.Now).Evaluate(ctx, req)
	if d.ID != uuid.Nil && req.TransactionID != uuid.Nil && d.TransactionID == uuid.Nil {
		_ = r.Risk().BindDecisionTxn(ctx, d.ID, req.TransactionID)
		d.TransactionID = req.TransactionID
	}
	return d, err
}

func (o *Orchestrator) recordRiskFailure(ctx context.Context, r store.Repos, req risk.Request) {
	if o == nil || !o.featureRiskEngine {
		return
	}
	_ = risk.NewEngine(r.Risk(), o.Now).RecordFailure(ctx, req)
}

func (o *Orchestrator) applyReview(t *tx.Transaction, d risk.Decision) {
	if t == nil || d.Outcome != risk.Review {
		return
	}
	t.FlagForReview(d.ReviewReason())
}

// HandlePaymentDispute ingest a Paystack charge.dispute.* webhook.
// Create still pauses the purchaser (HandleChargeDispute). Ledger posts are
// idempotent by dispute-open|win|loss:{psp_id}.
func (o *Orchestrator) HandlePaymentDispute(ctx context.Context, n disputes.Notice) error {
	n.Type = strings.TrimSpace(n.Type)
	n.Reference = strings.TrimSpace(n.Reference)
	n.PSPDisputeID = strings.TrimSpace(n.PSPDisputeID)
	if n.Reference == "" && n.PSPDisputeID == "" {
		return nil
	}
	if strings.HasSuffix(n.Type, ".create") || n.Type == "" {
		if err := o.HandleChargeDispute(ctx, n.Reference); err != nil {
			return err
		}
	}
	return o.persistDispute(ctx, n)
}

func (o *Orchestrator) persistDispute(ctx context.Context, n disputes.Notice) error {
	now := o.Now().UTC()
	return o.st.InTx(ctx, func(r store.Repos) error {
		d := disputes.Dispute{
			PSPDisputeID: n.PSPDisputeID,
			PaymentRef:   n.Reference,
			Amount:       money.New(n.AmountMinor, money.Currency(orCurrency(n.Currency))),
			PSPStatus:    n.PSPStatus,
			Resolution:   n.Resolution,
			Category:     n.Category,
			DueAt:        n.DueAt,
			Status:       disputes.StatusOpen,
		}
		if d.PSPDisputeID == "" {
			d.PSPDisputeID = "ref:" + n.Reference
		}
		if n.Reference != "" {
			if t, err := r.Transactions().ByPaymentRef(ctx, n.Reference); err == nil {
				d.TransactionID = t.ID
				if d.Amount.Minor <= 0 {
					d.Amount = t.Total
				}
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		switch {
		case strings.HasSuffix(n.Type, ".remind"):
			d.Status = disputes.StatusReminded
			d.RemindedAt = now
		case strings.HasSuffix(n.Type, ".resolve"):
			if out := disputes.OutcomeFromResolution(n.Resolution, n.PSPStatus); out != "" {
				d.Status = out
			} else {
				d.Status = disputes.StatusLost
			}
			d.ResolvedAt = now
		}
		if existing, err := r.Disputes().ByPSP(ctx, d.PSPDisputeID); err == nil {
			d.ID = existing.ID
			if existing.Status.Terminal() && !d.Status.Terminal() {
				d.Status = existing.Status
				d.ResolvedAt = existing.ResolvedAt
			}
		} else if err != nil && !errors.Is(err, disputes.ErrNotFound) {
			return err
		}
		saved, err := r.Disputes().Upsert(ctx, d)
		if err != nil {
			return err
		}
		if saved.Amount.Minor <= 0 {
			return nil
		}
		if _, err := r.Ledger().Post(ctx, disputes.OpenPosting(saved, now)); err != nil {
			return err
		}
		switch saved.Status {
		case disputes.StatusWon:
			if _, err := r.Ledger().Post(ctx, disputes.WinPosting(saved, now)); err != nil {
				return err
			}
		case disputes.StatusLost, disputes.StatusAccepted:
			if _, err := r.Ledger().Post(ctx, disputes.LossPosting(saved, now)); err != nil {
				return err
			}
			o.maybeDenyRepeat(ctx, r, saved)
		}
		return nil
	})
}

func (o *Orchestrator) maybeDenyRepeat(ctx context.Context, r store.Repos, d disputes.Dispute) {
	if d.TransactionID == uuid.Nil || !o.featureRiskEngine {
		return
	}
	t, err := r.Transactions().Get(ctx, d.TransactionID)
	if err != nil {
		return
	}
	n, err := r.Disputes().CountLostForUser(ctx, t.CustomerID)
	if err != nil || n < 2 {
		return
	}
	decs, _ := r.Risk().ListDecisionsByTxn(ctx, t.ID)
	var email, card string
	for _, dec := range decs {
		if email == "" {
			email = dec.EmailHMAC
		}
		if card == "" {
			card = dec.CardSignature
		}
	}
	if email != "" {
		_ = r.Risk().AddDenylist(ctx, risk.DenyEntry{
			ID: uuid.New(), Kind: risk.DenyEmail, Value: email,
			Reason: "repeated chargebacks", CreatedAt: o.Now().UTC(),
		})
	}
	if card != "" {
		_ = r.Risk().AddDenylist(ctx, risk.DenyEntry{
			ID: uuid.New(), Kind: risk.DenyCard, Value: card,
			Reason: "repeated chargebacks", CreatedAt: o.Now().UTC(),
		})
	}
}

func (o *Orchestrator) DisputeEvidence(ctx context.Context, id uuid.UUID) (disputes.EvidencePack, error) {
	d, err := o.st.Repos().Disputes().Get(ctx, id)
	if err != nil {
		return disputes.EvidencePack{}, err
	}
	pack := disputes.EvidencePack{
		TransactionID: d.TransactionID.String(),
		AmountMinor:   d.Amount.Minor,
		Currency:      string(d.Amount.Currency),
	}
	if d.TransactionID == uuid.Nil {
		return pack, nil
	}
	t, err := o.Get(ctx, d.TransactionID)
	if err != nil {
		return pack, nil
	}
	pack.PaymentStatus = string(t.Payment)
	pack.VendingStatus = string(t.Vending)
	pack.DeliveryStatus = string(t.Delivery)
	pack.DeliveryMethod = string(t.DeliveryMethod)
	pack.CreatedAt = t.CreatedAt.UTC().Format(time.RFC3339)
	pack.HasToken = len(t.TokenCiphertext) > 0
	if t.Payment == tx.PayConfirmed {
		pack.PaidNote = "payment verified server-side"
	}
	if t.Vending == tx.VendConfirmed {
		pack.VendNote = "vending confirmed"
	}
	if t.Delivery == tx.DelConfirmed {
		pack.DeliveryNote = "delivery confirmed"
	}
	raw, _ := json.Marshal(pack)
	_ = o.st.Repos().Disputes().SetEvidence(ctx, id, raw)
	return pack, nil
}

func orCurrency(c string) string {
	if c == "" {
		return "NGN"
	}
	return c
}
