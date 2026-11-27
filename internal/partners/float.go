package partners

import (
	"time"

	"github.com/google/uuid"
)

const (
	TopupPending  = "PENDING"
	TopupApproved = "APPROVED"
	TopupRejected = "REJECTED"
)

// UserExternalID is the users.external_id for the synthetic account that
// owns a partner's quotes and purchases (FK on quotes/electricity_transactions).
func UserExternalID(id uuid.UUID) string {
	return "partner:" + id.String()
}

// Float is the per-partner subledger for PARTNER_FLOAT. available_minor is
// what may still be debited; the CoA account is the sum across partners.
type Float struct {
	PartnerID      uuid.UUID
	AvailableMinor int64
	Currency       string
	Version        int
	UpdatedAt      time.Time
}

type FloatTopup struct {
	ID              uuid.UUID
	PartnerID       uuid.UUID
	ReceiptRef      string
	AmountMinor     int64
	Currency        string
	Status          string
	ProposedBy      string
	ProposedAt      time.Time
	ApprovedBy      string
	ApprovedAt      *time.Time
	RejectedBy      string
	RejectedAt      *time.Time
	Reason          string
	LedgerReference string
}
