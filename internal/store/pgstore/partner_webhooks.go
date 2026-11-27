package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/partners"
	"github.com/teamthy/meterly/internal/store"
)

const webhookCols = `id, partner_id, url, description, events,
	secret_ciphertext, secret_key_version, prev_secret_ciphertext, prev_secret_key_version,
	overlap_until, include_token, enabled, created_at, updated_at`

func scanWebhook(row interface{ Scan(dest ...any) error }) (partners.WebhookEndpoint, error) {
	var ep partners.WebhookEndpoint
	err := row.Scan(
		&ep.ID, &ep.PartnerID, &ep.URL, &ep.Description, &ep.Events,
		&ep.SecretCiphertext, &ep.SecretKeyVersion, &ep.PrevSecretCiphertext, &ep.PrevSecretKeyVersion,
		&ep.OverlapUntil, &ep.IncludeToken, &ep.Enabled, &ep.CreatedAt, &ep.UpdatedAt,
	)
	if err != nil {
		return partners.WebhookEndpoint{}, mapErr(err)
	}
	ep.CreatedAt = ep.CreatedAt.UTC()
	ep.UpdatedAt = ep.UpdatedAt.UTC()
	if ep.OverlapUntil != nil {
		t := ep.OverlapUntil.UTC()
		ep.OverlapUntil = &t
	}
	return ep, nil
}

func (r partnerRepo) CreateWebhookEndpoint(ctx context.Context, ep partners.WebhookEndpoint) error {
	if ep.ID == uuid.Nil {
		ep.ID = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO partner_webhook_endpoints (
			id, partner_id, url, description, events,
			secret_ciphertext, secret_key_version, prev_secret_ciphertext, prev_secret_key_version,
			overlap_until, include_token, enabled, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		ep.ID, ep.PartnerID, ep.URL, ep.Description, ep.Events,
		ep.SecretCiphertext, ep.SecretKeyVersion, nullBytes(ep.PrevSecretCiphertext), ep.PrevSecretKeyVersion,
		ep.OverlapUntil, ep.IncludeToken, ep.Enabled, ep.CreatedAt.UTC(), ep.UpdatedAt.UTC())
	return mapErr(err)
}

func (r partnerRepo) GetWebhookEndpoint(ctx context.Context, id uuid.UUID) (partners.WebhookEndpoint, error) {
	return scanWebhook(r.db.QueryRow(ctx, `SELECT `+webhookCols+` FROM partner_webhook_endpoints WHERE id=$1`, id))
}

func (r partnerRepo) GetWebhookEndpointForUpdate(ctx context.Context, id uuid.UUID) (partners.WebhookEndpoint, error) {
	return scanWebhook(r.db.QueryRow(ctx, `SELECT `+webhookCols+` FROM partner_webhook_endpoints WHERE id=$1 FOR UPDATE`, id))
}

func (r partnerRepo) ListWebhookEndpoints(ctx context.Context, partnerID uuid.UUID) ([]partners.WebhookEndpoint, error) {
	rows, err := r.db.Query(ctx, `SELECT `+webhookCols+` FROM partner_webhook_endpoints WHERE partner_id=$1 ORDER BY created_at DESC`, partnerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]partners.WebhookEndpoint, 0)
	for rows.Next() {
		ep, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

func (r partnerRepo) UpdateWebhookEndpoint(ctx context.Context, ep partners.WebhookEndpoint) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE partner_webhook_endpoints SET
			url=$2, description=$3, events=$4,
			secret_ciphertext=$5, secret_key_version=$6,
			prev_secret_ciphertext=$7, prev_secret_key_version=$8,
			overlap_until=$9, include_token=$10, enabled=$11, updated_at=$12
		WHERE id=$1`,
		ep.ID, ep.URL, ep.Description, ep.Events,
		ep.SecretCiphertext, ep.SecretKeyVersion,
		nullBytes(ep.PrevSecretCiphertext), ep.PrevSecretKeyVersion,
		ep.OverlapUntil, ep.IncludeToken, ep.Enabled, ep.UpdatedAt.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const webhookDeliveryCols = `id, endpoint_id, partner_id, event_id, event_type, transaction_id,
	payload, status, attempt_count, next_attempt_at, first_attempt_at, last_attempt_at,
	response_code, error_class, created_at, updated_at`

func scanWebhookDelivery(row interface{ Scan(dest ...any) error }) (partners.WebhookDelivery, error) {
	var d partners.WebhookDelivery
	var txnID *uuid.UUID
	var payload []byte
	err := row.Scan(
		&d.ID, &d.EndpointID, &d.PartnerID, &d.EventID, &d.EventType, &txnID,
		&payload, &d.Status, &d.AttemptCount, &d.NextAttemptAt, &d.FirstAttemptAt, &d.LastAttemptAt,
		&d.ResponseCode, &d.ErrorClass, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return partners.WebhookDelivery{}, mapErr(err)
	}
	if txnID != nil {
		d.TransactionID = *txnID
	}
	if len(payload) == 0 {
		d.Payload = json.RawMessage(`{}`)
	} else {
		d.Payload = payload
	}
	d.NextAttemptAt = d.NextAttemptAt.UTC()
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	if d.FirstAttemptAt != nil {
		t := d.FirstAttemptAt.UTC()
		d.FirstAttemptAt = &t
	}
	if d.LastAttemptAt != nil {
		t := d.LastAttemptAt.UTC()
		d.LastAttemptAt = &t
	}
	return d, nil
}

func nullTxn(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (r partnerRepo) InsertWebhookDelivery(ctx context.Context, d partners.WebhookDelivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	payload := d.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO partner_webhook_deliveries (
			id, endpoint_id, partner_id, event_id, event_type, transaction_id,
			payload, status, attempt_count, next_attempt_at, first_attempt_at, last_attempt_at,
			response_code, error_class, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (endpoint_id, event_id) DO NOTHING`,
		d.ID, d.EndpointID, d.PartnerID, d.EventID, d.EventType, nullTxn(d.TransactionID),
		payload, d.Status, d.AttemptCount, d.NextAttemptAt.UTC(), d.FirstAttemptAt, d.LastAttemptAt,
		d.ResponseCode, d.ErrorClass, d.CreatedAt.UTC(), d.UpdatedAt.UTC())
	return mapErr(err)
}

