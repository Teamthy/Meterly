package notificationconsumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/notificationconsumer"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

func TestMain(m *testing.M) { code := m.Run(); pgtest.DropTemplate(); os.Exit(code) }
func TestConsumer(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }},
		{"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) { exercise(t, b.open(t)) })
	}
}
func exercise(t *testing.T, st store.Store) {
	ctx := context.Background()
	since := time.Now().UTC().Add(-time.Minute)
	id := uuid.New()
	e := events.New("token.available", id, map[string]any{"phone": "synthetic-sensitive-phone", "token": "synthetic-sensitive-token"})
	other := events.New("something.else", id, nil)
	foreign := events.New("payment.confirmed", id, nil)
	foreign.AggregateType = "another_kind"
	for _, event := range []events.Event{e, other, foreign} {
		if err := st.Repos().Outbox().Add(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Repos().Outbox().MarkPublished(ctx, e.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Simulate a failure after recording: the candidate must roll back.
	injected := errors.New("injected failure")
	err := st.InTx(ctx, func(r store.Repos) error {
		items, err := r.NotificationCandidates().Claim(ctx, since, 10)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Fatalf("expected one candidate, got %d", len(items))
		}
		items[0].CreatedAt = time.Now().UTC()
		if err := r.NotificationCandidates().Record(ctx, items[0]); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, err := st.Repos().NotificationCandidates().Get(ctx, e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rollback failed: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := notificationconsumer.ConsumeBatch(ctx, st, since, 10)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("concurrent consumers recorded %d candidates", total)
	}
	c, err := st.Repos().NotificationCandidates().Get(ctx, e.ID)
	if err != nil || c.TransactionID != id || c.EventType != e.Type || c.CreatedAt.IsZero() {
		t.Fatalf("bad candidate: %+v %v", c, err)
	}
	data, _ := json.Marshal(c)
	if strings.Contains(string(data), "synthetic-sensitive") {
		t.Fatal("sensitive payload leaked")
	}
	if n, err := notificationconsumer.ConsumeBatch(ctx, st, since, 10); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
	if _, err := st.Repos().NotificationCandidates().Get(ctx, other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unsupported event recorded")
	}
	// A matching replay is harmless, but a different event identity conflicts.
	err = st.InTx(ctx, func(r store.Repos) error {
		if err := r.NotificationCandidates().Record(ctx, c); err != nil {
			return err
		}
		bad := c
		bad.TransactionID = uuid.New()
		if !errors.Is(r.NotificationCandidates().Record(ctx, bad), store.ErrConflict) {
			return errors.New("mismatched candidate replay accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bounded batches make forward progress and do not rely on published_at.
	extra := []events.Event{events.New("payment.confirmed", id, nil), events.New("electricity.refund.completed", id, nil)}
	for _, ev := range extra {
		if err := st.Repos().Outbox().Add(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		n, err := notificationconsumer.ConsumeBatch(ctx, st, since, 1)
		if err != nil || n != 1 {
			t.Fatalf("bounded batch: %d %v", n, err)
		}
	}
	if n, err := notificationconsumer.ConsumeBatch(ctx, st, since, 1); err != nil || n != 0 {
		t.Fatalf("unexpected remainder: %d %v", n, err)
	}
	if _, err := st.Repos().NotificationCandidates().Claim(ctx, since, 10); err == nil {
		t.Fatal("unlocked claim allowed")
	}
	if _, err := notificationconsumer.ConsumeBatch(ctx, st, since, 0); !errors.Is(err, notificationconsumer.ErrInvalidLimit) {
		t.Fatal("invalid limit accepted")
	}
	evs, err := st.Repos().Outbox().ForAggregate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range evs {
		if v.ID == other.ID && v.PublishedAt != nil {
			t.Fatal("consumer modified publish status")
		}
	}
}

func TestCutoffInclusiveAndLateEvents(t *testing.T) {
	for _, b := range []struct {
		name string
		open func(*testing.T) store.Store
	}{
		{"memory", func(*testing.T) store.Store { return memstore.New() }}, {"postgres", func(t *testing.T) store.Store { return pgstore.New(pgtest.NewPool(t), "SANDBOX") }},
	} {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			st := b.open(t)
			cutoff := time.Now().UTC().Truncate(time.Second)
			id := uuid.New()
			old := events.New("token.available", id, nil)
			old.CreatedAt = cutoff.Add(-time.Microsecond)
			equal := events.New("payment.confirmed", id, nil)
			equal.CreatedAt = cutoff
			for _, ev := range []events.Event{old, equal} {
				if err := st.Repos().Outbox().Add(ctx, ev); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := notificationconsumer.ConsumeBatch(ctx, st, time.Time{}, 1); !errors.Is(err, notificationconsumer.ErrInvalidCutoff) {
				t.Fatal("zero cutoff accepted")
			}
			n, err := notificationconsumer.ConsumeBatch(ctx, st, cutoff, 10)
			if err != nil || n != 1 {
				t.Fatalf("cutoff: %d %v", n, err)
			}
			if _, err := st.Repos().NotificationCandidates().Get(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("historic event consumed")
			}
			if _, err := st.Repos().NotificationCandidates().Get(ctx, equal.ID); err != nil {
				t.Fatal("inclusive boundary missed", err)
			}
			// No high-water cursor: an event appearing later with an eligible timestamp is still collected.
			late := events.New("electricity.refund.completed", id, nil)
			late.CreatedAt = cutoff
			if err := st.Repos().Outbox().Add(ctx, late); err != nil {
				t.Fatal(err)
			}
			n, err = notificationconsumer.ConsumeBatch(ctx, st, cutoff, 10)
			if err != nil || n != 1 {
				t.Fatalf("late event: %d %v", n, err)
			}
		})
	}
}
