package pgstore

import (
	"context"
	"time"

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

func (r shareRepo) AddShare(ctx context.Context, s tokenshares.Share) error {
	tag, err := r.db.Exec(ctx, `INSERT INTO token_shares (
        id,vending_record_id,shared_by_user_id,channel,recipient_reference_hash,
        destination_ciphertext,destination_key_version,destination_masked,shared_at)
 SELECT $1,v.id,$2,$3,$4,$5,$6,$7,$8 FROM vending_records v WHERE v.transaction_id=$9`,
		s.ID, s.UserID, s.Channel, s.DestHMAC, s.DestCipher, s.DestKeyVer, s.DestMasked, s.SharedAt, s.TransactionID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r shareRepo) GetShare(ctx context.Context, shareID uuid.UUID) (tokenshares.Share, error) {
	var s tokenshares.Share
	err := r.db.QueryRow(ctx, `SELECT s.id,v.transaction_id,s.shared_by_user_id::text,s.channel,s.shared_at,
        COALESCE(s.recipient_reference_hash,''), s.destination_ciphertext,
        COALESCE(s.destination_key_version,''), COALESCE(s.destination_masked,'')
 FROM token_shares s JOIN vending_records v ON v.id=s.vending_record_id
 WHERE s.id=$1`, shareID).Scan(&s.ID, &s.TransactionID, &s.UserID, &s.Channel, &s.SharedAt, &s.DestHMAC, &s.DestCipher, &s.DestKeyVer, &s.DestMasked)
	return s, mapErr(err)
}

func (r shareRepo) ListShares(ctx context.Context, id uuid.UUID, user string) ([]tokenshares.Share, error) {
	rows, err := r.db.Query(ctx, `SELECT s.id,v.transaction_id,s.shared_by_user_id::text,s.channel,s.shared_at,
        COALESCE(s.destination_masked,'')
 FROM token_shares s JOIN vending_records v ON v.id=s.vending_record_id
 WHERE v.transaction_id=$1 AND s.shared_by_user_id=$2
 ORDER BY s.shared_at DESC,s.id DESC LIMIT 200`, id, user)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	result := []tokenshares.Share{}
	for rows.Next() {
		var s tokenshares.Share
		if err := rows.Scan(&s.ID, &s.TransactionID, &s.UserID, &s.Channel, &s.SharedAt, &s.DestMasked); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (r shareRepo) CountSends(ctx context.Context, userID string, start, end time.Time) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM token_shares
 WHERE shared_by_user_id=$1 AND channel <> 'COPY' AND shared_at >= $2 AND shared_at < $3`,
		userID, start, end).Scan(&n)
	return n, mapErr(err)
}
