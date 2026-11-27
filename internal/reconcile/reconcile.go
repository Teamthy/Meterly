// Package reconcile is the daily settlement and exception engine (P2).
//
// Each run is idempotent per (provider, Africa/Lagos business date). Provider
// HTTP happens outside InTx; exceptions, settlement postings and the run row
// commit together. Re-running a completed day is a no-op. Corrections never
// post to the ledger implicitly: an operator proposes an adjusting entry and
// a different admin approves it.
package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/ledger"
)

const (
	ProviderPaystack = "paystack"
	ProviderVTPass   = "vtpass"
)

type Category string

const (
	CatInPSPNotOurs         Category = "IN_PSP_NOT_OURS"
	CatInOursNotPSP         Category = "IN_OURS_NOT_PSP"
	CatAmountMismatch       Category = "AMOUNT_MISMATCH"
	CatCurrencyMismatch     Category = "CURRENCY_MISMATCH"
	CatStatusMismatch       Category = "STATUS_MISMATCH"
	CatRefundMismatch       Category = "REFUND_MISMATCH"
	CatTokenMissing         Category = "TOKEN_MISSING"
	CatSettlementUnbalanced Category = "SETTLEMENT_UNBALANCED"
)

type Severity string

const (
	SeverityHigh   Severity = "HIGH"
	SeverityMedium Severity = "MEDIUM"
	SeverityLow    Severity = "LOW"
)

func SeverityOf(c Category) Severity {
	switch c {
	case CatAmountMismatch, CatStatusMismatch, CatRefundMismatch, CatTokenMissing, CatSettlementUnbalanced:
		return SeverityHigh
	case CatInPSPNotOurs, CatInOursNotPSP:
		return SeverityMedium
	default:
		return SeverityLow
	}
}

type RunStatus string

const (
	RunRunning   RunStatus = "RUNNING"
	RunCompleted RunStatus = "COMPLETED"
	RunFailed    RunStatus = "FAILED"
)

type ExceptionStatus string

const (
	StatusOpen          ExceptionStatus = "OPEN"
	StatusInvestigating ExceptionStatus = "INVESTIGATING"
	StatusResolved      ExceptionStatus = "RESOLVED"
	StatusWrittenOff    ExceptionStatus = "WRITTEN_OFF"
)

type AdjustmentStatus string

const (
	AdjProposed AdjustmentStatus = "PROPOSED"
	AdjApproved AdjustmentStatus = "APPROVED"
	AdjRejected AdjustmentStatus = "REJECTED"
)

type Run struct {
	ID             uuid.UUID
	Provider       string
	BusinessDate   time.Time // date-only, WAT
	Status         RunStatus
	StartedAt      time.Time
	CompletedAt    time.Time
	MatchedCount   int
	MatchedMinor   int64
	ExceptionCount int
	Error          string
}

type Exception struct {
	ID           uuid.UUID
	RunID        uuid.UUID
	Provider     string
	BusinessDate time.Time
	Category     Category
	Severity     Severity
	ProviderRef  string
	OurTxnID     uuid.UUID
	OursMinor    int64
	TheirsMinor  int64
	Currency     string
	Status       ExceptionStatus
	Owner        string
	Notes        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Adjustment struct {
	ID              uuid.UUID
	ExceptionID     uuid.UUID
	ProposedBy      string
	ApprovedBy      string
	Status          AdjustmentStatus
	DebitAccount    ledger.AccountCode
	CreditAccount   ledger.AccountCode
	AmountMinor     int64
	Currency        string
	LedgerReference string
	Reason          string
	CreatedAt       time.Time
	ApprovedAt      time.Time
}

type ExceptionFilter struct {
	Provider     string
	Status       ExceptionStatus
	Category     Category
	BusinessDate time.Time
	Limit        int
}

type OpenCount struct {
	Provider string
	Category Category
	N        int64
}

type Summary struct {
	BusinessDate    time.Time      `json:"business_date"`
	Runs            []Run          `json:"runs"`
	ExceptionsByCat map[string]int `json:"exceptions_by_category"`
	OpenCount       int            `json:"open_count"`
	MatchedCount    int            `json:"matched_count"`
	MatchedMinor    int64          `json:"matched_minor"`
	ReceivableMinor int64          `json:"psp_receivable_minor"`
}

var (
	ErrNotFound       = errors.New("reconcile: not found")
	ErrSameApprover   = errors.New("reconcile: proposer cannot approve")
	ErrNotProposed    = errors.New("reconcile: adjustment is not proposed")
	ErrUnknownAccount = errors.New("reconcile: unknown ledger account")
	ErrNoSource       = errors.New("reconcile: no statement source for provider")
	ErrBadStatus      = errors.New("reconcile: invalid status")
	// ErrIdempotencyKey: money-recording operator actions need a 16–128
	// character Idempotency-Key so a retry or double click posts once.
	ErrIdempotencyKey = errors.New("reconcile: Idempotency-Key header (16–128 chars) is required")
	// ErrIdempotencyConflict: the key was already used for a different amount.
	ErrIdempotencyConflict = errors.New("reconcile: Idempotency-Key was already used for a different top-up")
)

// Fingerprint is the idempotency key for an exception across re-runs of the
// same (provider, business date).
func (e Exception) Fingerprint() string {
	txn := "00000000-0000-0000-0000-000000000000"
	if e.OurTxnID != uuid.Nil {
		txn = e.OurTxnID.String()
	}
	return e.Provider + "|" + DateOnly(e.BusinessDate).Format("2006-01-02") + "|" + string(e.Category) + "|" + e.ProviderRef + "|" + txn
}

// Repo is the persistence for runs, exceptions and adjustments.
type Repo interface {
	ClaimRun(ctx context.Context, provider string, date time.Time, id uuid.UUID) (Run, error)
	GetRun(ctx context.Context, provider string, date time.Time) (Run, error)
	CompleteRun(ctx context.Context, run Run) error
	FailRun(ctx context.Context, id uuid.UUID, errText string) error
	UpsertException(ctx context.Context, e *Exception) error
	GetException(ctx context.Context, id uuid.UUID) (Exception, error)
	ListExceptions(ctx context.Context, f ExceptionFilter) ([]Exception, error)
	UpdateException(ctx context.Context, e Exception) error
	CountOpen(ctx context.Context) ([]OpenCount, error)
	CreateAdjustment(ctx context.Context, a *Adjustment) error
	GetAdjustment(ctx context.Context, id uuid.UUID) (Adjustment, error)
	UpdateAdjustment(ctx context.Context, a Adjustment) error
}

// KnownAccount reports whether code is on the chart of accounts.
func KnownAccount(code ledger.AccountCode) bool {
	switch code {
	case ledger.PSPSettlementReceivable, ledger.PotFundsHeld, ledger.CustomerFundsClearing,
		ledger.ProviderPayable, ledger.RefundPayable, ledger.PlatformFeeRevenue,
		ledger.CashAtBank, ledger.PSPFeesExpense, ledger.VendorWalletPrefund, ledger.VendorCommissionIncome:
		return true
	}
	return false
}

// WAT is Africa/Lagos without tzdata (WAT, UTC+1, no DST).
var WAT = time.FixedZone("WAT", 3600)

func BusinessDate(now time.Time) time.Time {
	w := now.UTC().Add(time.Hour)
	y, m, d := w.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, WAT)
}

func DayBounds(date time.Time) (from, to time.Time) {
	d := BusinessDate(date)
	return d.UTC(), d.Add(24 * time.Hour).UTC()
}

func DateOnly(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return BusinessDate(t)
}
