package orchestrator

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func parseCreditKey(key string) (uuid.UUID, bool) {
	const p = "credit:"
	if !strings.HasPrefix(key, p) {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(strings.TrimPrefix(key, p))
	return id, err == nil
}

// HandleRemoteCreditNotification processes a signature-verified HES webhook.
// The body is never trusted for the outcome: we always GetCreditStatus.
func (o *Orchestrator) HandleRemoteCreditNotification(ctx context.Context, idempotencyKey string) (*tx.Transaction, error) {
	if o.remote == nil {
		return nil, ErrProviderDisabled
	}
	id, ok := parseCreditKey(strings.TrimSpace(idempotencyKey))
	if !ok {
		return nil, tx.ErrNotFound
	}
	t, err := o.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.DeliveryMethod != tx.MethodRemote {
		return t, nil
	}
	switch t.Delivery {
	case tx.DelPending, tx.DelAccepted, tx.DelUnknown:
	default:
		return t, nil
	}
	pctx, cancel := o.pctx(ctx)
	res, err := o.remote.GetCreditStatus(pctx, creditKey(t.ID))
	cancel()
	if err != nil {
		return t, err
	}
	if res.Status == providers.CreditNotFound {
		return t, nil
	}
	return o.applyCreditResult(ctx, t.ID, res)
}

func (o *Orchestrator) CreateRemoteCohort(ctx context.Context, actor audit.Actor, disco, prefix, manufacturer, evidence string) (meters.RemoteCohort, error) {
	if err := checkActor(actor); err != nil {
		return meters.RemoteCohort{}, err
	}
	c, err := meters.NewRemoteCohort(disco, prefix, manufacturer, evidence, actor.UserID, o.Now())
	if err != nil {
		return meters.RemoteCohort{}, err
	}
	err = o.st.InTx(ctx, func(r store.Repos) error {
		if err := r.RemoteCohorts().Create(ctx, c); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event("remote_credit.cohort.created", "remote_credit_cohort", c.ID, map[string]any{
			"disco": c.DiscoCode, "prefix_len": len(c.NumberPrefix),
		}))
	})
	return c, err
}

func (o *Orchestrator) ListRemoteCohorts(ctx context.Context) ([]meters.RemoteCohort, error) {
	return o.st.Repos().RemoteCohorts().List(ctx)
}

func (o *Orchestrator) RevokeRemoteCohort(ctx context.Context, actor audit.Actor, id uuid.UUID) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	return o.st.InTx(ctx, func(r store.Repos) error {
		if _, err := r.RemoteCohorts().Get(ctx, id); err != nil {
			return err
		}
		if err := r.RemoteCohorts().Revoke(ctx, id, o.Now()); err != nil {
			return err
		}
		return r.Audit().Add(ctx, actor.Event("remote_credit.cohort.revoked", "remote_credit_cohort", id, nil))
	})
}

func (o *Orchestrator) RemoteProviderName() string {
	if o.remote == nil {
		return ""
	}
	return o.remote.Name()
}
