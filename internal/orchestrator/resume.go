package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// Resume continues a transaction from its last committed state. It is the
// single recovery path for crashes, lost responses, missed webhooks and
// provider delays, and it is safe to run repeatedly and concurrently:
//
//   - unpaid: re-verify with the PSP; expire after PaymentExpiry
//   - paid, not vended: vend
//   - vend PENDING/UNKNOWN: query the provider by our idempotency key;
//     re-send only if the provider has no record
//   - vend failed: refund; refund pending: retry the refund call
//   - vended, not delivered: deliver
//   - remote PENDING/ACCEPTED/UNKNOWN: query the HES by our idempotency key;
//     re-send only if it has no record
func (o *Orchestrator) Resume(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Get(ctx, id)
	if err != nil || t.Settled() {
		return t, err
	}
	switch t.Payment {
	case tx.PayCreated:
		if o.Now().Sub(t.CreatedAt) < o.cfg.PaymentExpiry {
			return t, nil // client can still retry creation to get a checkout
		}
		return o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
			if t.Payment != tx.PayCreated {
				return errSkip
			}
			if err := t.SetPayment(tx.PayFailed, "abandoned before checkout was created"); err != nil {
				return err
			}
			return emit(ctx, r, "payment.failed", t.ID, map[string]any{"reason": "abandoned"})
		})
	case tx.PayPending:
		return o.checkPayment(ctx, t)
	case tx.PayRefundPending:
		return o.executeRefund(ctx, t)
	case tx.PayConfirmed:
	default:
		return t, nil
	}

	switch t.Vending {
	case tx.VendNotStarted, tx.VendFailed:
		return o.advance(ctx, id)
	case tx.VendPending, tx.VendUnknown:
		return o.reconcileVend(ctx, t)
	}

	// Vending confirmed.
	if t.Delivery == tx.DelNotStarted {
		return o.deliver(ctx, id)
	}
	if t.DeliveryMethod == tx.MethodRemote {
		switch t.Delivery {
		case tx.DelPending, tx.DelAccepted, tx.DelUnknown:
			return o.reconcileCredit(ctx, t)
		}
	}
	return t, nil
}

func (o *Orchestrator) reconcileVend(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	pctx, cancel := o.pctx(ctx)
	res, err := o.vending.GetVendStatus(pctx, vendKey(t.ID))
	cancel()
	if err != nil {
		return t, fmt.Errorf("vend status: %w", err)
	}
	if res.Status != providers.VendNotFound {
		return o.applyVendResult(ctx, t.ID, res)
	}
	// The provider confirms it never received the request, so sending it
	// (again) cannot double-vend.
	if err := o.requireEnabled(ctx, o.vending.Name()); err != nil {
		return t, err
	}
	// Re-claim (UNKNOWN -> PENDING, or touch a stale PENDING) so that only
	// one worker re-sends.
	claimed := false
	t, err = o.mutate(ctx, t.ID, func(cur *tx.Transaction, r store.Repos) error {
		if cur.Vending != t.Vending || cur.Version != t.Version {
			return errSkip // someone else moved it since we looked
		}
		claimed = true
		if cur.Vending == tx.VendUnknown {
			return cur.SetVending(tx.VendPending, "provider has no record; safe to retry")
		}
		cur.UpdatedAt = o.Now()
		return nil
	})
	if err != nil || !claimed {
		return t, err
	}
	return o.callVend(ctx, t)
}

func (o *Orchestrator) reconcileCredit(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	pctx, cancel := o.pctx(ctx)
	res, err := o.remote.GetCreditStatus(pctx, creditKey(t.ID))
	cancel()
	if err != nil {
		return t, fmt.Errorf("credit status: %w", err)
	}
	if res.Status != providers.CreditNotFound {
		return o.applyCreditResult(ctx, t.ID, res)
	}
	switch t.Delivery {
	case tx.DelAccepted:
		// The HES accepted it earlier but now has no record: never guess.
		return o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
			if t.Delivery != tx.DelAccepted {
				return errSkip
			}
			t.FlagForReview("HES lost record of an accepted credit command")
			return emit(ctx, r, "electricity.transaction.escalated", t.ID, map[string]any{"reason": "credit record lost"})
		})
	}
	if err := o.requireEnabled(ctx, o.remote.Name()); err != nil {
		return t, err
	}
	// UNKNOWN or stale PENDING and the HES has no record: re-claim so only
	// one worker re-sends.
	claimed := false
	t, err = o.mutate(ctx, t.ID, func(cur *tx.Transaction, r store.Repos) error {
		if cur.Delivery != t.Delivery || cur.Version != t.Version {
			return errSkip
		}
		claimed = true
		if cur.Delivery == tx.DelUnknown {
			return cur.SetDelivery(tx.DelPending, "HES has no record; safe to resend")
		}
		cur.UpdatedAt = o.Now()
		return nil
	})
	if err != nil || !claimed {
		return t, err
	}
	return o.sendRemoteCredit(ctx, t)
}

