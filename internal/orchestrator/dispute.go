package orchestrator

import (
	"context"
	"errors"
	"strings"

	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func (o *Orchestrator) ensurePurchaserActive(ctx context.Context, repos store.Repos, customerID string) error {
	if repos == nil {
		repos = o.st.Repos()
	}
	st, err := repos.Users().Status(ctx, customerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if st == store.UserSuspended || st == store.UserClosed {
		return ErrAccountSuspended
	}
	return nil
}

// HandleChargeDispute pauses the purchaser after a Paystack chargeback.
// It does not refund and does not re-vend. Unknown references are ignored.
func (o *Orchestrator) HandleChargeDispute(ctx context.Context, providerRef string) error {
	providerRef = strings.TrimSpace(providerRef)
	if providerRef == "" {
		return nil
	}
	t, err := o.st.Repos().Transactions().ByPaymentRef(ctx, providerRef)
	if errors.Is(err, store.ErrNotFound) {
		o.log.Warn("charge dispute for unknown payment")
		return nil
	}
	if err != nil {
		return err
	}
	_, err = o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
		if t.ReviewReason == "" {
			t.FlagForReview("Paystack chargeback; do not re-vend or refund automatically")
		}
		if err := r.Users().SetStatus(ctx, t.CustomerID, store.UserSuspended); err != nil {
			return err
		}
		actor := audit.Actor{Type: audit.ActorProvider, Reason: "paystack charge.dispute"}
		if err := r.Audit().Add(ctx, actor.Event("payment.dispute", "transaction", t.ID, map[string]any{
			"action": "customer_paused",
		})); err != nil {
			return err
		}
		return emit(ctx, r, "payment.dispute", t.ID, map[string]any{"action": "customer_paused"})
	})
	return err
}
