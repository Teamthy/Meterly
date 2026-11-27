package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type meterRepo struct{ *repos }

const meterCols = `m.id, m.meter_number_encrypted, m.key_version, m.meter_number_hash, m.meter_number_masked, d.code,
	coalesce(m.meter_type, ''), coalesce(m.customer_name, ''), coalesce(m.address, ''),
	coalesce(m.validated_at, m.created_at), m.capabilities`

func scanMeter(row pgx.Row) (*meters.Meter, error) {
	var m meters.Meter
	var caps []byte
	if err := row.Scan(&m.ID, &m.NumberEncrypted, &m.KeyVersion, &m.NumberHash, &m.Masked, &m.DiscoCode,
		&m.MeterType, &m.CustomerName, &m.Address, &m.ValidatedAt, &caps); err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal(caps, &m.Capabilities); err != nil {
		return nil, err
	}
	m.ValidatedAt = m.ValidatedAt.UTC()
	return &m, nil
}

func (r meterRepo) Get(ctx context.Context, id uuid.UUID) (*meters.Meter, error) {
	return scanMeter(r.db.QueryRow(ctx, `SELECT `+meterCols+` FROM meters m JOIN distribution_companies d ON d.id = m.disco_id WHERE m.id = $1`, id))
}

func (r meterRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*meters.Meter, error) {
	if !r.inTx {
		return nil, errors.New("meter lock requires transaction")
	}
	return scanMeter(r.db.QueryRow(ctx, `SELECT `+meterCols+` FROM meters m JOIN distribution_companies d ON d.id=m.disco_id WHERE m.id=$1 FOR UPDATE OF m`, id))
}

func (r meterRepo) GetByHash(ctx context.Context, hash string) (*meters.Meter, error) {
	return scanMeter(r.db.QueryRow(ctx, `SELECT `+meterCols+` FROM meters m JOIN distribution_companies d ON d.id = m.disco_id WHERE m.meter_number_hash = $1 FOR UPDATE OF m`, hash))
}

func (r meterRepo) Insert(ctx context.Context, m *meters.Meter) error {
	disco, err := r.discoID(ctx, m.DiscoCode)
	if err != nil {
		return err
	}
	caps, err := json.Marshal(m.Capabilities)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `
		INSERT INTO meters (id, meter_number_encrypted, meter_number_hash, meter_number_masked, key_version, disco_id,
		                    meter_type, validation_status, validated_at, customer_name, address, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'VERIFIED', $8, $9, $10, $11)
		ON CONFLICT (meter_number_hash) DO NOTHING`,
		m.ID, m.NumberEncrypted, m.NumberHash, m.Masked, m.KeyVersion, disco, m.MeterType, m.ValidatedAt, m.CustomerName, m.Address, caps)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrDuplicate
	}
	return nil
}

func (r meterRepo) Update(ctx context.Context, m *meters.Meter) error {
	caps, err := json.Marshal(m.Capabilities)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE meters SET meter_type = $2, customer_name = $3, address = $4, validated_at = $5,
		                  capabilities = $6, validation_status = 'VERIFIED'
		WHERE id = $1`, m.ID, m.MeterType, m.CustomerName, m.Address, m.ValidatedAt, caps)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r meterRepo) UpsertGrant(ctx context.Context, g meters.Grant) error {
	return r.atomic(ctx, func(db DB) error {
		for attempt := 0; attempt < 2; attempt++ {
			var id uuid.UUID
			var cur meters.Permission
			err := db.QueryRow(ctx, `SELECT id, permission FROM meter_access_grants
				WHERE meter_id = $1 AND user_id = $2 AND revoked_at IS NULL FOR UPDATE`, g.MeterID, g.UserID).Scan(&id, &cur)
			if err == nil {
				perm := cur
				if g.Permission.Allows(cur) {
					perm = g.Permission
				}
				_, err = db.Exec(ctx, `UPDATE meter_access_grants
					SET permission = $2, label = CASE WHEN $3 = '' THEN label ELSE $3 END WHERE id = $1`, id, perm, g.Label)
				return mapErr(err)
			}
			if mapErr(err) != store.ErrNotFound {
				return mapErr(err)
			}
			tag, err := db.Exec(ctx, `
				INSERT INTO meter_access_grants (meter_id, user_id, permission, label, granted_by)
				VALUES ($1, $2, $3, NULLIF($4, ''), $5)
				ON CONFLICT (meter_id, user_id) WHERE revoked_at IS NULL DO NOTHING`,
				g.MeterID, g.UserID, g.Permission, g.Label, nullUUID(g.GrantedBy))
			if err != nil {
				return mapErr(err)
			}
			if tag.RowsAffected() == 1 {
				return nil
			}
			// Lost an insert race: loop once to update the winner's grant.
		}
		return store.ErrConflict
	})
}

func (r meterRepo) ActiveGrant(ctx context.Context, meterID uuid.UUID, userID string) (*meters.Grant, error) {
	g := meters.Grant{MeterID: meterID, UserID: userID}
	err := r.db.QueryRow(ctx, `SELECT permission, coalesce(label, ''), coalesce(granted_by::text, '')
		FROM meter_access_grants WHERE meter_id = $1 AND user_id = $2 AND revoked_at IS NULL`, meterID, userID).
		Scan(&g.Permission, &g.Label, &g.GrantedBy)
	if err != nil {
		return nil, mapErr(err)
	}
	return &g, nil
}

func (r meterRepo) ListAll(ctx context.Context, limit int) ([]*meters.Meter, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT `+meterCols+` FROM meters m
		JOIN distribution_companies d ON d.id = m.disco_id
		ORDER BY m.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]*meters.Meter, 0)
	for rows.Next() {
		m, err := scanMeter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r meterRepo) ListForUser(ctx context.Context, userID string) ([]meters.Listed, error) {
	rows, err := r.db.Query(ctx, `SELECT `+meterCols+`, coalesce(g.label, ''), g.permission
		FROM meter_access_grants g
		JOIN meters m ON m.id = g.meter_id
		JOIN distribution_companies d ON d.id = m.disco_id
		WHERE g.user_id = $1 AND g.revoked_at IS NULL
		ORDER BY g.created_at`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []meters.Listed
	for rows.Next() {
		var l meters.Listed
		var caps []byte
		if err := rows.Scan(&l.ID, &l.NumberEncrypted, &l.KeyVersion, &l.NumberHash, &l.Masked, &l.DiscoCode,
			&l.MeterType, &l.CustomerName, &l.Address, &l.ValidatedAt, &caps, &l.Label, &l.Permission); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(caps, &l.Capabilities); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r meterRepo) ListActiveUserIDs(ctx context.Context, meterID uuid.UUID) ([]string, error) {
	rows, err := r.db.Query(ctx, `SELECT DISTINCT user_id::text FROM meter_access_grants
		WHERE meter_id = $1 AND revoked_at IS NULL`, meterID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r meterRepo) Revoke(ctx context.Context, meterID uuid.UUID, userID string, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE meter_access_grants SET revoked_at = $3
		WHERE meter_id = $1 AND user_id = $2 AND revoked_at IS NULL`, meterID, userID, at)
	return mapErr(err)
}

