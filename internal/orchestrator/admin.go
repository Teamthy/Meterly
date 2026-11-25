package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Operator actions. Each one requires a reason and writes an audit event in
// the same database transaction as the change it makes.

var (
	ErrReasonRequired  = errors.New("a reason is required for operator actions")
	ErrNotInReview     = errors.New("transaction is not in manual review")
	ErrRefundNotSafe   = errors.New("refund not allowed: electricity was or may have been vended")
	ErrNothingToRefund = errors.New("the payment provider reports no successful payment to refund")
)

const resourceTxn = "electricity_transaction"

func checkActor(a audit.Actor) error {
	if a.Reason == "" {
		return ErrReasonRequired
	}
	return nil
}

// Token returns the decrypted token for the purchaser, recording the read
// in the audit log first: if the audit write fails, no token is returned.
// It is available on the token path, or after a remote-delivery failure
// was resolved to token.
func (o *Orchestrator) Token(ctx context.Context, customerID string, id uuid.UUID) (string, *tx.Transaction, error) {
	t, err := o.Get(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if t.CustomerID != customerID {
		return "", nil, tx.ErrNotFound // do not reveal existence
	}
	if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
		return "", t, ErrTokenUnavailable
	}
	actor := audit.Actor{UserID: customerID, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
	if err := o.st.Repos().Audit().Add(ctx, actor.Event("token.read", resourceTxn, id, nil)); err != nil {
		return "", t, err
	}
	tok, err := o.box.Decrypt(t.TokenCiphertext)
	return tok, t, err
}

// RevealToken lets an operator read a vended token (for example to read it
// to a customer on the phone). Masked everywhere else; every reveal is audited.
func (o *Orchestrator) RevealToken(ctx context.Context, id uuid.UUID, actor audit.Actor) (string, *tx.Transaction, error) {
	if err := checkActor(actor); err != nil {
		return "", nil, err
	}
	t, err := o.Get(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 {
		return "", t, ErrTokenUnavailable
	}
	if err := o.st.Repos().Audit().Add(ctx, actor.Event("token.reveal", resourceTxn, id, nil)); err != nil {
		return "", t, err
	}
	tok, err := o.box.Decrypt(t.TokenCiphertext)
	return tok, t, err
}

// FallbackToToken is an operator action after a confirmed remote-delivery
// failure: the already-vended token is released to the customer. STS tokens
// are single-use per meter, so releasing it cannot double-credit.
func (o *Orchestrator) FallbackToToken(ctx context.Context, id uuid.UUID, actor audit.Actor) (*tx.Transaction, error) {
	if err := checkActor(actor); err != nil {
		return nil, err
	}
	t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Delivery != tx.DelFailed {
			return fmt.Errorf("%w: fallback only allowed after confirmed delivery failure", tx.ErrInvalidTransition)
		}
		if err := t.SetDelivery(tx.DelPending, "fallback to token: "+actor.Reason); err != nil {
			return err
		}
		t.DeliveryMethod = tx.MethodToken
		if err := t.SetDelivery(tx.DelConfirmed, "token released"); err != nil {
			return err
		}
		if err := r.Audit().Add(ctx, actor.Event("transaction.fallback_to_token", resourceTxn, t.ID, nil)); err != nil {
			return err
		}
		return emit(ctx, r, "token.available", t.ID, map[string]any{"fallback": true})
	})
	if err == nil && t != nil && t.EffectiveFundingSource() == tx.FundingPot {
		o.trySettleAllocatedPot(ctx, t.ID)
	}
	return t, err
}

