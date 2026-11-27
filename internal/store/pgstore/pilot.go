package pgstore

import (
	"context"
	"strings"
	"time"

	"github.com/teamthy/meterly/internal/pilot"
)

type pilotRepo struct{ *repos }

func (r pilotRepo) List(ctx context.Context, kind pilot.Kind) ([]pilot.Entry, error) {
	q := `SELECT kind, value, reason, created_at, created_by FROM pilot_allowlist`
	var args []any
	if kind != "" {
		q += ` WHERE kind=$1`
		args = append(args, string(kind))
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []pilot.Entry
	for rows.Next() {
		var e pilot.Entry
		if err := rows.Scan(&e.Kind, &e.Value, &e.Reason, &e.CreatedAt, &e.CreatedBy); err != nil {
			return nil, mapErr(err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r pilotRepo) Add(ctx context.Context, e pilot.Entry) error {
	e.Value = strings.TrimSpace(e.Value)
	if e.Value == "" {
		return nil
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `INSERT INTO pilot_allowlist (kind, value, reason, created_at, created_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (kind, value) DO NOTHING`,
		string(e.Kind), e.Value, e.Reason, e.CreatedAt.UTC(), e.CreatedBy)
	return mapErr(err)
}

func (r pilotRepo) Remove(ctx context.Context, kind pilot.Kind, value string) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM pilot_allowlist WHERE kind=$1 AND value=$2`, string(kind), strings.TrimSpace(value))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return pilot.ErrNotFound
	}
	return nil
}

func (r pilotRepo) Has(ctx context.Context, kind pilot.Kind, value string) (bool, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM pilot_allowlist WHERE kind=$1 AND value=$2`, string(kind), strings.TrimSpace(value)).Scan(&n)
	return n > 0, mapErr(err)
}

func (r pilotRepo) CountKind(ctx context.Context, kind pilot.Kind) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM pilot_allowlist WHERE kind=$1`, string(kind)).Scan(&n)
	return n, mapErr(err)
}