// ---- quotes -------------------------------------------------------------------

type quoteRepo struct{ *repos }

func (r quoteRepo) Create(ctx context.Context, q *tx.Quote) error {
	opts := make([]string, len(q.DeliveryOptions))
	for i, o := range q.DeliveryOptions {
		opts[i] = string(o)
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO quotes (id, customer_id, meter_id, amount_minor, fee_minor, total_minor, currency,
		                    delivery_method, delivery_options, expires_at, status, created_at,
		                    pack_id, tariff_band, kobo_per_kwh, estimated_units_milli, credit_minor, discount_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'OPEN', $11, $12, $13, $14, $15, $16, $17)`,
		q.ID, q.CustomerID, q.MeterID, q.Amount.Minor, q.Fee.Minor, q.Total.Minor, string(q.Total.Currency),
		q.DeliveryMethod, opts, q.ExpiresAt, q.CreatedAt,
		q.PackID, q.TariffBand, q.KoboPerKWh, q.EstimatedUnitsMilli, q.CreditMinor, q.DiscountMinor)
	return mapErr(err)
}

func (r quoteRepo) get(ctx context.Context, id uuid.UUID, lock string) (*tx.Quote, error) {
	var q tx.Quote
	var cur, status string
	var opts []string
	err := r.db.QueryRow(ctx, `SELECT id, customer_id::text, meter_id, amount_minor, fee_minor, total_minor, currency,
		delivery_method, delivery_options, expires_at, status, created_at,
		coalesce(pack_id, ''), coalesce(tariff_band, ''), coalesce(kobo_per_kwh, 0),
		coalesce(estimated_units_milli, 0), coalesce(credit_minor, 0), coalesce(discount_minor, 0)
		FROM quotes WHERE id = $1 `+lock, id).
		Scan(&q.ID, &q.CustomerID, &q.MeterID, &q.Amount.Minor, &q.Fee.Minor, &q.Total.Minor, &cur,
			&q.DeliveryMethod, &opts, &q.ExpiresAt, &status, &q.CreatedAt,
			&q.PackID, &q.TariffBand, &q.KoboPerKWh, &q.EstimatedUnitsMilli, &q.CreditMinor, &q.DiscountMinor)
	if err != nil {
		return nil, mapErr(err)
	}
	q.Amount.Currency, q.Fee.Currency, q.Total.Currency = money.Currency(cur), money.Currency(cur), money.Currency(cur)
	for _, o := range opts {
		q.DeliveryOptions = append(q.DeliveryOptions, tx.DeliveryMethod(o))
	}
	q.Used = status == "USED"
	q.ExpiresAt, q.CreatedAt = q.ExpiresAt.UTC(), q.CreatedAt.UTC()
	return &q, nil
}

func (r quoteRepo) Get(ctx context.Context, id uuid.UUID) (*tx.Quote, error) {
	return r.get(ctx, id, "")
}
func (r quoteRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Quote, error) {
	return r.get(ctx, id, "FOR UPDATE")
}

func (r quoteRepo) MarkUsed(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE quotes SET status = 'USED' WHERE id = $1 AND status = 'OPEN'`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	return nil
}