// Requeue takes a transaction out of manual review and resumes automatic
// processing (for example after a provider outage was fixed). If the
// underlying problem persists, automation will flag it again.
func (o *Orchestrator) Requeue(ctx context.Context, id uuid.UUID, actor audit.Actor) (*tx.Transaction, error) {
	if err := checkActor(actor); err != nil {
		return nil, err
	}
	_, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.ReviewReason == "" {
			return ErrNotInReview
		}
		prev := t.ReviewReason
		t.ReviewReason, t.CheckAttempts, t.NextCheckAt = "", 0, o.Now()
		t.History = append(t.History, tx.Event{At: o.Now(), Dimension: "review", From: "MANUAL_REVIEW", To: "REQUEUED", Note: actor.Reason})
		t.UpdatedAt = o.Now()
		return r.Audit().Add(ctx, actor.Event("transaction.requeue", resourceTxn, t.ID, map[string]any{"previous_review_reason": prev}))
	})
	if err != nil {
		return nil, err
	}
	return o.Resume(ctx, id)
}

// Reconcile runs Resume for one transaction on an operator's request.
func (o *Orchestrator) Reconcile(ctx context.Context, id uuid.UUID, actor audit.Actor) (*tx.Transaction, error) {
	if err := checkActor(actor); err != nil {
		return nil, err
	}
	if err := o.st.Repos().Audit().Add(ctx, actor.Event("transaction.reconcile", resourceTxn, id, nil)); err != nil {
		return nil, err
	}
	return o.Resume(ctx, id)
}

// OperatorRefund refunds a transaction in manual review whose customer
// provably received no electricity: vending never started or definitively
// failed. It refunds what the PSP says was actually paid, which covers an
// amount mismatch and a payment that arrived after expiry.
func (o *Orchestrator) OperatorRefund(ctx context.Context, id uuid.UUID, actor audit.Actor) (*tx.Transaction, error) {
	if err := checkActor(actor); err != nil {
		return nil, err
	}
	t, err := o.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.ReviewReason == "" {
		return t, ErrNotInReview
	}
	if t.Vending != tx.VendNotStarted && t.Vending != tx.VendFailed {
		return t, ErrRefundNotSafe
	}
	if t.EffectiveFundingSource() == tx.FundingPot {
		return o.refundPotFunded(ctx, id, "operator refund: "+actor.Reason, &actor)
	}
	if t.PaymentProviderRef == "" {
		return t, ErrNothingToRefund
	}
	pctx, cancel := o.pctx(ctx)
	v, err := o.payments.VerifyPayment(pctx, t.PaymentProviderRef)
	cancel()
	if err != nil {
		return t, fmt.Errorf("verify payment: %w", err)
	}
	if v.Status != providers.PaymentSucceeded || v.Amount.Minor <= 0 {
		return t, ErrNothingToRefund
	}
	claimed := false
	t, err = o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.ReviewReason == "" || (t.Vending != tx.VendNotStarted && t.Vending != tx.VendFailed) {
			return errSkip
		}
		switch t.Payment {
		case tx.PayPending, tx.PayFailed:
			// Money arrived but was never accepted: record its receipt first.
			if err := post(ctx, r, "pay-received:"+t.ID.String(), t.ID, "Payment received but not accepted",
				ledger.Entry{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: v.Amount},
				ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: v.Amount},
			); err != nil {
				return err
			}
		case tx.PayConfirmed:
			if v.Amount != t.Total {
				return fmt.Errorf("%w: confirmed payment amount changed at the provider", ErrNothingToRefund)
			}
		default:
			return errSkip
		}
		reason := "operator refund: " + actor.Reason
		t.RefundReason, t.RefundAmount = reason, v.Amount
		if err := t.SetPayment(tx.PayRefundPending, reason); err != nil {
			return err
		}
		if err := post(ctx, r, "refund-due:"+t.ID.String(), t.ID, "Refund owed: "+reason,
			ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: v.Amount},
			ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: v.Amount},
		); err != nil {
			return err
		}
		prev := t.ReviewReason
		t.ReviewReason = ""
		claimed = true
		if err := r.Audit().Add(ctx, actor.Event("transaction.refund", resourceTxn, t.ID,
			map[string]any{"amount_minor": v.Amount.Minor, "previous_review_reason": prev})); err != nil {
			return err
		}
		return emit(ctx, r, "electricity.refund.pending", t.ID, nil)
	})
	if err != nil || !claimed {
		return t, err
	}
	return o.executeRefund(ctx, t)
}

