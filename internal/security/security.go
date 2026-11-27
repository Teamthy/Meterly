// Package security is Meterly's customer security engine: risk decisions,
// step-up (passkey) for sensitive actions, session revoke, and an
// append-only event log that never stores tokens, meter numbers or contacts.
package security

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
)

const (
	ActionLogin         = "LOGIN"
	ActionRegister      = "REGISTER"
	ActionStepUp        = "STEP_UP"
	ActionTokenRead     = "TOKEN_READ"
	ActionSaveCard      = "SAVE_CARD"
	ActionDeletePasskey = "DELETE_PASSKEY"
	ActionAddPasskey    = "ADD_PASSKEY"
	ActionRevokeSession = "REVOKE_SESSION"
	ActionRevokeOthers  = "REVOKE_OTHERS"
	ActionChangeContact = "CHANGE_CONTACT"
	ActionDataExport    = "DATA_EXPORT"
	ActionDeleteAccount = "DELETE_ACCOUNT"

	OutcomeAllow    = "ALLOW"
	OutcomeStepUp   = "STEP_UP"
	OutcomeDeny     = "DENY"
	OutcomeRecorded = "RECORDED"

	DefaultStepUpAfter = 15 * time.Minute
)

var (
	ErrStepUp      = errors.New("security: passkey step-up required")
	ErrDenied      = errors.New("security: action denied")
	ErrLastPasskey = errors.New("security: cannot delete the last passkey")
	ErrNoSession   = errors.New("security: no passkey session")
)

type Event struct {
	ID        uuid.UUID `json:"id"`
	UserID    string    `json:"-"`
	Action    string    `json:"action"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason"`
	SessionID uuid.UUID `json:"session_id,omitempty"`
	IPHMAC    string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

type SessionView struct {
	ID         uuid.UUID `json:"id"`
	Current    bool      `json:"current"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	SteppedUp  bool      `json:"stepped_up"`
}

type Request struct {
	Action     string
	UserID     string
	Session    *identity.Session // nil = bearer is not a Meterly passkey session
	Passkeys   int
	DeletingID uuid.UUID
	IP         string
}

type Decision struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Action  string `json:"action"`
}

type Line struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Emphasis bool   `json:"emphasis,omitempty"`
}

type Console struct {
	Title    string        `json:"title"`
	Subtitle string        `json:"subtitle"`
	Copy     string        `json:"copy"`
	Lines    []Line        `json:"lines"`
	Sessions []SessionView `json:"sessions"`
	Events   []Event       `json:"events"`
}
