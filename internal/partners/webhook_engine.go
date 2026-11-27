package partners

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
)

func (e *Engine) repo(repo Repo) Repo {
	if repo != nil {
		return repo
	}
	return e.Repo
}

func (e *Engine) guard() *Guard {
	if e != nil && e.Guard != nil {
		return e.Guard
	}
	return &Guard{}
}

func webhookSecretContext(id uuid.UUID) []byte {
	return []byte("partner-webhook:" + id.String())
}

func (e *Engine) encryptSecret(id uuid.UUID, full string) ([]byte, string, error) {
	if e == nil || e.Box == nil {
		return nil, "", ErrUnauthenticated
	}
	ct, err := e.Box.EncryptBound(full, webhookSecretContext(id))
	if err != nil {
		return nil, "", err
	}
	return ct, e.Box.KeyVersion, nil
}

func (e *Engine) decryptSecret(id uuid.UUID, ct []byte) (string, error) {
	if e == nil || e.Box == nil || len(ct) == 0 {
		return "", ErrInvalidInput
	}
	return e.Box.DecryptBound(ct, webhookSecretContext(id))
}

func (e *Engine) secretsFor(ep WebhookEndpoint) []string {
	now := e.now()
	var out []string
	if s, err := e.decryptSecret(ep.ID, ep.SecretCiphertext); err == nil && s != "" {
		out = append(out, s)
	}
	if len(ep.PrevSecretCiphertext) > 0 && ep.OverlapUntil != nil && ep.OverlapUntil.After(now) {
		if s, err := e.decryptSecret(ep.ID, ep.PrevSecretCiphertext); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (e *Engine) CreateWebhook(ctx context.Context, repo Repo, partnerID uuid.UUID, rawURL, description string, eventTypes []string, includeToken bool) (WebhookEndpoint, string, error) {
	repo = e.repo(repo)
	if repo == nil {
		return WebhookEndpoint{}, "", ErrInvalidInput
	}
	p, err := repo.GetPartner(ctx, partnerID)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	if p.Status != StatusActive {
		if p.Status == StatusSuspended {
			return WebhookEndpoint{}, "", ErrSuspended
		}
		return WebhookEndpoint{}, "", ErrNotActive
	}
	eventTypes, err = NormalizeWebhookEvents(eventTypes)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	description = strings.TrimSpace(description)
	if utf8.RuneCountInString(description) > 200 {
		return WebhookEndpoint{}, "", ErrInvalidInput
	}
	if _, err := e.guard().Check(ctx, rawURL); err != nil {
		return WebhookEndpoint{}, "", err
	}
	existing, err := repo.ListWebhookEndpoints(ctx, partnerID)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	if len(existing) >= MaxWebhookEndpoints {
		return WebhookEndpoint{}, "", ErrWebhookLimit
	}
	full, _, err := GenerateWebhookSecret()
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	now := e.now()
	ep := WebhookEndpoint{
		ID:           uuid.New(),
		PartnerID:    partnerID,
		URL:          rawURL,
		Description:  description,
		Events:       eventTypes,
		IncludeToken: includeToken,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	ct, ver, err := e.encryptSecret(ep.ID, full)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	ep.SecretCiphertext = ct
	ep.SecretKeyVersion = ver
	if err := repo.CreateWebhookEndpoint(ctx, ep); err != nil {
		return WebhookEndpoint{}, "", err
	}
	return ep, full, nil
}

func (e *Engine) UpdateWebhook(ctx context.Context, repo Repo, partnerID, id uuid.UUID, rawURL *string, description *string, eventTypes []string, includeToken *bool, enabled *bool) (WebhookEndpoint, error) {
	repo = e.repo(repo)
	ep, err := repo.GetWebhookEndpointForUpdate(ctx, id)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	if ep.PartnerID != partnerID {
		return WebhookEndpoint{}, ErrNotFound
	}
	if rawURL != nil {
		if _, err := e.guard().Check(ctx, *rawURL); err != nil {
			return WebhookEndpoint{}, err
		}
		ep.URL = *rawURL
	}
	if description != nil {
		d := strings.TrimSpace(*description)
		if utf8.RuneCountInString(d) > 200 {
			return WebhookEndpoint{}, ErrInvalidInput
		}
		ep.Description = d
	}
	if eventTypes != nil {
		ev, err := NormalizeWebhookEvents(eventTypes)
		if err != nil {
			return WebhookEndpoint{}, err
		}
		ep.Events = ev
	}
	if includeToken != nil {
		ep.IncludeToken = *includeToken
	}
	if enabled != nil {
		ep.Enabled = *enabled
	}
	ep.UpdatedAt = e.now()
	if err := repo.UpdateWebhookEndpoint(ctx, ep); err != nil {
		return WebhookEndpoint{}, err
	}
	return ep, nil
}

func (e *Engine) DisableWebhook(ctx context.Context, repo Repo, partnerID, id uuid.UUID) (WebhookEndpoint, error) {
	off := false
	return e.UpdateWebhook(ctx, repo, partnerID, id, nil, nil, nil, nil, &off)
}

func (e *Engine) RotateWebhook(ctx context.Context, repo Repo, partnerID, id uuid.UUID) (WebhookEndpoint, string, error) {
	repo = e.repo(repo)
	ep, err := repo.GetWebhookEndpointForUpdate(ctx, id)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	if ep.PartnerID != partnerID {
		return WebhookEndpoint{}, "", ErrNotFound
	}
	full, _, err := GenerateWebhookSecret()
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	now := e.now()
	ep.PrevSecretCiphertext = ep.SecretCiphertext
	ep.PrevSecretKeyVersion = ep.SecretKeyVersion
	until := now.Add(e.overlap())
	ep.OverlapUntil = &until
	ct, ver, err := e.encryptSecret(ep.ID, full)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	ep.SecretCiphertext = ct
	ep.SecretKeyVersion = ver
	ep.UpdatedAt = now
	if err := repo.UpdateWebhookEndpoint(ctx, ep); err != nil {
		return WebhookEndpoint{}, "", err
	}
	return ep, full, nil
}

func (e *Engine) Enqueue(ctx context.Context, repo Repo, ev events.Event, snap PurchaseSnapshot) error {
	repo = e.repo(repo)
	typ := MapOutbox(ev.Type)
	if typ == "" || snap.PartnerID == uuid.Nil {
		return nil
	}
	eps, err := repo.ListWebhookEndpoints(ctx, snap.PartnerID)
	if err != nil {
		return err
	}
	payload, err := BuildPayload(ev, typ, snap)
	if err != nil {
		return err
	}
	now := e.now()
	for _, ep := range eps {
		if !ep.Enabled || !ep.Subscribes(typ) {
			continue
		}
		d := WebhookDelivery{
			ID:            uuid.New(),
			EndpointID:    ep.ID,
			PartnerID:     snap.PartnerID,
			EventID:       ev.ID,
			EventType:     typ,
			TransactionID: snap.ID,
			Payload:       payload,
			Status:        DeliveryPending,
			NextAttemptAt: now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := repo.InsertWebhookDelivery(ctx, d); err != nil {
			return err
		}
	}
	return nil
}
