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
	n := 0
	err := p.st.InTx(ctx, func(r store.Repos) error {
		evs, err := r.Outbox().ClaimUnpublished(ctx, p.batch)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := p.sink(ctx, e); err != nil {
				p.log.Warn("outbox sink failed; will retry", "event", e.ID, "type", e.Type, "err", err)
				return nil // commit what was published so far
			}
			if err := r.Outbox().MarkPublished(ctx, e.ID, time.Now().UTC()); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// LogSink logs event metadata (never payload values, which may become
// sensitive as consumers are added). Notification consumers replace it.
func LogSink(log *slog.Logger) Sink {
	return func(_ context.Context, e events.Event) error {
		log.Info("event", "id", e.ID, "type", e.Type, "aggregate", e.AggregateID)
		return nil
	}
}
