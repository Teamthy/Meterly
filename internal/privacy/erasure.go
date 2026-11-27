package privacy

import (
	"time"

	"github.com/google/uuid"
)

const (
	ErasureCoolingOff = "COOLING_OFF"
	ErasureCancelled  = "CANCELLED"
	ErasureReady      = "READY"
	ErasureRunning    = "RUNNING"
	ErasureCompleted  = "COMPLETED"
	ErasureBlocked    = "BLOCKED"
	ErasureFailed     = "FAILED"

	DefaultErasureCooling  = 7 * 24 * time.Hour
	TombstoneExternalID    = "tombstone:erasure"
	ErasureStepFinancial   = "financial"
	ErasureStepIdentity    = "identity"
	ErasureStepMemberships = "memberships"
	ErasureStepSupport     = "support"
	ErasureStepNotify      = "notify"
	ErasureStepCards       = "cards"
	ErasureStepExports     = "exports"
)

// TombstoneUserID receives financial FKs after erasure. Inserted by 0027.
var TombstoneUserID = uuid.MustParse("00000000-0000-0000-0000-00000000e5ed")

// ErasureSteps is the resumable job order. Each step is idempotent.
var ErasureSteps = []string{
	ErasureStepFinancial,
	ErasureStepIdentity,
	ErasureStepMemberships,
	ErasureStepSupport,
	ErasureStepNotify,
	ErasureStepCards,
	ErasureStepExports,
}

const (
	BlockOpenDispute      = "open_dispute"
	BlockUnresolvedTxn    = "unresolved_transaction"
	BlockPotWithFunds     = "pot_with_funds"
	BlockUnpaidObligation = "unpaid_obligation"
)

// ErasureRequest is one account-deletion job. No free-text reason is stored
// (it would be PII in audit). error is a machine code.
type ErasureRequest struct {
	ID          uuid.UUID
	UserID      string
	Status      string
	DueAt       time.Time
	CancelledAt *time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Step        string
	Error       string
	TombstoneID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func CloneErasure(j ErasureRequest) ErasureRequest {
	out := j
	if j.CancelledAt != nil {
		t := *j.CancelledAt
		out.CancelledAt = &t
	}
	if j.StartedAt != nil {
		t := *j.StartedAt
		out.StartedAt = &t
	}
	if j.FinishedAt != nil {
		t := *j.FinishedAt
		out.FinishedAt = &t
	}
	return out
}

func ErasureOpen(status string) bool {
	switch status {
	case ErasureCoolingOff, ErasureReady, ErasureRunning, ErasureBlocked, ErasureFailed:
		return true
	}
	return false
}

func StepIndex(step string) int {
	if step == "" {
		return -1
	}
	for i, s := range ErasureSteps {
		if s == step {
			return i
		}
	}
	return -1
}
