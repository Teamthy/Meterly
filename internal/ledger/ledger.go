// Package ledger implements a double-entry ledger. Every posting must
// balance per currency, postings are idempotent by reference, and posted
// entries are never edited (corrections use reversal postings).
package ledger

import (
	"errors"
	"fmt"
	"sync"
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
	ErrRefConflict = errors.New("ledger: reference already posted with different entries")
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

// Ledger is an in-memory, append-only ledger. A PostgreSQL implementation
// must enforce the same rules inside a database transaction.
type Ledger struct {
	mu    sync.Mutex
	txns  []Transaction
	byRef map[string]int
}

func New() *Ledger { return &Ledger{byRef: map[string]int{}} }

// Post records a balanced transaction. Posting the same reference twice is
// a no-op returning the original.
func (l *Ledger) Post(t Transaction) (Transaction, error) {
	if err := Validate(t.Entries); err != nil {
		return Transaction{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if i, ok := l.byRef[t.Reference]; ok {
		return l.txns[i], nil
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.PostedAt.IsZero() {
		t.PostedAt = time.Now().UTC()
	}
	t.Entries = append([]Entry(nil), t.Entries...)
	l.byRef[t.Reference] = len(l.txns)
	l.txns = append(l.txns, t)
	return t, nil
}

// Balance returns debits minus credits for an account.
func (l *Ledger) Balance(acct AccountCode, c money.Currency) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var v int64
	for _, t := range l.txns {
		for _, e := range t.Entries {
			if e.Account != acct || e.Amount.Currency != c {
				continue
			}
			if e.Direction == Debit {
				v += e.Amount.Minor
			} else {
				v -= e.Amount.Minor
			}
		}
	}
	return v
}

// BySource returns postings for a source record (e.g. one purchase).
func (l *Ledger) BySource(id uuid.UUID) []Transaction {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Transaction
	for _, t := range l.txns {
		if t.SourceID == id {
			out = append(out, t)
		}
	}
	return out
}

// TrialBalance returns total debits minus credits across the ledger; it
// must always be zero.
func (l *Ledger) TrialBalance() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var v int64
	for _, t := range l.txns {
		for _, e := range t.Entries {
			if e.Direction == Debit {
				v += e.Amount.Minor
			} else {
				v -= e.Amount.Minor
			}
		}
	}
	return v
}
