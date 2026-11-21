// Package ledger defines double-entry ledger types and rules. Every posting
// must balance per currency, postings are idempotent by reference, and
// posted entries are never edited (corrections use reversal postings).
// Storage lives in internal/store; PostgreSQL additionally enforces balance
// and immutability with triggers.
package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
)

type AccountCode string

// Illustrative chart of accounts (spec §9.5). The final structure must be
// agreed with finance based on the contractual money flow.
const (
	PSPSettlementReceivable AccountCode = "PSP_SETTLEMENT_RECEIVABLE" // asset
	CustomerFundsClearing   AccountCode = "CUSTOMER_FUNDS_CLEARING"   // liability
	ProviderPayable         AccountCode = "PROVIDER_PAYABLE"          // liability
	RefundPayable           AccountCode = "REFUND_PAYABLE"            // liability
	PlatformFeeRevenue      AccountCode = "PLATFORM_FEE_REVENUE"      // income
)

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type Entry struct {
	Account   AccountCode
	Direction Direction
	Amount    money.Amount
}

type Transaction struct {
	ID          uuid.UUID
	Reference   string // unique; makes posting idempotent
	SourceType  string
	SourceID    uuid.UUID
	Description string
	PostedAt    time.Time
	Entries     []Entry
}

var (
	ErrUnbalanced  = errors.New("ledger: posting does not balance")
	ErrEmpty       = errors.New("ledger: posting needs at least two entries")
	ErrNonPositive = errors.New("ledger: entry amounts must be positive")
)

// Validate checks that debits equal credits for each currency.
func Validate(entries []Entry) error {
	if len(entries) < 2 {
		return ErrEmpty
	}
	bal := map[money.Currency]int64{}
	for _, e := range entries {
		if e.Amount.Minor <= 0 {
			return ErrNonPositive
		}
		switch e.Direction {
		case Debit:
			bal[e.Amount.Currency] += e.Amount.Minor
		case Credit:
			bal[e.Amount.Currency] -= e.Amount.Minor
		default:
			return fmt.Errorf("ledger: bad direction %q", e.Direction)
		}
	}
	for c, v := range bal {
		if v != 0 {
			return fmt.Errorf("%w: %s off by %d", ErrUnbalanced, c, v)
		}
	}
	return nil
}
