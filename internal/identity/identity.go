// Package identity is Meterly's first-party authenticator: passkeys
// (platform WebAuthn / device biometrics), sessions, profile and
// stateful onboarding.
package identity

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	StepWelcome = "welcome"
	StepAccount = "account"
	StepProfile = "profile"
	StepMeter   = "meter"
	StepAlerts  = "alerts"
	StepDone    = "done"

	SessionPrefix = "mls_"
	SessionTTL    = 30 * 24 * time.Hour
	ChallengeTTL  = 5 * time.Minute
)

var Steps = []string{StepWelcome, StepAccount, StepProfile, StepMeter, StepAlerts, StepDone}

var (
	ErrInvalidStep       = errors.New("identity: invalid onboarding step")
	ErrStepOrder         = errors.New("identity: cannot skip onboarding steps")
	ErrBadAttestation    = errors.New("identity: invalid passkey attestation")
	ErrBadAssertion      = errors.New("identity: invalid passkey assertion")
	ErrUnknownCredential = errors.New("identity: unknown passkey")
	ErrChallenge         = errors.New("identity: challenge expired or already used")
	ErrOrigin            = errors.New("identity: origin is not allowed")
	ErrClonedKey         = errors.New("identity: passkey sign count did not increase")
	ErrNoPasskeys        = errors.New("identity: passkeys are not configured")
	ErrSession           = errors.New("identity: session expired or unknown")
)

type CredentialView struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	SignCount  uint32    `json:"sign_count"`
}

type Profile struct {
	ID                    string     `json:"user_id"`
	ExternalID            string     `json:"external_id"`
	DisplayName           string     `json:"display_name"`
	EmailMasked           string     `json:"email_masked"`
	PhoneMasked           string     `json:"phone_masked"`
	OnboardingStep        string     `json:"onboarding_step"`
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

type Credential struct {
	ID           uuid.UUID `json:"id"`
	UserID       string    `json:"user_id"`
	CredentialID []byte    `json:"-"`
	PublicKey    []byte    `json:"-"` // X||Y, 64 bytes P-256
	SignCount    uint32    `json:"sign_count"`
	AAGUID       []byte    `json:"-"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   time.Time `json:"last_used_at"`
}

type Challenge struct {
	ID        uuid.UUID
	Challenge []byte
	Purpose   string
	UserID    string
	ExpiresAt time.Time
}

type Session struct {
	ID        uuid.UUID
	UserID    string
	TokenHMAC string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

func NextStep(current string) (string, error) {
	if current == "" {
		current = StepWelcome
	}
	for i, s := range Steps {
		if s == current {
			if i == len(Steps)-1 {
				return StepDone, nil
			}
			return Steps[i+1], nil
		}
	}
	return "", ErrInvalidStep
}

func ValidStep(step string) bool {
	for _, s := range Steps {
		if s == step {
			return true
		}
	}
	return false
}

func CanAdvance(from, to string) bool {
	if from == "" {
		from = StepWelcome
	}
	if !ValidStep(from) || !ValidStep(to) {
		return false
	}
	if to == from {
		return true
	}
	next, err := NextStep(from)
	return err == nil && next == to
}
