package notifications

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestTemplateFor(t *testing.T) {
	for _, tc := range []struct {
		typ         string
		method      tx.DeliveryMethod
		ok, wantErr bool
	}{
		{"payment.confirmed", tx.MethodToken, true, false},
		{"token.available", tx.MethodToken, true, false},
		{"token.available", tx.MethodRemote, false, true},
		{"electricity.delivery.confirmed", tx.MethodRemote, true, false},
		{"electricity.delivery.confirmed", tx.MethodToken, false, false},
		{"electricity.delivery.failed", tx.MethodRemote, true, false},
		{"electricity.delivery.unknown", tx.MethodRemote, true, false},
		{"electricity.refund.completed", tx.MethodToken, true, false},
		{"electricity.delivery.accepted", tx.MethodRemote, false, false},
		{"unknown.event", tx.MethodToken, false, false},
	} {
		t.Run(tc.typ+string(tc.method), func(t *testing.T) {
			e := events.New(tc.typ, uuid.New(), map[string]any{"token": "SYNTHETIC_TOKEN", "meter": "SYNTHETIC_METER"})
			content, ok, err := TemplateFor(e, tc.method)
			if ok != tc.ok || (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %v %v", ok, err)
			}
			if strings.Contains(content.Body, "SYNTHETIC_") {
				t.Fatal("event payload leaked")
			}
			if tc.typ == "token.available" && tc.ok && !strings.Contains(content.Body, "only on the meter") {
				t.Fatal("missing token scope warning")
			}
		})
	}
	if _, _, err := TemplateFor(events.Event{}, tx.MethodToken); err == nil {
		t.Fatal("invalid event accepted")
	}
}
func TestMockSenderConcurrentDedup(t *testing.T) {
	s, err := NewMockSender()
	if err != nil {
		t.Fatal(err)
	}
	m := Message{EventID: uuid.New(), TransactionID: uuid.New(), Channel: SMS, Destination: "synthetic-phone", Subject: "subject", Body: "synthetic message"}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Send(context.Background(), m); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s.Count() != 1 {
		t.Fatal("duplicate sends")
	}
	changed := m
	changed.Body = "different"
	if !errors.Is(s.Send(context.Background(), changed), ErrDuplicateMismatch) {
		t.Fatal("mismatch accepted")
	}
	changed = m
	changed.Destination = "another"
	if err := s.Send(context.Background(), changed); err != nil || s.Count() != 2 {
		t.Fatal("distinct recipient not sent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed.EventID = uuid.New()
	if !errors.Is(s.Send(ctx, changed), context.Canceled) || s.Count() != 2 {
		t.Fatal("cancelled send executed")
	}
}
