package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/store"
)

type authRepo struct{ *repos }

func (r authRepo) PutChallenge(ctx context.Context, c identity.Challenge) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	var uid any
	if c.UserID != "" {
		id, err := uuid.Parse(c.UserID)
		if err != nil {
			return store.ErrNotFound
		}
		uid = id
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO auth_challenges (id, challenge, purpose, user_id, expires_at)
		VALUES ($1,$2,$3,$4,$5)`, c.ID, c.Challenge, c.Purpose, uid, c.ExpiresAt.UTC())
	return mapErr(err)
}

func (r authRepo) TakeChallenge(ctx context.Context, challenge []byte, now time.Time) (identity.Challenge, error) {
	var c identity.Challenge
	var uid *uuid.UUID
	err := r.db.QueryRow(ctx, `
		UPDATE auth_challenges
		SET consumed_at = $2
		WHERE challenge = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING id, challenge, purpose, user_id, expires_at`,
		challenge, now.UTC(),
	).Scan(&c.ID, &c.Challenge, &c.Purpose, &uid, &c.ExpiresAt)
	if err != nil {
		return identity.Challenge{}, mapErr(err)
	}
	if uid != nil {
		c.UserID = uid.String()
	}
	c.ExpiresAt = c.ExpiresAt.UTC()
	return c, nil
}

func (r authRepo) InsertCredential(ctx context.Context, c identity.Credential) error {
	uid, err := uuid.Parse(c.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, sign_count, aaguid, name, created_at, last_used_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		c.ID, uid, c.CredentialID, c.PublicKey, int64(c.SignCount), c.AAGUID, c.Name, c.CreatedAt.UTC(), c.LastUsedAt.UTC())
	return mapErr(err)
}

func (r authRepo) GetCredentialByCredID(ctx context.Context, credID []byte) (identity.Credential, error) {
	var c identity.Credential
	var sc int64
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id::text, credential_id, public_key, sign_count, aaguid, name, created_at, last_used_at
		FROM webauthn_credentials WHERE credential_id = $1`, credID).
		Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &sc, &c.AAGUID, &c.Name, &c.CreatedAt, &c.LastUsedAt)
	if err != nil {
		return identity.Credential{}, mapErr(err)
	}
	c.SignCount = uint32(sc)
	c.CreatedAt = c.CreatedAt.UTC()
	c.LastUsedAt = c.LastUsedAt.UTC()
	return c, nil
}

func (r authRepo) ListCredentials(ctx context.Context, userID string) ([]identity.Credential, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id::text, credential_id, public_key, sign_count, aaguid, name, created_at, last_used_at
		FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []identity.Credential{}
	for rows.Next() {
		var c identity.Credential
		var sc int64
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &sc, &c.AAGUID, &c.Name, &c.CreatedAt, &c.LastUsedAt); err != nil {
			return nil, mapErr(err)
		}
		c.SignCount = uint32(sc)
		c.CreatedAt = c.CreatedAt.UTC()
		c.LastUsedAt = c.LastUsedAt.UTC()
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

func (r authRepo) UpdateCredentialUse(ctx context.Context, credID []byte, signCount uint32, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE webauthn_credentials SET sign_count = $2, last_used_at = $3 WHERE credential_id = $1`,
		credID, int64(signCount), at.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r authRepo) DeleteCredential(ctx context.Context, userID string, id uuid.UUID) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return store.ErrNotFound
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, id, uid)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r authRepo) InsertSession(ctx context.Context, s identity.Session) error {
	uid, err := uuid.Parse(s.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO auth_sessions (id, user_id, token_hmac, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5)`, s.ID, uid, s.TokenHMAC, s.ExpiresAt.UTC(), s.CreatedAt.UTC())
	return mapErr(err)
}

func (r authRepo) GetSessionByHMAC(ctx context.Context, hmac string) (identity.Session, error) {
	var s identity.Session
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id::text, token_hmac, expires_at, revoked_at, created_at
		FROM auth_sessions WHERE token_hmac = $1`, hmac).
		Scan(&s.ID, &s.UserID, &s.TokenHMAC, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt)
	if err != nil {
		return identity.Session{}, mapErr(err)
	}
	s.ExpiresAt = s.ExpiresAt.UTC()
	s.CreatedAt = s.CreatedAt.UTC()
	return s, nil
}

func (r authRepo) RevokeSession(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE auth_sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
