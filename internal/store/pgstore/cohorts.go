package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type remoteCohortRepo struct{ *repos }

func (r remoteCohortRepo) Create(ctx context.Context, c meters.RemoteCohort) error {
	_, err := r.db.Exec(ctx, `INSERT INTO remote_credit_cohorts
		(id, disco_code, number_prefix, manufacturer, authorised, evidence, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		c.ID, c.DiscoCode, c.NumberPrefix, c.Manufacturer, c.Authorised, c.Evidence, c.CreatedBy, c.CreatedAt)
	return mapErr(err)
}

func (r remoteCohortRepo) Get(ctx context.Context, id uuid.UUID) (meters.RemoteCohort, error) {
	c, err := scanCohort(r.db.QueryRow(ctx, `SELECT id, disco_code, number_prefix, manufacturer, authorised,
		evidence, created_by, created_at, revoked_at FROM remote_credit_cohorts WHERE id = $1`, id))
	if err != nil {
		return meters.RemoteCohort{}, mapErr(err)
	}
	return c, nil
}

func (r remoteCohortRepo) List(ctx context.Context) ([]meters.RemoteCohort, error) {
	rows, err := r.db.Query(ctx, `SELECT id, disco_code, number_prefix, manufacturer, authorised,
		evidence, created_by, created_at, revoked_at FROM remote_credit_cohorts ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []meters.RemoteCohort
	for rows.Next() {
		c, err := scanCohort(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r remoteCohortRepo) Match(ctx context.Context, disco, meterNumber string) (meters.RemoteCohort, error) {
	c, err := scanCohort(r.db.QueryRow(ctx, `SELECT id, disco_code, number_prefix, manufacturer, authorised,
		evidence, created_by, created_at, revoked_at
		FROM remote_credit_cohorts
		WHERE revoked_at IS NULL AND authorised
		  AND disco_code = upper($1) AND starts_with($2, number_prefix)
		ORDER BY length(number_prefix) DESC
		LIMIT 1`, disco, meterNumber))
	if err != nil {
		return meters.RemoteCohort{}, mapErr(err)
	}
	return c, nil
}

func (r remoteCohortRepo) Revoke(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE remote_credit_cohorts
		SET revoked_at = $2, authorised = false
		WHERE id = $1 AND revoked_at IS NULL`, id, at.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

type cohortRow interface {
	Scan(dest ...any) error
}

func scanCohort(row cohortRow) (meters.RemoteCohort, error) {
	var c meters.RemoteCohort
	err := row.Scan(&c.ID, &c.DiscoCode, &c.NumberPrefix, &c.Manufacturer, &c.Authorised,
		&c.Evidence, &c.CreatedBy, &c.CreatedAt, &c.RevokedAt)
	if err != nil {
		return meters.RemoteCohort{}, err
	}
	c.CreatedAt = c.CreatedAt.UTC()
	if c.RevokedAt != nil {
		t := c.RevokedAt.UTC()
		c.RevokedAt = &t
	}
	return c, nil
}
