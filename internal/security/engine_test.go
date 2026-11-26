package security

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
)

type mem struct {
	events   []Event
	sessions []identity.Session
}

func (m *mem) InsertSecurityEvent(_ context.Context, e Event) error {
	m.events = append(m.events, e)
	return nil
}
func (m *mem) ListSecurityEvents(_ context.Context, userID string, limit int) ([]Event, error) {
	out := []Event{}
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if m.events[i].UserID == userID {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}
func (m *mem) ListSessions(_ context.Context, _ string) ([]identity.Session, error) {
	return m.sessions, nil
}
func (m *mem) GetSession(_ context.Context, id uuid.UUID) (identity.Session, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return identity.Session{}, ErrNoSession
}
func (m *mem) MarkSessionStepUp(_ context.Context, id uuid.UUID, at time.Time) error {
	for i, s := range m.sessions {
		if s.ID == id {
			m.sessions[i].StepUpAt = &at
			return nil
		}
	}
	return ErrNoSession
}
func (m *mem) RevokeOtherSessions(_ context.Context, _ string, keep uuid.UUID, at time.Time) error {
	for i, s := range m.sessions {
		if s.ID != keep {
			m.sessions[i].RevokedAt = &at
		}
	}
	return nil
}
func (m *mem) TouchSession(_ context.Context, _ uuid.UUID, _, _ string, _ time.Time) error {
	return nil
}

func TestEvaluateDevBearerAllows(t *testing.T) {
	e := &Engine{}
	d := e.Evaluate(context.Background(), Request{Action: ActionTokenRead, UserID: "u", Passkeys: 0})
	if d.Outcome != OutcomeAllow {
		t.Fatalf("got %s", d.Outcome)
	}
}

func TestEvaluateLastPasskeyDenied(t *testing.T) {
	e := &Engine{}
	d := e.Evaluate(context.Background(), Request{
		Action: ActionDeletePasskey, UserID: "u", Passkeys: 1, DeletingID: uuid.New(),
	})
	if d.Outcome != OutcomeDeny {
		t.Fatalf("got %s", d.Outcome)
	}
}

func TestEvaluatePasskeySessionNeedsStepUp(t *testing.T) {
	e := &Engine{Now: func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }}
	sess := &identity.Session{ID: uuid.New(), UserID: "u"}
	d := e.Evaluate(context.Background(), Request{
		Action: ActionTokenRead, UserID: "u", Session: sess, Passkeys: 1,
	})
	if d.Outcome != OutcomeStepUp {
		t.Fatalf("got %s", d.Outcome)
	}
	at := e.Now().Add(-time.Minute)
	sess.StepUpAt = &at
	d = e.Evaluate(context.Background(), Request{
		Action: ActionTokenRead, UserID: "u", Session: sess, Passkeys: 1,
	})
	if d.Outcome != OutcomeAllow {
		t.Fatalf("fresh step-up got %s", d.Outcome)
	}
}

func TestConsoleAndRevokeOthers(t *testing.T) {
	keep := uuid.New()
	other := uuid.New()
	st := &mem{sessions: []identity.Session{
		{ID: keep, UserID: "u", CreatedAt: time.Now()},
		{ID: other, UserID: "u", CreatedAt: time.Now()},
	}}
	e := &Engine{Store: st}
	if err := e.RevokeOthers(context.Background(), "u", keep); err != nil {
		t.Fatal(err)
	}
	c, err := e.Console(context.Background(), "u", &identity.Session{ID: keep})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sessions) != 1 || !c.Sessions[0].Current {
		t.Fatalf("sessions %+v", c.Sessions)
	}
}
