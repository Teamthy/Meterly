package pgstore

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notificationintents"
	"github.com/teamthy/meterly/internal/store"
)

type candidateRepo struct{ *repos }

func (r candidateRepo) Claim(ctx context.Context, limit int) ([]notificationintents.Intent, error) {
	if !r.inTx {
		return nil, errors.New("candidate claim requires transaction")
	}
	if limit <= 0 {
		return nil, errors.New("invalid candidate limit")
	}
	rows, err := r.db.Query(ctx, `SELECT e.id,e.aggregate_id,e.event_type FROM outbox_events e
 WHERE e.aggregate_type='electricity_transaction' AND e.event_type=ANY($1)
 AND NOT EXISTS (SELECT 1 FROM notification_candidates c WHERE c.event_id=e.id)
 ORDER BY e.created_at,e.id LIMIT $2 FOR UPDATE OF e SKIP LOCKED`, notificationintents.EventTypes(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []notificationintents.Intent
	for rows.Next() {
		var c notificationintents.Intent
		if err := rows.Scan(&c.EventID, &c.TransactionID, &c.EventType); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (r candidateRepo) Record(ctx context.Context, c notificationintents.Intent) error {
	if !r.inTx {
		return errors.New("candidate record requires transaction")
	}
	tag, err := r.db.Exec(ctx, `INSERT INTO notification_candidates (event_id,transaction_id,event_type,created_at)
 SELECT id,aggregate_id,event_type,$4 FROM outbox_events
 WHERE id=$1 AND aggregate_id=$2 AND event_type=$3 AND aggregate_type='electricity_transaction' AND event_type=ANY($5)
 ON CONFLICT (event_id) DO NOTHING`, c.EventID, c.TransactionID, c.EventType, c.CreatedAt, notificationintents.EventTypes())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		existing, err := r.Get(ctx, c.EventID)
		if err != nil {
			return err
		}
		if existing.TransactionID != c.TransactionID || existing.EventType != c.EventType {
			return store.ErrConflict
		}
	}
	return nil
}
func (r candidateRepo) Get(ctx context.Context, id uuid.UUID) (notificationintents.Intent, error) {
	var c notificationintents.Intent
	err := r.db.QueryRow(ctx, `SELECT event_id,transaction_id,event_type,created_at FROM notification_candidates WHERE event_id=$1`, id).Scan(&c.EventID, &c.TransactionID, &c.EventType, &c.CreatedAt)
	return c, mapErr(err)
}
