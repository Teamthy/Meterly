package partners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"

	"github.com/google/uuid"
)

func (e *Engine) log() *slog.Logger {
	if e != nil && e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Engine) ProcessDue(ctx context.Context, limit int) (int, error) {
	if e == nil || e.Repo == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 50
	}
	now := e.now()
	ids, err := e.Repo.ListDueWebhookDeliveries(ctx, now, now.Add(-WebhookStaleAfter), limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return n, nil
		}
		ok, err := e.ProcessOne(ctx, id)
		if err != nil {
			e.log().Warn("partner webhook delivery failed", "delivery_id", id, "err", err)
			continue
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func (e *Engine) ProcessOne(ctx context.Context, id uuid.UUID) (bool, error) {
	var (
		d  WebhookDelivery
		ep WebhookEndpoint
	)
	claimed := false
	claim := func(repo Repo) error {
		var err error
		d, err = repo.GetWebhookDeliveryForUpdate(ctx, id)
		if err != nil {
			return err
		}
		now := e.now()
		switch d.Status {
		case DeliveryDelivered, DeliveryDeadLetter:
			return nil
		case DeliveryDelivering:
			if d.LastAttemptAt != nil && d.LastAttemptAt.After(now.Add(-WebhookStaleAfter)) {
				return nil
			}
		case DeliveryPending, DeliveryFailed:
			if d.NextAttemptAt.After(now) {
				return nil
			}
		default:
			return nil
		}
		ep, err = repo.GetWebhookEndpoint(ctx, d.EndpointID)
		if err != nil {
			return err
		}
		d.Status = DeliveryDelivering
		d.AttemptCount++
		t := now
		d.LastAttemptAt = &t
		if d.FirstAttemptAt == nil {
			d.FirstAttemptAt = &t
		}
		d.UpdatedAt = now
		if err := repo.UpdateWebhookDelivery(ctx, d); err != nil {
			return err
		}
		claimed = true
		return nil
	}
	if err := e.withTx(ctx, claim); err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	code, class, postErr := e.postDelivery(ctx, ep, d)
	return true, e.finishDelivery(ctx, d.ID, code, class, postErr)
}

func (e *Engine) withTx(ctx context.Context, fn func(Repo) error) error {
	if e.Runner != nil {
		return e.Runner.InTx(ctx, fn)
	}
	if e.Repo == nil {
		return ErrInvalidInput
	}
	return fn(e.Repo)
}

func (e *Engine) postDelivery(ctx context.Context, ep WebhookEndpoint, d WebhookDelivery) (int, string, error) {
	if !ep.Enabled {
		return 0, "DISABLED", ErrInvalidInput
	}
	body := append([]byte(nil), d.Payload...)
	if ep.IncludeToken && d.TransactionID != uuid.Nil && e.Token != nil {
		if tok, err := e.Token(ctx, ep.PartnerID, d.TransactionID); err == nil && tok != "" {
			if b, err := injectToken(body, tok); err == nil {
				body = b
			}
		}
	}
	secrets := e.secretsFor(ep)
	if len(secrets) == 0 {
		return 0, "SECRET", ErrInvalidInput
	}
	ts := e.now().Unix()
	sig, err := SignBody(secrets, ts, body)
	if err != nil {
		return 0, "SIGN", err
	}
	headers := map[string]string{
		"Meterly-Signature":  sig,
		"Meterly-Event-Id":   d.EventID.String(),
		"Meterly-Event-Type": d.EventType,
		"Meterly-Webhook-Id": ep.ID.String(),
		"Meterly-Timestamp":  strconv.FormatInt(ts, 10),
	}
	code, err := e.guard().Post(ctx, ep.URL, headers, body)
	if err != nil {
		class := "NETWORK"
		if errors.Is(err, ErrRedirect) {
			class = "REDIRECT"
		}
		if errors.Is(err, ErrBlockedURL) || errors.Is(err, ErrNotHTTPS) {
			class = "SSRF"
		}
		return 0, class, err
	}
	if code >= 200 && code < 300 {
		return code, "", nil
	}
	return code, "HTTP_" + strconv.Itoa(code), errors.New("webhook endpoint rejected delivery")
}

func injectToken(payload []byte, token string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
		body["data"] = data
	}
	data["token"] = token
	return json.Marshal(body)
}

