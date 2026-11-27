package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/support"
)

type supportRepo struct{ *repos }

func (r supportRepo) UpsertIdentityKey(ctx context.Context, k support.IdentityKey) error {
	uid, err := uuid.Parse(k.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO support_identity_keys (user_id, public_key, algorithm, staff, created_at, rotated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET
			public_key = EXCLUDED.public_key,
			algorithm = EXCLUDED.algorithm,
			staff = EXCLUDED.staff,
			rotated_at = EXCLUDED.rotated_at`,
		uid, k.PublicKey, k.Algorithm, k.Staff, k.CreatedAt, k.RotatedAt)
	return mapErr(err)
}

func (r supportRepo) GetIdentityKey(ctx context.Context, userID string) (support.IdentityKey, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return support.IdentityKey{}, store.ErrNotFound
	}
	var k support.IdentityKey
	err = r.db.QueryRow(ctx, `
		SELECT user_id::text, public_key, algorithm, staff, created_at, rotated_at
		FROM support_identity_keys WHERE user_id = $1`, uid).
		Scan(&k.UserID, &k.PublicKey, &k.Algorithm, &k.Staff, &k.CreatedAt, &k.RotatedAt)
	return k, mapErr(err)
}

func (r supportRepo) ListStaffKeys(ctx context.Context) ([]support.IdentityKey, error) {
	rows, err := r.db.Query(ctx, `
		SELECT user_id::text, public_key, algorithm, staff, created_at, rotated_at
		FROM support_identity_keys WHERE staff = TRUE ORDER BY user_id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []support.IdentityKey
	for rows.Next() {
		var k support.IdentityKey
		if err := rows.Scan(&k.UserID, &k.PublicKey, &k.Algorithm, &k.Staff, &k.CreatedAt, &k.RotatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (r supportRepo) CreateThread(ctx context.Context, t *support.Thread) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO support_threads (id, customer_id, mode, status, assigned_operator_id, created_at, updated_at, last_message_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.CustomerID, string(t.Mode), string(t.Status), nullUUID(t.AssignedOperatorID),
		t.CreatedAt, t.UpdatedAt, t.LastMessageAt)
	return mapErr(err)
}

func scanThread(row pgx.Row) (*support.Thread, error) {
	var t support.Thread
	var assigned *string
	err := row.Scan(&t.ID, &t.CustomerID, &t.Mode, &t.Status, &assigned, &t.CreatedAt, &t.UpdatedAt, &t.LastMessageAt)
	if assigned != nil {
		t.AssignedOperatorID = *assigned
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (r supportRepo) GetThread(ctx context.Context, id uuid.UUID) (*support.Thread, error) {
	return scanThread(r.db.QueryRow(ctx, `
		SELECT id, customer_id::text, mode, status, assigned_operator_id::text, created_at, updated_at, last_message_at
		FROM support_threads WHERE id = $1`, id))
}

func (r supportRepo) GetThreadForUpdate(ctx context.Context, id uuid.UUID) (*support.Thread, error) {
	return scanThread(r.db.QueryRow(ctx, `
		SELECT id, customer_id::text, mode, status, assigned_operator_id::text, created_at, updated_at, last_message_at
		FROM support_threads WHERE id = $1 FOR UPDATE`, id))
}

func (r supportRepo) UpdateThread(ctx context.Context, t *support.Thread) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE support_threads
		SET mode = $2, status = $3, assigned_operator_id = $4, updated_at = $5, last_message_at = $6
		WHERE id = $1`,
		t.ID, string(t.Mode), string(t.Status), nullUUID(t.AssignedOperatorID), t.UpdatedAt, t.LastMessageAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return store.ErrNotFound
	}
	return nil
}

func collectThreads(rows pgx.Rows) ([]*support.Thread, error) {
	defer rows.Close()
	var out []*support.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r supportRepo) ListThreadsForCustomer(ctx context.Context, customerID string) ([]*support.Thread, error) {
	uid, err := uuid.Parse(customerID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, customer_id::text, mode, status, assigned_operator_id::text, created_at, updated_at, last_message_at
		FROM support_threads WHERE customer_id = $1 ORDER BY last_message_at DESC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	return collectThreads(rows)
}

func (r supportRepo) ListThreadsForStaff(ctx context.Context, status support.ThreadStatus, limit int) ([]*support.Thread, error) {
	if limit <= 0 {
		limit = support.DefaultListLimit
	}
	var (
		rows pgx.Rows
		err  error
	)
	if status == "" {
		rows, err = r.db.Query(ctx, `
			SELECT id, customer_id::text, mode, status, assigned_operator_id::text, created_at, updated_at, last_message_at
			FROM support_threads ORDER BY last_message_at DESC LIMIT $1`, limit)
	} else {
		rows, err = r.db.Query(ctx, `
			SELECT id, customer_id::text, mode, status, assigned_operator_id::text, created_at, updated_at, last_message_at
			FROM support_threads WHERE status = $1 ORDER BY last_message_at DESC LIMIT $2`, string(status), limit)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return collectThreads(rows)
}

func (r supportRepo) AddMessage(ctx context.Context, m *support.Message) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO support_messages (id, thread_id, sender_id, sender_kind, ciphertext, nonce, body_enc, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		m.ID, m.ThreadID, m.SenderID, string(m.SenderKind), nilIfEmpty(m.Ciphertext), nilIfEmpty(m.Nonce), nilIfEmpty(m.BodyEnc), m.CreatedAt)
	return mapErr(err)
}

func (r supportRepo) ListMessages(ctx context.Context, threadID uuid.UUID, after time.Time, limit int) ([]*support.Message, error) {
	if limit <= 0 {
		limit = support.DefaultListLimit
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, thread_id, sender_id, sender_kind, ciphertext, nonce, body_enc, created_at
		FROM support_messages
		WHERE thread_id = $1 AND created_at > $2
		ORDER BY created_at ASC, id ASC
		LIMIT $3`, threadID, after, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*support.Message
	for rows.Next() {
		var m support.Message
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.SenderID, &m.SenderKind, &m.Ciphertext, &m.Nonce, &m.BodyEnc, &m.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (r supportRepo) UpsertWrappedKey(ctx context.Context, w support.WrappedKey) error {
	holder, err := uuid.Parse(w.HolderUserID)
	if err != nil {
		return store.ErrNotFound
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO support_thread_keys (thread_id, holder_user_id, wrapped_key, nonce, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (thread_id, holder_user_id) DO UPDATE SET
			wrapped_key = EXCLUDED.wrapped_key,
			nonce = EXCLUDED.nonce`,
		w.ThreadID, holder, w.WrappedKey, w.Nonce, w.CreatedAt)
	return mapErr(err)
}

func (r supportRepo) ListWrappedKeys(ctx context.Context, threadID uuid.UUID) ([]support.WrappedKey, error) {
	rows, err := r.db.Query(ctx, `
		SELECT thread_id, holder_user_id::text, wrapped_key, nonce, created_at
		FROM support_thread_keys WHERE thread_id = $1 ORDER BY holder_user_id`, threadID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []support.WrappedKey
	for rows.Next() {
		var w support.WrappedKey
		if err := rows.Scan(&w.ThreadID, &w.HolderUserID, &w.WrappedKey, &w.Nonce, &w.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r supportRepo) GetWrappedKey(ctx context.Context, threadID uuid.UUID, holderUserID string) (support.WrappedKey, error) {
	holder, err := uuid.Parse(holderUserID)
	if err != nil {
		return support.WrappedKey{}, store.ErrNotFound
	}
	var w support.WrappedKey
	err = r.db.QueryRow(ctx, `
		SELECT thread_id, holder_user_id::text, wrapped_key, nonce, created_at
		FROM support_thread_keys WHERE thread_id = $1 AND holder_user_id = $2`, threadID, holder).
		Scan(&w.ThreadID, &w.HolderUserID, &w.WrappedKey, &w.Nonce, &w.CreatedAt)
	return w, mapErr(err)
}
