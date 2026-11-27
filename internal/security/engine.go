package security

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/secure"
)

type Store interface {
	InsertSecurityEvent(ctx context.Context, e Event) error
	ListSecurityEvents(ctx context.Context, userID string, limit int) ([]Event, error)
	ListSessions(ctx context.Context, userID string) ([]identity.Session, error)
	GetSession(ctx context.Context, id uuid.UUID) (identity.Session, error)
	MarkSessionStepUp(ctx context.Context, id uuid.UUID, ipHMAC string, at time.Time) error
	RevokeOtherSessions(ctx context.Context, userID string, keep uuid.UUID, at time.Time) error
	TouchSession(ctx context.Context, id uuid.UUID, ipHMAC, uaHMAC string, at time.Time) error
}

type Engine struct {
	Store       Store
	Identity    *identity.Engine
	Box         *secure.Box
	StepUpAfter time.Duration
	Now         func() time.Time
}

func (e *Engine) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) stepUpAfter() time.Duration {
	if e != nil && e.StepUpAfter > 0 {
		return e.StepUpAfter
	}
	return DefaultStepUpAfter
}

func sensitive(action string) bool {
	switch action {
	case ActionTokenRead, ActionSaveCard, ActionDeletePasskey, ActionAddPasskey, ActionChangeContact, ActionRevokeOthers, ActionRevokeSession:
		return true
	}
	return false
}

// Evaluate is fail-closed on last-passkey delete and fail-open for
// non-passkey bearers (dev/OIDC) so existing staff and sandbox flows work.
func (e *Engine) Evaluate(_ context.Context, req Request) Decision {
	if req.Action == ActionDeletePasskey && req.Passkeys <= 1 {
		return Decision{Outcome: OutcomeDeny, Reason: "last passkey on this account", Action: req.Action}
	}
	if req.Session == nil || req.Passkeys == 0 {
		return Decision{Outcome: OutcomeAllow, Reason: "no passkey session to step up", Action: req.Action}
	}
	if !sensitive(req.Action) {
		return Decision{Outcome: OutcomeAllow, Reason: "not a gated action", Action: req.Action}
	}
	if e != nil && req.Session.IPHMAC != "" && req.IP != "" {
		if got := e.HashIP(req.IP); got != "" && got != req.Session.IPHMAC {
			return Decision{Outcome: OutcomeStepUp, Reason: "session moved to a new network", Action: req.Action}
		}
	}
	fresh := req.Session.StepUpAt != nil && e.now().Sub(req.Session.StepUpAt.UTC()) < e.stepUpAfter()
	if fresh {
		return Decision{Outcome: OutcomeAllow, Reason: "recent passkey step-up", Action: req.Action}
	}
	return Decision{Outcome: OutcomeStepUp, Reason: "confirm this device to continue", Action: req.Action}
}

func (e *Engine) Record(ctx context.Context, ev Event) error {
	if e == nil || e.Store == nil {
		return nil
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = e.now()
	}
	if ev.Outcome == "" {
		ev.Outcome = OutcomeRecorded
	}
	return e.Store.InsertSecurityEvent(ctx, ev)
}

func (e *Engine) Console(ctx context.Context, userID string, current *identity.Session) (Console, error) {
	c := Console{
		Title:    "This device, this account.",
		Subtitle: "Passkeys sign you in. Step-up unlocks tokens and saved cards.",
		Copy:     "Meterly never stores a password. Sessions are hashed. Security events never include tokens, meter numbers or contacts.",
		Sessions: []SessionView{},
		Events:   []Event{},
	}
	if e == nil || e.Store == nil {
		return c, nil
	}
	sessions, err := e.Store.ListSessions(ctx, userID)
	if err != nil {
		return c, err
	}
	active := 0
	for _, s := range sessions {
		if s.RevokedAt != nil {
			continue
		}
		active++
		view := SessionView{
			ID: s.ID, CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, ExpiresAt: s.ExpiresAt,
			SteppedUp: s.StepUpAt != nil && e.now().Sub(s.StepUpAt.UTC()) < e.stepUpAfter(),
		}
		if s.LastSeenAt.IsZero() {
			view.LastSeenAt = s.CreatedAt
		}
		if current != nil && s.ID == current.ID {
			view.Current = true
		}
		c.Sessions = append(c.Sessions, view)
	}
	events, err := e.Store.ListSecurityEvents(ctx, userID, 20)
	if err != nil {
		return c, err
	}
	if events != nil {
		c.Events = events
	}
	passkeys := 0
	if e.Identity != nil {
		if list, err := e.Identity.ListPasskeys(ctx, userID); err == nil {
			passkeys = len(list)
		}
	}
	c.Lines = []Line{
		{Label: "Passkeys", Value: itoa(passkeys), Emphasis: true},
		{Label: "Active sessions", Value: itoa(active)},
		{Label: "Step-up window", Value: e.stepUpAfter().String()},
	}
	return c, nil
}

// CompleteStepUp records a successful passkey step-up and (re)binds the
// session to the network it was confirmed from. Ordinary requests never
// change the bound network (see TouchSession), so a session replayed from
// elsewhere keeps triggering step-up until a passkey is presented there.
func (e *Engine) CompleteStepUp(ctx context.Context, sessionID uuid.UUID, ip string) error {
	if e == nil || e.Store == nil {
		return ErrNoSession
	}
	return e.Store.MarkSessionStepUp(ctx, sessionID, e.HashIP(ip), e.now())
}

func (e *Engine) RevokeOthers(ctx context.Context, userID string, keep uuid.UUID) error {
	if e == nil || e.Store == nil {
		return nil
	}
	return e.Store.RevokeOtherSessions(ctx, userID, keep, e.now())
}

func (e *Engine) HashIP(ip string) string {
	if e == nil || e.Box == nil || ip == "" {
		return ""
	}
	return e.Box.Hash("security-ip:" + ip)
}

func (e *Engine) HashUA(ua string) string {
	if e == nil || e.Box == nil || ua == "" {
		return ""
	}
	return e.Box.Hash("security-ua:" + ua)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
