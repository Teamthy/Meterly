// Package disputes models Paystack chargebacks (P3).
//
// Ledger (finance default; revisit against the merchant contract):
//
//	open  dispute-open:{psp_id}
//	  Dr DISPUTE_LOSS          amount
//	  Cr DISPUTE_RESERVE       amount
//	  Conservative provision: P&L takes the hit while the case is open.
//
//	win   dispute-win:{psp_id}
//	  Dr DISPUTE_RESERVE       amount
//	  Cr DISPUTE_LOSS          amount
//	  Reverses the provision.
//
//	loss  dispute-loss:{psp_id}
//	  Dr DISPUTE_RESERVE              amount
//	  Cr PSP_SETTLEMENT_RECEIVABLE    amount
//	  Reserve clears; the loss stays on DISPUTE_LOSS; the PSP receivable
//	  drops because Paystack will not settle the disputed charge.
//
// References are ledger-idempotent. Evidence packs never include STS tokens.
package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
)

var ErrNotFound = errors.New("disputes: not found")

type Status string

const (
	StatusOpen     Status = "OPEN"
	StatusReminded Status = "REMINDED"
	StatusWon      Status = "WON"
	StatusLost     Status = "LOST"
	StatusAccepted Status = "ACCEPTED"
)

func (s Status) Terminal() bool {
	return s == StatusWon || s == StatusLost || s == StatusAccepted
}

type Dispute struct {
	ID            uuid.UUID
	PSPDisputeID  string
	TransactionID uuid.UUID
	PaymentRef    string
	Amount        money.Amount
	Status        Status
	PSPStatus     string
	Resolution    string
	Category      string
	DueAt         time.Time
	RemindedAt    time.Time
	ResolvedAt    time.Time
	Evidence      json.RawMessage
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Notice struct {
	Type         string // charge.dispute.create|remind|resolve
	PSPDisputeID string
	Reference    string
	AmountMinor  int64
	Currency     string
	PSPStatus    string
	Resolution   string
	Category     string
	DueAt        time.Time
}

type EvidencePack struct {
	TransactionID  string `json:"transaction_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	PaymentStatus  string `json:"payment_status"`
	VendingStatus  string `json:"vending_status"`
	DeliveryStatus string `json:"delivery_status"`
	DeliveryMethod string `json:"delivery_method"`
	CreatedAt      string `json:"created_at,omitempty"`
	PaidNote       string `json:"payment_confirmed,omitempty"`
	VendNote       string `json:"vending_confirmed,omitempty"`
	DeliveryNote   string `json:"delivery_confirmed,omitempty"`
	HasToken       bool   `json:"token_issued"`
}

type Filter struct {
	Status Status
	Limit  int
}

type Repo interface {
	Upsert(ctx context.Context, d Dispute) (Dispute, error)
	Get(ctx context.Context, id uuid.UUID) (Dispute, error)
	ByPSP(ctx context.Context, pspID string) (Dispute, error)
	List(ctx context.Context, f Filter) ([]Dispute, error)
	CountLostForUser(ctx context.Context, userID string) (int, error)
	SetEvidence(ctx context.Context, id uuid.UUID, raw json.RawMessage) error
}

func OpenPosting(d Dispute, now time.Time) ledger.Transaction {
	amt := d.Amount
	return ledger.Transaction{
		Reference:   "dispute-open:" + d.PSPDisputeID,
		SourceType:  "dispute",
		SourceID:    d.ID,
		Description: "Dispute opened; provision reserve",
		PostedAt:    now,
		Entries: []ledger.Entry{
			{Account: ledger.DisputeLoss, Direction: ledger.Debit, Amount: amt},
			{Account: ledger.DisputeReserve, Direction: ledger.Credit, Amount: amt},
		},
	}
}

func WinPosting(d Dispute, now time.Time) ledger.Transaction {
	amt := d.Amount
	return ledger.Transaction{
		Reference:   "dispute-win:" + d.PSPDisputeID,
		SourceType:  "dispute",
		SourceID:    d.ID,
		Description: "Dispute won; release reserve",
		PostedAt:    now,
		Entries: []ledger.Entry{
			{Account: ledger.DisputeReserve, Direction: ledger.Debit, Amount: amt},
			{Account: ledger.DisputeLoss, Direction: ledger.Credit, Amount: amt},
		},
	}
}

func LossPosting(d Dispute, now time.Time) ledger.Transaction {
	amt := d.Amount
	return ledger.Transaction{
		Reference:   "dispute-loss:" + d.PSPDisputeID,
		SourceType:  "dispute",
		SourceID:    d.ID,
		Description: "Dispute lost; PSP clawback",
		PostedAt:    now,
		Entries: []ledger.Entry{
			{Account: ledger.DisputeReserve, Direction: ledger.Debit, Amount: amt},
			{Account: ledger.PSPSettlementReceivable, Direction: ledger.Credit, Amount: amt},
		},
	}
}

// OutcomeFromResolution maps Paystack's documented resolution values.
// merchant-accepted → we lost (accepted the chargeback).
// declined → we won (the issuer declined the claim).
func OutcomeFromResolution(resolution, pspStatus string) Status {
	r := resolution
	switch r {
	case "merchant-accepted":
		return StatusAccepted
	case "declined":
		return StatusWon
	}
	switch pspStatus {
	case "resolved":
		if r == "" {
			return StatusLost
		}
	}
	return ""
}
