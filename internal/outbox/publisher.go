// Package outbox publishes committed outbox events to a sink. Delivery is
// at-least-once: an event is marked published only after the sink accepts
// it, in the same transaction that claimed it, so a crash re-delivers it.
// Sinks must dedupe on Event.ID.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/store"
)

// Sink receives events. It runs while the claimed rows are locked, so it
// should be quick; a slow external broker belongs behind a local queue.
type Sink func(ctx context.Context, e events.Event) error

// Chain composes multiple sinks in order, stopping on the first error.
func Chain(sinks ...Sink) Sink {
	return func(ctx context.Context, e events.Event) error {
		for _, s := range sinks {
			if s == nil {
				continue
			}
			if err := s(ctx, e); err != nil {
				return err
			}
		}
		return nil
	}
}

type Publisher struct {
	st    store.Store
	sink  Sink
	log   *slog.Logger
	batch int
}

func NewPublisher(st store.Store, sink Sink, log *slog.Logger) *Publisher {
	return &Publisher{st: st, sink: sink, log: log, batch: 100}
}

// PublishBatch publishes up to one batch and returns how many were
// published. It stops at the first sink error; the rest stay pending.
func (p *Publisher) PublishBatch(ctx context.Context) (int, error) {
	return p.PublishBatchWithDrain(ctx, ctx)
}

// PublishBatchWithDrain stops claiming new unpublished events once stopCtx is
// cancelled while allowing an already-started event transaction to commit on drainCtx.
func (p *Publisher) PublishBatchWithDrain(stopCtx, drainCtx context.Context) (int, error) {
	if drainCtx == nil {
		drainCtx = stopCtx
	}
	n := 0
	for i := 0; i < p.batch; i++ {
		if stopCtx.Err() != nil {
			return n, nil
		}
		var (
			claimed bool
			sinkErr error
		)
		err := p.st.InTx(drainCtx, func(r store.Repos) error {
			evs, err := r.Outbox().ClaimUnpublished(drainCtx, 1)
			if err != nil {
				return err
			}
			if len(evs) == 0 {
				return nil
			}
			claimed = true
			e := evs[0]
			if err := p.sink(store.WithRepos(drainCtx, r), e); err != nil {
				sinkErr = err
				return err // roll back any partial sink writes for this event
			}
			return r.Outbox().MarkPublished(drainCtx, e.ID, time.Now().UTC())
		})
		if sinkErr != nil {
			p.log.Warn("outbox sink failed; will retry", "err", sinkErr)
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if !claimed {
			break
		}
		n++
	}
	return n, nil
}

// LogSink logs event metadata (never payload values, which may become
// sensitive as consumers are added). Notification consumers replace it.
func LogSink(log *slog.Logger) Sink {
	return func(_ context.Context, e events.Event) error {
		log.Info("event", "id", e.ID, "type", e.Type, "aggregate", e.AggregateID)
		return nil
	}
}