func (e *Engine) finishDelivery(ctx context.Context, id uuid.UUID, code int, class string, postErr error) error {
	return e.withTx(ctx, func(repo Repo) error {
		d, err := repo.GetWebhookDeliveryForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if d.Status != DeliveryDelivering {
			return nil
		}
		now := e.now()
		d.ResponseCode = code
		d.UpdatedAt = now
		if postErr == nil {
			d.Status = DeliveryDelivered
			d.ErrorClass = ""
			d.NextAttemptAt = now
			return repo.UpdateWebhookDelivery(ctx, d)
		}
		d.ErrorClass = class
		first := now
		if d.FirstAttemptAt != nil {
			first = *d.FirstAttemptAt
		}
		dead, next := DeadLetterAfterFailure(first, now, d.AttemptCount)
		if dead {
			d.Status = DeliveryDeadLetter
			d.NextAttemptAt = now
		} else {
			d.Status = DeliveryFailed
			d.NextAttemptAt = next
		}
		return repo.UpdateWebhookDelivery(ctx, d)
	})
}

func (e *Engine) Ping(ctx context.Context, repo Repo, partnerID, endpointID uuid.UUID) (WebhookDelivery, error) {
	repo = e.repo(repo)
	ep, err := repo.GetWebhookEndpoint(ctx, endpointID)
	if err != nil {
		return WebhookDelivery{}, err
	}
	if ep.PartnerID != partnerID {
		return WebhookDelivery{}, ErrNotFound
	}
	if !ep.Enabled {
		return WebhookDelivery{}, ErrInvalidInput
	}
	now := e.now()
	evID := uuid.New()
	payload, err := BuildPingPayload(evID, ep.ID, now)
	if err != nil {
		return WebhookDelivery{}, err
	}
	d := WebhookDelivery{
		ID:            uuid.New(),
		EndpointID:    ep.ID,
		PartnerID:     partnerID,
		EventID:       evID,
		EventType:     EventPing,
		Payload:       payload,
		Status:        DeliveryPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := repo.InsertWebhookDelivery(ctx, d); err != nil {
		return WebhookDelivery{}, err
	}
	if _, err := e.ProcessOne(ctx, d.ID); err != nil {
		e.log().Warn("partner webhook ping post", "delivery_id", d.ID, "err", err)
	}
	out, err := repo.GetWebhookDelivery(ctx, d.ID)
	if err != nil {
		return d, err
	}
	return out, nil
}

func (e *Engine) Redeliver(ctx context.Context, repo Repo, partnerID, endpointID, deliveryID uuid.UUID) (WebhookDelivery, error) {
	repo = e.repo(repo)
	d, err := repo.GetWebhookDeliveryForUpdate(ctx, deliveryID)
	if err != nil {
		return WebhookDelivery{}, err
	}
	if d.PartnerID != partnerID || d.EndpointID != endpointID {
		return WebhookDelivery{}, ErrNotFound
	}
	now := e.now()
	d.Status = DeliveryPending
	d.NextAttemptAt = now
	d.FirstAttemptAt = &now
	d.AttemptCount = 0
	d.ErrorClass = ""
	d.ResponseCode = 0
	d.UpdatedAt = now
	if err := repo.UpdateWebhookDelivery(ctx, d); err != nil {
		return WebhookDelivery{}, err
	}
	return d, nil
}

func (e *Engine) GetWebhook(ctx context.Context, repo Repo, partnerID, id uuid.UUID) (WebhookEndpoint, error) {
	repo = e.repo(repo)
	ep, err := repo.GetWebhookEndpoint(ctx, id)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	if ep.PartnerID != partnerID {
		return WebhookEndpoint{}, ErrNotFound
	}
	return ep, nil
}

func (e *Engine) ListWebhooks(ctx context.Context, repo Repo, partnerID uuid.UUID) ([]WebhookEndpoint, error) {
	return e.repo(repo).ListWebhookEndpoints(ctx, partnerID)
}

func (e *Engine) ListDeliveries(ctx context.Context, repo Repo, partnerID, endpointID uuid.UUID, limit int) ([]WebhookDelivery, error) {
	repo = e.repo(repo)
	ep, err := repo.GetWebhookEndpoint(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	if ep.PartnerID != partnerID {
		return nil, ErrNotFound
	}
	return repo.ListWebhookDeliveries(ctx, partnerID, endpointID, limit)
}

// PayloadHasToken reports whether a JSON webhook body includes a token field.
// Tests use this; production never logs the body.
func PayloadHasToken(body []byte) bool {
	return bytes.Contains(body, []byte(`"token"`))
}
