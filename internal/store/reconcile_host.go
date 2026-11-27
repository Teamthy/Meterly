package store

import (
	"context"
	"time"

	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/reconcile"
)

// ReconcileHost adapts a Store to reconcile.Host without a package cycle.
func ReconcileHost(s Store) reconcile.Host { return reconHost{s} }

type reconHost struct{ Store }

type reconTx struct{ Repos }

func (h reconHost) InTx(ctx context.Context, fn func(reconcile.Tx) error) error {
	return h.Store.InTx(ctx, func(r Repos) error { return fn(reconTx{r}) })
}

func (h reconHost) Reconcile() reconcile.Repo { return h.Store.Repos().Reconcile() }

func (h reconHost) ListOur(ctx context.Context, from, to time.Time) ([]reconcile.OurTxn, error) {
	list, err := h.Store.Repos().Transactions().ListCreatedBetween(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]reconcile.OurTxn, 0, len(list))
	for _, t := range list {
		out = append(out, reconcile.FromTransaction(t))
	}
	return out, nil
}

func (h reconHost) Balance(ctx context.Context, acct ledger.AccountCode, c money.Currency) (int64, error) {
	return h.Store.Repos().Ledger().Balance(ctx, acct, c)
}

func (t reconTx) Reconcile() reconcile.Repo { return t.Repos.Reconcile() }

func (t reconTx) PostLedger(ctx context.Context, txn ledger.Transaction) (ledger.Transaction, error) {
	return t.Repos.Ledger().Post(ctx, txn)
}

func (t reconTx) AddAudit(ctx context.Context, e audit.Event) error {
	return t.Repos.Audit().Add(ctx, e)
}
