package pgstore

import (
	"context"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/tokenshares"
)

type shareRepo struct{ *repos }

func (r shareRepo) AddCopy(ctx context.Context, s tokenshares.Share) error {
	tag, err := r.db.Exec(ctx, `INSERT INTO token_shares (id,vending_record_id,shared_by_user_id,channel,shared_at)
 SELECT $1,v.id,$2,$3,$4 FROM vending_records v WHERE v.transaction_id=$5`, s.ID, s.UserID, s.Channel, s.SharedAt, s.TransactionID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}
func (r shareRepo) ListCopies(ctx context.Context, id uuid.UUID, user string) ([]tokenshares.Share, error) {
	rows, err := r.db.Query(ctx, `SELECT s.id,v.transaction_id,s.shared_by_user_id::text,s.channel,s.shared_at
 FROM token_shares s JOIN vending_records v ON v.id=s.vending_record_id
 WHERE v.transaction_id=$1 AND s.shared_by_user_id=$2 AND s.channel='COPY'
 ORDER BY s.shared_at DESC,s.id DESC LIMIT 200`, id, user)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	result := []tokenshares.Share{}
	for rows.Next() {
		var s tokenshares.Share
		if err := rows.Scan(&s.ID, &s.TransactionID, &s.UserID, &s.Channel, &s.SharedAt); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (r shareRepo) GetCopy(ctx context.Context, shareID, txnID uuid.UUID, user string) (tokenshares.Share, error) {
	var s tokenshares.Share
	err := r.db.QueryRow(ctx, `SELECT s.id,v.transaction_id,s.shared_by_user_id::text,s.channel,s.shared_at
 FROM token_shares s JOIN vending_records v ON v.id=s.vending_record_id
 WHERE s.id=$1 AND v.transaction_id=$2 AND s.shared_by_user_id=$3 AND s.channel='COPY'`, shareID, txnID, user).Scan(&s.ID, &s.TransactionID, &s.UserID, &s.Channel, &s.SharedAt)
	return s, mapErr(err)
}
