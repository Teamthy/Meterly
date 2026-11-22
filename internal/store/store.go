// Package store defines persistence contracts. All writes that belong
// together — a state transition, its history, its ledger posting and its
// outbox event — run inside one InTx call so they commit or roll back as a
// unit. Provider (network) calls must never happen inside InTx.
//
// Implementations: store/memstore (tests, local dev) and store/pgstore
// (PostgreSQL via pgx).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notificationintents"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: concurrent update")
	ErrDuplicate = errors.New("store: duplicate")
)

type Store interface {
	// InTx runs fn in a single database transaction. If fn returns an
	// error, every write made through the passed Repos is rolled back.
	InTx(ctx context.Context, fn func(Repos) error) error
	// Repos returns auto-commit repositories for reads and single writes.
	Repos() Repos
	Close()
}

type Repos interface {
	Users() UserRepo
	Meters() MeterRepo
	Quotes() QuoteRepo
	Transactions() TransactionRepo
	Ledger() LedgerRepo
	Outbox() OutboxRepo
	Idempotency() IdempotencyRepo
	Audit() AuditRepo
	Providers() ProviderRepo
	TokenShares() TokenShareRepo
	Gifts() GiftRepo
	NotificationCandidates() NotificationCandidateRepo
}

type AuditRepo interface {
	Add(ctx context.Context, e audit.Event) error
	ForResource(ctx context.Context, resourceType string, id uuid.UUID) ([]audit.Event, error)
}

// ProviderRepo holds operator switches for integrations. A provider with
// no record is enabled.
type ProviderRepo interface {
	Enabled(ctx context.Context, name string) (bool, error)
	SetEnabled(ctx context.Context, name, providerType string, enabled bool) error
}

// TxnFilter selects transactions for operators. Zero values match all.
type TxnFilter struct {
	ReviewOnly bool
	Payment    tx.PaymentState
	Vending    tx.VendingState
	Delivery   tx.DeliveryState
	CustomerID string
	MeterID    uuid.UUID
	Before     time.Time // created strictly before (keyset pagination)
	Limit      int
}

type UserRepo interface {
	// EnsureByExternalID maps an identity-provider subject to an internal
	// user ID (UUID string), creating the user on first sight.
	EnsureByExternalID(ctx context.Context, externalID string) (string, error)
}

type MeterRepo interface {
	Get(ctx context.Context, id uuid.UUID) (*meters.Meter, error)
	// GetForUpdate serializes registration and self-removal for this meter.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*meters.Meter, error)
	GetByHash(ctx context.Context, hash string) (*meters.Meter, error)
	// Insert returns ErrDuplicate if a meter with the same hash exists.
	Insert(ctx context.Context, m *meters.Meter) error
	Update(ctx context.Context, m *meters.Meter) error
	// UpsertGrant creates an active grant, or raises the permission and
	// updates the label of an existing active grant. Never lowers it.
	// Grant-changing workflows must hold the meter lock in the same InTx
	// before calling this method, to serialize with purchase authorization.
	UpsertGrant(ctx context.Context, g meters.Grant) error
	ActiveGrant(ctx context.Context, meterID uuid.UUID, userID string) (*meters.Grant, error)
	ListForUser(ctx context.Context, userID string) ([]meters.Listed, error)
	// Revoke callers must hold the meter lock in the surrounding InTx.
	Revoke(ctx context.Context, meterID uuid.UUID, userID string, at time.Time) error
}

type QuoteRepo interface {
	Create(ctx context.Context, q *tx.Quote) error
	Get(ctx context.Context, id uuid.UUID) (*tx.Quote, error)
	// GetForUpdate locks the quote until the surrounding transaction ends.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Quote, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
}

type TransactionRepo interface {
	Create(ctx context.Context, t *tx.Transaction) error
	Get(ctx context.Context, id uuid.UUID) (*tx.Transaction, error)
	// GetForUpdate locks the row until the surrounding transaction ends.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Transaction, error)
	ByPaymentRef(ctx context.Context, ref string) (*tx.Transaction, error)
	// Save persists t if nobody saved since it was loaded (optimistic
	// concurrency), appends new history, and increments t.Version.
	Save(ctx context.Context, t *tx.Transaction) error
	ListForCustomer(ctx context.Context, customerID string) ([]*tx.Transaction, error)
	// Search returns matches newest first.
	Search(ctx context.Context, f TxnFilter) ([]*tx.Transaction, error)
	// ClaimDue returns unsettled transactions whose NextCheckAt <= now and
	// that have not changed since staleBefore, and pushes their NextCheckAt
	// to now+lease so concurrent workers skip them.
	ClaimDue(ctx context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)
}

type LedgerRepo interface {
	// Post validates and records a balanced posting. Posting an existing
	// reference again is a no-op that returns the original.
	Post(ctx context.Context, t ledger.Transaction) (ledger.Transaction, error)
	BySource(ctx context.Context, sourceID uuid.UUID) ([]ledger.Transaction, error)
	Balance(ctx context.Context, acct ledger.AccountCode, c money.Currency) (int64, error)
	TrialBalance(ctx context.Context) (int64, error)
}

type OutboxRepo interface {
	Add(ctx context.Context, e events.Event) error
	// ClaimUnpublished locks up to limit unpublished events (skipping ones
	// locked by other workers) for the surrounding transaction.
	ClaimUnpublished(ctx context.Context, limit int) ([]events.Event, error)
	MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error
	ForAggregate(ctx context.Context, id uuid.UUID) ([]events.Event, error)
}

type IdempotencyRepo interface {
	// Reserve inserts rec. If an unexpired record exists for the same
	// scope/operation/key, it returns that record instead (and inserts
	// nothing). Expired records are replaced.
	Reserve(ctx context.Context, rec idempotency.Record) (existing *idempotency.Record, err error)
	Complete(ctx context.Context, scope, op, key, resultRef string) error
}

// TokenShareRepo persists metadata only, never token values or destinations.
type TokenShareRepo interface {
	AddCopy(context.Context, tokenshares.Share) error
	GetCopy(context.Context, uuid.UUID, uuid.UUID, string) (tokenshares.Share, error)
	ListCopies(context.Context, uuid.UUID, string) ([]tokenshares.Share, error)
}

// GiftRepo stores encrypted contact metadata; no raw recipient contact.
type GiftRepo interface {
	Create(context.Context, gifts.Gift) error
	Get(context.Context, uuid.UUID) (gifts.Gift, error)
}

// NotificationCandidateRepo is a held, non-deliverable record of relevant
// outbox events. Claim and Record must run within the same Store.InTx.
type NotificationCandidateRepo interface {
	Claim(context.Context, int) ([]notificationintents.Intent, error)
	Record(context.Context, notificationintents.Intent) error
	Get(context.Context, uuid.UUID) (notificationintents.Intent, error)
}
