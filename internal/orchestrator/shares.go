package orchestrator

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const opRecordTokenCopy = "record_token_copy"

// RecordTokenCopy records a client-reported copy action, not actual delivery.
// Only the purchaser can record it, and never for a still-remote transaction.
// Reservation, record and audit are atomic; retries with the same key return
// the original metadata. The STS token itself is never accessed or recorded.
func (o *Orchestrator) RecordTokenCopy(ctx context.Context, user string, id uuid.UUID, key string) (tokenshares.Share, error) {
	var result tokenshares.Share
	if user == "" || id == uuid.Nil || len(key) < 16 || len(key) > 128 {
		return result, errors.New("invalid copy request")
	}
	now := o.Now().UTC()
	err := o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Transactions().GetForUpdate(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && t.CustomerID != user) {
			return tx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
			return ErrTokenUnavailable
		}
		requestHash := idempotency.HashRequest([]byte(id.String() + ":COPY"))
		existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{Scope: user, Operation: opRecordTokenCopy, Key: key, RequestHash: requestHash, CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL)})
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RequestHash != requestHash {
				return idempotency.ErrConflict
			}
			if existing.ResultRef == "" {
				return idempotency.ErrInProgress
			}
			sid, err := uuid.Parse(existing.ResultRef)
			if err != nil {
				return err
			}
			result, err = r.TokenShares().GetCopy(ctx, sid, id, user)
			return err
		}
		result = tokenshares.Share{ID: uuid.New(), TransactionID: id, UserID: user, Channel: tokenshares.Copy, SharedAt: now}
		if err := r.TokenShares().AddCopy(ctx, result); err != nil {
			return err
		}
		actor := audit.Actor{UserID: user, Type: audit.ActorUser, CorrelationID: audit.CorrelationFrom(ctx)}
		if err := r.Audit().Add(ctx, actor.Event("token.copy_recorded", resourceTxn, id, map[string]any{"channel": "COPY"})); err != nil {
			return err
		}
		if err := emit(ctx, r, "token.copy.recorded", id, map[string]any{"channel": "COPY"}); err != nil {
			return err
		}
		return r.Idempotency().Complete(ctx, user, opRecordTokenCopy, key, result.ID.String())
	})
	return result, err
}

// ListTokenCopies returns only the purchaser's copy metadata.
func (o *Orchestrator) ListTokenCopies(ctx context.Context, user string, id uuid.UUID) ([]tokenshares.Share, error) {
	t, err := o.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, tx.ErrNotFound) || (err == nil && t.CustomerID != user) {
		return nil, tx.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return o.st.Repos().TokenShares().ListCopies(ctx, id, user)
}