func (r partnerRepo) GetWebhookDelivery(ctx context.Context, id uuid.UUID) (partners.WebhookDelivery, error) {
	return scanWebhookDelivery(r.db.QueryRow(ctx, `SELECT `+webhookDeliveryCols+` FROM partner_webhook_deliveries WHERE id=$1`, id))
}

func (r partnerRepo) GetWebhookDeliveryForUpdate(ctx context.Context, id uuid.UUID) (partners.WebhookDelivery, error) {
	return scanWebhookDelivery(r.db.QueryRow(ctx, `SELECT `+webhookDeliveryCols+` FROM partner_webhook_deliveries WHERE id=$1 FOR UPDATE`, id))
}

func (r partnerRepo) ListWebhookDeliveries(ctx context.Context, partnerID, endpointID uuid.UUID, limit int) ([]partners.WebhookDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT `+webhookDeliveryCols+` FROM partner_webhook_deliveries
		WHERE partner_id=$1 AND endpoint_id=$2 ORDER BY created_at DESC LIMIT $3`, partnerID, endpointID, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]partners.WebhookDelivery, 0)
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r partnerRepo) ListDueWebhookDeliveries(ctx context.Context, now, staleDeliveringBefore time.Time, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id FROM partner_webhook_deliveries
		WHERE (status IN ('PENDING', 'FAILED') AND next_attempt_at <= $1)
		   OR (status = 'DELIVERING' AND (last_attempt_at IS NULL OR last_attempt_at <= $2))
		ORDER BY next_attempt_at, id
		LIMIT $3`, now.UTC(), staleDeliveringBefore.UTC(), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r partnerRepo) UpdateWebhookDelivery(ctx context.Context, d partners.WebhookDelivery) error {
	payload := d.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE partner_webhook_deliveries SET
			payload=$2, status=$3, attempt_count=$4, next_attempt_at=$5,
			first_attempt_at=$6, last_attempt_at=$7, response_code=$8, error_class=$9, updated_at=$10
		WHERE id=$1`,
		d.ID, payload, d.Status, d.AttemptCount, d.NextAttemptAt.UTC(),
		d.FirstAttemptAt, d.LastAttemptAt, d.ResponseCode, d.ErrorClass, d.UpdatedAt.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
