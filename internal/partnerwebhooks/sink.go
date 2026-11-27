// Package partnerwebhooks wires partner outbound webhooks onto the outbox
// and the store without creating an import cycle (store already imports partners).
package partnerwebhooks

import (
	"context"
	"errors"

	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

type storeRunner struct{ st store.Store }

func (s storeRunner) InTx(ctx context.Context, fn func(partners.Repo) error) error {
	return s.st.InTx(ctx, func(r store.Repos) error { return fn(r.Partners()) })
}

func Runner(st store.Store) partners.Runner { return storeRunner{st} }

// Sink enqueues partner webhook deliveries for mapped outbox events. It uses
// the transaction's repos when the publisher put them on ctx.
func Sink(eng *partners.Engine) outbox.Sink {
	return func(ctx context.Context, ev events.Event) error {
		if partners.MapOutbox(ev.Type) == "" {
			return nil
		}
		r, ok := store.ReposFromContext(ctx)
		if !ok {
			return errors.New("partner webhooks: missing store repos")
		}
		t, err := r.Transactions().Get(ctx, ev.AggregateID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return eng.Enqueue(ctx, r.Partners(), ev, partners.SnapshotOf(t))
	}
}
