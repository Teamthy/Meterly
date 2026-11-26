// Package support is Meterly customer-support chat: a server-side agent
// that can look up the caller's own purchases, and an end-to-end encrypted
// channel to a human operator. The server never holds HUMAN message
// plaintext or device private keys.
package support

import (
	"time"

	"github.com/google/uuid"
)

const AlgorithmP256 = "P-256"

type ThreadMode string

const (
	ModeAgent ThreadMode = "AGENT"
	ModeHuman ThreadMode = "HUMAN"
)

type ThreadStatus string

const (
	StatusOpen         ThreadStatus = "OPEN"
	StatusWaitingHuman ThreadStatus = "WAITING_HUMAN"
	StatusAssigned     ThreadStatus = "ASSIGNED"
	StatusResolved     ThreadStatus = "RESOLVED"
	StatusClosed       ThreadStatus = "CLOSED"
)

type SenderKind string

const (
	SenderCustomer SenderKind = "CUSTOMER"
	SenderOperator SenderKind = "OPERATOR"
	SenderAgent    SenderKind = "AGENT"
	SenderSystem   SenderKind = "SYSTEM"
)

const (
	MaxBodyRunes       = 2000
	MaxCiphertextBytes = 16 << 10
	MaxPublicKeyBytes  = 200
	MinPublicKeyBytes  = 50
	DefaultListLimit   = 80
	MaxListLimit       = 200
)

type IdentityKey struct {
	UserID    string
	PublicKey []byte
	Algorithm string
	Staff     bool
	CreatedAt time.Time
	RotatedAt time.Time
}

type Thread struct {
	ID                 uuid.UUID
	CustomerID         string
	Mode               ThreadMode
	Status             ThreadStatus
	AssignedOperatorID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastMessageAt      time.Time
}

func (t Thread) AcceptsAgent() bool {
	return t.Mode == ModeAgent && (t.Status == StatusOpen || t.Status == StatusWaitingHuman)
}

func (t Thread) AcceptsHuman() bool {
	return t.Mode == ModeHuman && (t.Status == StatusWaitingHuman || t.Status == StatusAssigned)
}

func (t Thread) Closed() bool {
	return t.Status == StatusClosed || t.Status == StatusResolved
}

type Message struct {
	ID         uuid.UUID
	ThreadID   uuid.UUID
	SenderID   string
	SenderKind SenderKind
	Ciphertext []byte
	Nonce      []byte
	BodyEnc    []byte
	CreatedAt  time.Time
}

func (m Message) EndToEnd() bool {
	return len(m.Ciphertext) > 0
}

type WrappedKey struct {
	ThreadID     uuid.UUID
	HolderUserID string
	WrappedKey   []byte
	Nonce        []byte
	CreatedAt    time.Time
}