// SetProviderEnabled switches an integration on or off. Off pauses new
// outbound work (checkouts, vends, credit sends); nothing in flight fails.
func (o *Orchestrator) SetProviderEnabled(ctx context.Context, name string, enabled bool, actor audit.Actor) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	typ := ""
	switch name {
	case o.payments.Name():
		typ = "PAYMENT"
	case o.vending.Name():
		typ = "VENDING"
	default:
		if (o.remote != nil && name == o.remote.Name()) || (o.readings != nil && name == o.readings.Name()) {
			typ = "AMI"
		} else {
			for _, np := range o.notifProviders {
				if name == np {
					typ = "NOTIFICATION"
					break
				}
			}
		}
	}
	if typ == "" {
		return fmt.Errorf("%w: unknown provider %q", store.ErrNotFound, name)
	}
	action := "provider.disable"
	if enabled {
		action = "provider.enable"
	}
	return o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.Providers().SetEnabled(ctx, name, typ, enabled); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event(action, "provider", ProviderResourceID(name), map[string]any{"provider": name}))
	})
}

// ProviderResourceID is a stable audit resource ID for a provider name.
func ProviderResourceID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("meterly:provider:"+name))
}

// ProviderStatus lists configured providers and whether each is enabled.
func (o *Orchestrator) ProviderStatus(ctx context.Context) (map[string]bool, error) {
	names := []string{o.payments.Name(), o.vending.Name()}
	if o.remote != nil {
		names = append(names, o.remote.Name())
	}
	if o.readings != nil {
		names = append(names, o.readings.Name())
	}
	names = append(names, o.notifProviders...)
	out := map[string]bool{}
	for _, n := range names {
		ok, err := o.st.Repos().Providers().Enabled(ctx, n)
		if err != nil {
			return nil, err
		}
		out[n] = ok
	}
	return out, nil
}

// Search finds transactions for operators.
func (o *Orchestrator) Search(ctx context.Context, f store.TxnFilter) ([]*tx.Transaction, error) {
	return o.st.Repos().Transactions().Search(ctx, f)
}

// Detail is everything an operator needs to investigate one transaction.
type Detail struct {
	Transaction   *tx.Transaction
	Ledger        []ledger.Transaction
	Events        []events.Event
	Audit         []audit.Event
	Notifications []notifications.OperatorView
}

func (o *Orchestrator) Detail(ctx context.Context, id uuid.UUID) (*Detail, error) {
	t, err := o.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	r := o.st.Repos()
	d := &Detail{Transaction: t}
	if d.Ledger, err = r.Ledger().BySource(ctx, id); err != nil {
		return nil, err
	}
	if d.Events, err = r.Outbox().ForAggregate(ctx, id); err != nil {
		return nil, err
	}
	if d.Audit, err = r.Audit().ForResource(ctx, resourceTxn, id); err != nil {
		return nil, err
	}
	delivs, err := r.Notifications().ForTransaction(ctx, id)
	if err != nil {
		return nil, err
	}
	d.Notifications = make([]notifications.OperatorView, len(delivs))
	for i := range delivs {
		d.Notifications[i] = delivs[i].RedactedView()
	}
	return d, nil
}

// DetailForOperator loads Detail and fail-closed audits the operator's view of
// the transaction's notification delivery history.
func (o *Orchestrator) DetailForOperator(ctx context.Context, id uuid.UUID, actor audit.Actor) (*Detail, error) {
	d, err := o.Detail(ctx, id)
	if err != nil {
		return nil, err
	}
	ev := actor.Event("notification_deliveries.view", "notification_deliveries", id, map[string]any{
		"count": len(d.Notifications),
	})
	if err := o.st.Repos().Audit().Add(ctx, ev); err != nil {
		return nil, err
	}
	return d, nil
}
