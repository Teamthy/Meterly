// Package rekey batch-re-encrypts AES-GCM ciphertexts to a new KEY_VERSION.
// HMAC hashes are not rotated (METER_HMAC_KEY stays). Never logs plaintext.
package rekey

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/secure"
)

type Counts struct {
	Table string `json:"table"`
	N     int    `json:"n"`
}

type Report struct {
	FromVersion string   `json:"from_version"`
	ToVersion   string   `json:"to_version"`
	Counts      []Counts `json:"counts"`
}

func ReencryptUnbound(oldB, newB *secure.Box, ct []byte) ([]byte, error) {
	if len(ct) == 0 {
		return ct, nil
	}
	pt, err := oldB.Decrypt(ct)
	if err != nil {
		return nil, err
	}
	return newB.Encrypt(pt)
}

func ReencryptBound(oldB, newB *secure.Box, ct, aad []byte) ([]byte, error) {
	if len(ct) == 0 {
		return ct, nil
	}
	pt, err := oldB.DecryptBound(ct, aad)
	if err != nil {
		return nil, err
	}
	return newB.EncryptBound(pt, aad)
}

func Run(ctx context.Context, pool *pgxpool.Pool, oldB, newB *secure.Box) (Report, error) {
	rep := Report{FromVersion: oldB.KeyVersion, ToVersion: newB.KeyVersion}
	add := func(table string, n int) { rep.Counts = append(rep.Counts, Counts{Table: table, N: n}) }

	n, err := reencryptUnboundTable(ctx, pool, oldB, newB,
		`SELECT id, meter_number_encrypted FROM meters WHERE octet_length(meter_number_encrypted) > 0 AND coalesce(key_version,'') <> $1`,
		`UPDATE meters SET meter_number_encrypted=$1, key_version=$2 WHERE id=$3`, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("meters: %w", err)
	}
	add("meters", n)

	n, err = reencryptUnboundTable(ctx, pool, oldB, newB,
		`SELECT transaction_id, token_ciphertext FROM vending_records WHERE token_ciphertext IS NOT NULL AND octet_length(token_ciphertext) > 0 AND coalesce(token_key_version,'') <> $1`,
		`UPDATE vending_records SET token_ciphertext=$1, token_key_version=$2 WHERE transaction_id=$3`, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("vending_records: %w", err)
	}
	add("vending_records", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, email_ciphertext FROM users WHERE email_ciphertext IS NOT NULL AND octet_length(email_ciphertext) > 0 AND coalesce(email_key_version,'') <> $1`,
		`UPDATE users SET email_ciphertext=$1, email_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("user-email:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("users.email: %w", err)
	}
	add("users.email", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, phone_ciphertext FROM users WHERE phone_ciphertext IS NOT NULL AND octet_length(phone_ciphertext) > 0 AND coalesce(phone_key_version,'') <> $1`,
		`UPDATE users SET phone_ciphertext=$1, phone_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("user-phone:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("users.phone: %w", err)
	}
	add("users.phone", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT transaction_id::text, recipient_contact_encrypted FROM gift_transactions WHERE recipient_contact_encrypted IS NOT NULL AND octet_length(recipient_contact_encrypted) > 0 AND coalesce(contact_key_version,'') <> $1`,
		`UPDATE gift_transactions SET recipient_contact_encrypted=$1, contact_key_version=$2 WHERE transaction_id=$3::uuid`,
		func(id string) []byte { return []byte("gift-contact:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("gift_transactions: %w", err)
	}
	add("gift_transactions", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, destination_ciphertext FROM token_shares WHERE destination_ciphertext IS NOT NULL AND octet_length(destination_ciphertext) > 0 AND coalesce(destination_key_version,'') <> $1`,
		`UPDATE token_shares SET destination_ciphertext=$1, destination_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte {
			u, _ := uuid.Parse(id)
			return []byte("token-share-dest:" + u.String())
		}, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("token_shares: %w", err)
	}
	add("token_shares", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, note_ciphertext FROM payment_requests WHERE note_ciphertext IS NOT NULL AND octet_length(note_ciphertext) > 0 AND coalesce(note_key_version,'') <> $1`,
		`UPDATE payment_requests SET note_ciphertext=$1, note_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("payrequest-note:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("payment_requests: %w", err)
	}
	add("payment_requests", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT transaction_id::text, email_ciphertext FROM transaction_payers WHERE octet_length(email_ciphertext) > 0 AND coalesce(email_key_version,'') <> $1`,
		`UPDATE transaction_payers SET email_ciphertext=$1, email_key_version=$2 WHERE transaction_id=$3::uuid`,
		func(id string) []byte { return []byte("payer-email:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("transaction_payers.email: %w", err)
	}
	add("transaction_payers.email", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT transaction_id::text, payer_name_ciphertext FROM transaction_payers WHERE payer_name_ciphertext IS NOT NULL AND octet_length(payer_name_ciphertext) > 0 AND coalesce(payer_name_key_version,'') <> $1`,
		`UPDATE transaction_payers SET payer_name_ciphertext=$1, payer_name_key_version=$2 WHERE transaction_id=$3::uuid`,
		func(id string) []byte { return []byte("payer-name:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("transaction_payers.name: %w", err)
	}
	add("transaction_payers.name", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT transaction_id::text, status_code_ciphertext FROM transaction_payers WHERE octet_length(status_code_ciphertext) > 0 AND coalesce(status_code_key_version,'') <> $1`,
		`UPDATE transaction_payers SET status_code_ciphertext=$1, status_code_key_version=$2 WHERE transaction_id=$3::uuid`,
		func(id string) []byte { return []byte("payer-status-code:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("transaction_payers.status: %w", err)
	}
	add("transaction_payers.status", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, note_ciphertext FROM pots WHERE note_ciphertext IS NOT NULL AND octet_length(note_ciphertext) > 0 AND coalesce(note_key_version,'') <> $1`,
		`UPDATE pots SET note_ciphertext=$1, note_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("pot-note:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("pots: %w", err)
	}
	add("pots", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, email_ciphertext FROM pot_contributions WHERE octet_length(email_ciphertext) > 0 AND coalesce(email_key_version,'') <> $1`,
		`UPDATE pot_contributions SET email_ciphertext=$1, email_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("pot-contributor-email:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("pot_contributions.email: %w", err)
	}
	add("pot_contributions.email", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, name_ciphertext FROM pot_contributions WHERE name_ciphertext IS NOT NULL AND octet_length(name_ciphertext) > 0 AND coalesce(name_key_version,'') <> $1`,
		`UPDATE pot_contributions SET name_ciphertext=$1, name_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("pot-contributor-name:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("pot_contributions.name: %w", err)
	}
	add("pot_contributions.name", n)

	n, err = reencryptBoundTable(ctx, pool, oldB, newB,
		`SELECT id::text, phone_ciphertext FROM pot_contributions WHERE phone_ciphertext IS NOT NULL AND octet_length(phone_ciphertext) > 0 AND coalesce(phone_key_version,'') <> $1`,
		`UPDATE pot_contributions SET phone_ciphertext=$1, phone_key_version=$2 WHERE id=$3::uuid`,
		func(id string) []byte { return []byte("pot-contributor-phone:" + id) }, newB.KeyVersion)
	if err != nil {
		return rep, fmt.Errorf("pot_contributions.phone: %w", err)
	}
	add("pot_contributions.phone", n)

	n, err = reencryptAutopower(ctx, pool, oldB, newB)
	if err != nil {
		return rep, err
	}
	add("saved_payment_methods", n)

	return rep, nil
}

func reencryptUnboundTable(ctx context.Context, pool *pgxpool.Pool, oldB, newB *secure.Box, sel, upd, newVer string) (int, error) {
	rows, err := pool.Query(ctx, sel, newVer)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var ct []byte
		if err := rows.Scan(&id, &ct); err != nil {
			return n, err
		}
		next, err := ReencryptUnbound(oldB, newB, ct)
		if err != nil {
			return n, err
		}
		if _, err := pool.Exec(ctx, upd, next, newVer, id); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func reencryptBoundTable(ctx context.Context, pool *pgxpool.Pool, oldB, newB *secure.Box, sel, upd string, aad func(id string) []byte, newVer string) (int, error) {
	rows, err := pool.Query(ctx, sel, newVer)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var ct []byte
		if err := rows.Scan(&id, &ct); err != nil {
			return n, err
		}
		next, err := ReencryptBound(oldB, newB, ct, aad(id))
		if err != nil {
			return n, err
		}
		if _, err := pool.Exec(ctx, upd, next, newVer, id); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func reencryptAutopower(ctx context.Context, pool *pgxpool.Pool, oldB, newB *secure.Box) (int, error) {
	rows, err := pool.Query(ctx, `SELECT id, user_id::text, auth_code_ciphertext, email_ciphertext FROM saved_payment_methods WHERE coalesce(key_version,'') <> $1`, newB.KeyVersion)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var userID string
		var authCT, emailCT []byte
		if err := rows.Scan(&id, &userID, &authCT, &emailCT); err != nil {
			return n, err
		}
		authNext, err := ReencryptBound(oldB, newB, authCT, []byte("autopower-auth:"+userID+":"+id.String()))
		if err != nil {
			return n, err
		}
		emailNext, err := ReencryptBound(oldB, newB, emailCT, []byte("autopower-email:"+userID+":"+id.String()))
		if err != nil {
			return n, err
		}
		if _, err := pool.Exec(ctx, `UPDATE saved_payment_methods SET auth_code_ciphertext=$1, email_ciphertext=$2, key_version=$3 WHERE id=$4`,
			authNext, emailNext, newB.KeyVersion, id); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
