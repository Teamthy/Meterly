package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/store"
)

type readingRepo struct{ *repos }

func (r readingRepo) InsertSample(ctx context.Context, s meters.ReadingSample) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO meter_reading_samples (id, meter_id, milli_kwh, read_at, provider)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (meter_id, read_at) DO NOTHING`,
		s.ID, s.MeterID, s.MilliKWh, s.ReadAt.UTC(), s.Provider)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r readingRepo) ListSamples(ctx context.Context, meterID uuid.UUID, since time.Time, limit int) ([]meters.ReadingSample, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, meter_id, milli_kwh, read_at, provider
		FROM meter_reading_samples
		WHERE meter_id = $1 AND read_at >= $2
		ORDER BY read_at DESC
		LIMIT $3`, meterID, since.UTC(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []meters.ReadingSample{}
	for rows.Next() {
		var s meters.ReadingSample
		if err := rows.Scan(&s.ID, &s.MeterID, &s.MilliKWh, &s.ReadAt, &s.Provider); err != nil {
			return nil, mapErr(err)
		}
		s.ReadAt = s.ReadAt.UTC()
		out = append(out, s)
	}
	return out, mapErr(rows.Err())
}

func (r readingRepo) ListDistinctMeterIDs(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `SELECT DISTINCT meter_id FROM meter_reading_samples WHERE read_at >= $1`, since.UTC())
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, mapErr(rows.Err())
}