type progress struct {
	p tx.PaymentState
	v tx.VendingState
	d tx.DeliveryState
	r string
}

func snapshot(t *tx.Transaction) progress {
	if t == nil {
		return progress{}
	}
	return progress{t.Payment, t.Vending, t.Delivery, t.ReviewReason}
}

// ResumeDue claims due transactions and resumes each one, then schedules
// the next check with exponential backoff, escalating to MANUAL_REVIEW
// after ResumeMaxAttempts checks without progress. It returns how many
// transactions were processed and how many made progress.
func (o *Orchestrator) ResumeDue(ctx context.Context) (processed, progressed int, err error) {
	return o.ResumeDueWithDrain(ctx, ctx)
}

// ResumeDueWithDrain stops starting new transaction claims once stopCtx is
// cancelled while allowing an already-started claim to finish using drainCtx.
func (o *Orchestrator) ResumeDueWithDrain(stopCtx, drainCtx context.Context) (processed, progressed int, err error) {
	if stopCtx.Err() != nil {
		return 0, 0, nil
	}
	if drainCtx == nil {
		drainCtx = stopCtx
	}
	now := o.Now()
	ids, err := o.st.Repos().Transactions().ClaimDue(stopCtx, now, now.Add(-o.cfg.ResumeStaleAfter), o.cfg.ResumeLease, o.cfg.ResumeBatch)
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
		before, err := o.Get(itemCtx, id)
		if err != nil {
			continue
		}
		after, rerr := o.Resume(itemCtx, id)
		paused := errors.Is(rerr, ErrProviderDisabled)
		if rerr != nil && !paused {
			o.log.Warn("resume failed", "txn", id, "err", rerr)
		}
		moved := after != nil && snapshot(after) != snapshot(before)
		if moved {
			progressed++
		}
		processed++
		if err := o.schedule(itemCtx, id, moved, paused); err != nil {
			o.log.Warn("schedule failed", "txn", id, "err", err)
		}
	}
	return processed, progressed, nil
}

func (o *Orchestrator) backoff(attempts int) time.Duration {
	d := o.cfg.ResumeBaseBackoff
	for i := 1; i < attempts && d < o.cfg.ResumeMaxBackoff; i++ {
		d *= 2
	}
	if d > o.cfg.ResumeMaxBackoff {
		d = o.cfg.ResumeMaxBackoff
	}
	return d
}

// schedule sets the next check. A paused step (provider switched off by an
// operator) is retried at the base interval without counting as a failed
// attempt, so an outage the operator is managing never escalates.
func (o *Orchestrator) schedule(ctx context.Context, id uuid.UUID, moved, paused bool) error {
	now := o.Now()
	var escalated bool
	_, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Settled() {
			return errSkip
		}
		if moved {
			t.CheckAttempts = 0
			t.NextCheckAt = now
			return nil
		}
		if paused {
			t.NextCheckAt = now.Add(o.cfg.ResumeBaseBackoff)
			return nil
		}
		t.CheckAttempts++
		t.NextCheckAt = now.Add(o.backoff(t.CheckAttempts))
		// Unpaid transactions are bounded by PaymentExpiry instead: waiting
		// on a customer is not a fault.
		unpaid := t.Payment == tx.PayCreated || t.Payment == tx.PayPending
		if !unpaid && t.CheckAttempts >= o.cfg.ResumeMaxAttempts {
			escalated = true
			t.FlagForReview(fmt.Sprintf("automatic reconciliation exhausted after %d attempts", t.CheckAttempts))
			return emit(ctx, r, "electricity.transaction.escalated", t.ID, map[string]any{"attempts": t.CheckAttempts})
		}
		return nil
	})
	if err == nil && escalated {
		o.metrics.IncResumeEscalations()
		o.log.Warn("transaction escalated to manual review", "txn", id, "metric", "resume_escalations_total")
	}
	return err
}
