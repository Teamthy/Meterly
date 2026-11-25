// Package pgstore implements store.Store on PostgreSQL using pgx v5.
//
// Locking model: InTx uses READ COMMITTED; aggregates read for change are
// locked with SELECT ... FOR UPDATE (GetForUpdate), and Save additionally
// checks the version column (optimistic concurrency) so a writer that did
// not lock still cannot overwrite a newer state. Worker claims use
// FOR UPDATE SKIP LOCKED.
package pgstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

// DB is satisfied by *pgxpool.Pool and pgx.Tx.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool        *pgxpool.Pool
	environment string // providers.environment: SANDBOX or PRODUCTION
	refs        sync.Map
}

// New wraps a pool. environment is recorded on provider rows.
func New(pool *pgxpool.Pool, environment string) *Store {
	return &Store{pool: pool, environment: environment}
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }
func (s *Store) Close()              { s.pool.Close() }

func (s *Store) MetricsSnapshot(ctx context.Context, now time.Time) (store.MetricsSnapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			CASE
				WHEN review_reason <> '' THEN 'MANUAL_REVIEW'
				WHEN payment_status IN ('CREATED', 'PENDING') THEN 'PAYMENT_PENDING'
				WHEN payment_status = 'FAILED' THEN 'FAILED'
				WHEN payment_status = 'REFUND_PENDING' THEN 'REFUND_PENDING'
				WHEN payment_status = 'REFUNDED' THEN 'REFUNDED'
				WHEN vending_status IN ('NOT_STARTED', 'PENDING') THEN 'VENDING_PENDING'
				WHEN vending_status = 'UNKNOWN' THEN 'RECONCILIATION_PENDING'
				WHEN vending_status = 'FAILED' THEN 'MANUAL_REVIEW'
				WHEN delivery_status = 'CONFIRMED' THEN 'COMPLETED'
				WHEN delivery_status = 'FAILED' THEN 'MANUAL_REVIEW'
				WHEN delivery_status = 'UNKNOWN' THEN 'RECONCILIATION_PENDING'
				ELSE 'DELIVERY_PENDING'
			END AS overall_status,
			count(*)
		FROM electricity_transactions
		GROUP BY 1`)
	if err != nil {
		return store.MetricsSnapshot{}, err
	}
	defer rows.Close()

	byStatus := make(map[tx.Overall]int64)
	for rows.Next() {
		var status string
		var cnt int64
		if err := rows.Scan(&status, &cnt); err != nil {
			return store.MetricsSnapshot{}, err
		}
		byStatus[tx.Overall(status)] = cnt
	}
	if err := rows.Err(); err != nil {
		return store.MetricsSnapshot{}, err
	}

	var lag float64
	if err := s.pool.QueryRow(ctx, `
		SELECT coalesce(GREATEST(EXTRACT(EPOCH FROM ($1::timestamptz - min(created_at))), 0), 0)::float8
		FROM outbox_events
		WHERE published_at IS NULL`, now.UTC()).Scan(&lag); err != nil {
		return store.MetricsSnapshot{}, err
	}

	return store.MetricsSnapshot{
		TransactionsByStatus: byStatus,
		ManualReviewOpen:     byStatus[tx.OverallManualReview],
		OutboxLagSeconds:     lag,
	}, nil
}

func (s *Store) InTx(ctx context.Context, fn func(store.Repos) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(t pgx.Tx) error { return fn(&repos{s: s, db: t, inTx: true}) })
}

func (s *Store) Repos() store.Repos { return &repos{s: s, db: s.pool} }

type repos struct {
	s    *Store
	db   DB
	inTx bool
}

// atomic runs fn in the caller's transaction, or in a new one when the
// repos are auto-commit, so multi-statement writes are never partial.
func (r *repos) atomic(ctx context.Context, fn func(db DB) error) error {
	if r.inTx {
		return fn(r.db)
	}
	return pgx.BeginFunc(ctx, r.s.pool, func(t pgx.Tx) error { return fn(t) })
}

func (r *repos) Users() store.UserRepo                                   { return userRepo{r} }
func (r *repos) Meters() store.MeterRepo                                 { return meterRepo{r} }
func (r *repos) Quotes() store.QuoteRepo                                 { return quoteRepo{r} }
func (r *repos) Transactions() store.TransactionRepo                     { return txnRepo{r} }
func (r *repos) Ledger() store.LedgerRepo                                { return ledgerRepo{r} }
func (r *repos) Outbox() store.OutboxRepo                                { return outboxRepo{r} }
func (r *repos) Idempotency() store.IdempotencyRepo                      { return idemRepo{r} }
func (r *repos) Audit() store.AuditRepo                                  { return auditRepo{r} }
func (r *repos) Providers() store.ProviderRepo                           { return providerRepo{r} }
func (r *repos) TokenShares() store.TokenShareRepo                       { return shareRepo{r} }
func (r *repos) Gifts() store.GiftRepo                                   { return giftRepo{r} }
func (r *repos) NotificationCandidates() store.NotificationCandidateRepo { return candidateRepo{r} }
func (r *repos) Notifications() store.NotificationRepo                   { return notifRepo{r} }
func (r *repos) PayRequests() store.PayRequestRepo                       { return payReqRepo{r} }
func (r *repos) Pots() store.PotRepo                                     { return potRepo{r} }
func (r *repos) AutoPower() store.AutoPowerRepo                          { return autoPowerRepo{r} }

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return store.ErrDuplicate
	}
	return err
}

// Reference data (providers, DisCos) is upserted on the pool, outside the
// caller's transaction, so a rollback never leaves a cached ID dangling.
func (r *repos) refID(ctx context.Context, cacheKey, sql string, args ...any) (uuid.UUID, error) {
	if v, ok := r.s.refs.Load(cacheKey); ok {
		return v.(uuid.UUID), nil
	}
	var id uuid.UUID
	if err := r.s.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	r.s.refs.Store(cacheKey, id)
	return id, nil
}

func (r *repos) providerID(ctx context.Context, name, typ string) (uuid.UUID, error) {
	return r.refID(ctx, "provider:"+name, `
		INSERT INTO providers (name, provider_type, environment) VALUES ($1, $2, $3)
		ON CONFLICT (name, environment) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, name, typ, r.s.environment)
}

func (r *repos) discoID(ctx context.Context, code string) (uuid.UUID, error) {
	return r.refID(ctx, "disco:"+code, `
		INSERT INTO distribution_companies (name, code) VALUES ($1, $1)
		ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
		RETURNING id`, code)
}

// nullUUID converts "" to NULL for optional user references.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- users -------------------------------------------------------------------

type userRepo struct{ *repos }

func (r userRepo) EnsureByExternalID(ctx context.Context, ext string) (string, error) {
	var id string
	err := r.db.QueryRow(ctx, `
		INSERT INTO users (external_id) VALUES ($1)
		ON CONFLICT (external_id) DO UPDATE SET external_id = EXCLUDED.external_id
		RETURNING id::text`, ext).Scan(&id)
	return id, mapErr(err)
}

func (r userRepo) LockForUpdate(ctx context.Context, userID string) error {
	if !r.inTx {
		return errors.New("user lock requires transaction")
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	var locked uuid.UUID
	return mapErr(r.db.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, uid).Scan(&locked))
}
