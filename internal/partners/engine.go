package partners

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/secure"
)

type Engine struct {
	Repo        Repo
	Box         *secure.Box
	Environment string // process PROVIDER_ENVIRONMENT: SANDBOX | PRODUCTION
	Overlap     time.Duration
	Now         func() time.Time
	Guard       *Guard
	Runner      Runner
	Token       func(ctx context.Context, partnerID, txnID uuid.UUID) (string, error)
	Log         *slog.Logger
}

func (e *Engine) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) overlap() time.Duration {
	if e != nil && e.Overlap > 0 {
		return e.Overlap
	}
	return DefaultOverlap
}

func (e *Engine) processEnv() string {
	if e != nil && e.Environment != "" {
		return e.Environment
	}
	return EnvSandbox
}

func cloneScopes(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (e *Engine) Create(ctx context.Context, repo Repo, actorUserID, organisation, environment, contactRef string, capMinor int64, ratePerMin int) (Partner, error) {
	if repo == nil {
		repo = e.Repo
	}
	organisation = strings.TrimSpace(organisation)
	if organisation == "" || len(organisation) > 200 {
		return Partner{}, ErrInvalidInput
	}
	if !ValidEnvironment(environment) {
		return Partner{}, ErrInvalidEnv
	}
	contactRef = strings.TrimSpace(contactRef)
	if len(contactRef) > 200 {
		return Partner{}, ErrInvalidInput
	}
	if capMinor < 0 {
		return Partner{}, ErrInvalidInput
	}
	if ratePerMin <= 0 {
		ratePerMin = DefaultRatePerMinute
	}
	if ratePerMin > 10000 {
		return Partner{}, ErrInvalidInput
	}
	now := e.now()
	p := Partner{
		ID:                 uuid.New(),
		Organisation:       organisation,
		Environment:        environment,
		ContactRef:         contactRef,
		DailyValueCapMinor: capMinor,
		RateLimitPerMinute: ratePerMin,
		CreatedBy:          actorUserID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if environment == EnvSandbox {
		p.Status = StatusActive
		p.ActivatedBy = actorUserID
		t := now
		p.ActivatedAt = &t
	} else {
		p.Status = StatusPending
	}
	if err := repo.CreatePartner(ctx, p); err != nil {
		return Partner{}, err
	}
	return p, nil
}

func (e *Engine) Activate(ctx context.Context, repo Repo, actorUserID string, id uuid.UUID) (Partner, error) {
	if repo == nil {
		repo = e.Repo
	}
	p, err := repo.GetPartner(ctx, id)
	if err != nil {
		return Partner{}, err
	}
	if p.Status == StatusSuspended {
		return Partner{}, ErrSuspended
	}
	if p.Status == StatusActive {
		return Partner{}, ErrAlreadyActive
	}
	if p.Status != StatusPending {
		return Partner{}, ErrNotPending
	}
	now := e.now()
	if p.Environment == EnvSandbox {
		p.Status = StatusActive
		p.ActivatedBy = actorUserID
		p.ActivatedAt = &now
		p.UpdatedAt = now
		if err := repo.UpdatePartner(ctx, p); err != nil {
			return Partner{}, err
		}
		return p, nil
	}
	if p.ProposedActivationBy == "" {
		p.ProposedActivationBy = actorUserID
		p.ProposedActivationAt = &now
		p.UpdatedAt = now
		if err := repo.UpdatePartner(ctx, p); err != nil {
			return Partner{}, err
		}
		return p, nil
	}
	if p.ProposedActivationBy == actorUserID {
		return Partner{}, ErrSameApprover
	}
	p.Status = StatusActive
	p.ActivatedBy = actorUserID
	p.ActivatedAt = &now
	p.UpdatedAt = now
	if err := repo.UpdatePartner(ctx, p); err != nil {
		return Partner{}, err
	}
	return p, nil
}

func (e *Engine) Suspend(ctx context.Context, repo Repo, actorUserID string, id uuid.UUID) (Partner, error) {
	if repo == nil {
		repo = e.Repo
	}
	p, err := repo.GetPartner(ctx, id)
	if err != nil {
		return Partner{}, err
	}
	if p.Status == StatusSuspended {
		return p, nil
	}
	now := e.now()
	p.Status = StatusSuspended
	p.SuspendedBy = actorUserID
	p.SuspendedAt = &now
	p.UpdatedAt = now
	if err := repo.UpdatePartner(ctx, p); err != nil {
		return Partner{}, err
	}
	return p, nil
}

func (e *Engine) Issue(ctx context.Context, repo Repo, actorUserID string, partnerID uuid.UUID, scopes []string, expires *time.Time) (plaintext string, key APIKey, err error) {
	if repo == nil {
		repo = e.Repo
	}
	if e == nil || e.Box == nil {
		return "", APIKey{}, ErrUnauthenticated
	}
	p, err := repo.GetPartner(ctx, partnerID)
	if err != nil {
		return "", APIKey{}, err
	}
	if p.Status == StatusSuspended {
		return "", APIKey{}, ErrSuspended
	}
	if p.Status != StatusActive {
		return "", APIKey{}, ErrNotActive
	}
	scopes, err = NormalizeScopes(scopes)
	if err != nil {
		return "", APIKey{}, err
	}
	now := e.now()
	if expires != nil {
		exp := expires.UTC()
		if !exp.After(now) {
			return "", APIKey{}, ErrKeyExpired
		}
		expires = &exp
	}
	var last error
	for i := 0; i < 5; i++ {
		full, prefix, err := GenerateSecret(p.Environment)
		if err != nil {
			return "", APIKey{}, err
		}
		k := APIKey{
			ID:          uuid.New(),
			PartnerID:   p.ID,
			Prefix:      prefix,
			SecretHash:  HashSecret(e.Box, full),
			Environment: p.Environment,
			Scopes:      cloneScopes(scopes),
			CreatedBy:   actorUserID,
			CreatedAt:   now,
			ExpiresAt:   expires,
		}
		if err := repo.InsertKey(ctx, k); err != nil {
			last = err
			continue
		}
		return full, k, nil
	}
	if last == nil {
		last = ErrInvalidKey
	}
	return "", APIKey{}, last
}

func (e *Engine) Rotate(ctx context.Context, repo Repo, actorUserID string, partnerID, keyID uuid.UUID, scopes []string, overlap time.Duration) (plaintext string, next APIKey, err error) {
	if repo == nil {
		repo = e.Repo
	}
	old, err := repo.GetKey(ctx, keyID)
	if err != nil {
		return "", APIKey{}, err
	}
	if old.PartnerID != partnerID {
		return "", APIKey{}, ErrNotFound
	}
	if old.RevokedAt != nil {
		return "", APIKey{}, ErrKeyRevoked
	}
	if len(scopes) == 0 {
		scopes = old.Scopes
	}
	plain, next, err := e.Issue(ctx, repo, actorUserID, partnerID, scopes, old.ExpiresAt)
	if err != nil {
		return "", APIKey{}, err
	}
	from := old.ID
	next.RotatedFrom = &from
	if err := repo.UpdateKey(ctx, next); err != nil {
		return "", APIKey{}, err
	}
	if overlap <= 0 {
		overlap = e.overlap()
	}
	until := e.now().Add(overlap)
	old.OverlapUntil = &until
	if err := repo.UpdateKey(ctx, old); err != nil {
		return "", APIKey{}, err
	}
	return plain, next, nil
}

func (e *Engine) Revoke(ctx context.Context, repo Repo, actorUserID string, partnerID, keyID uuid.UUID) (APIKey, error) {
	if repo == nil {
		repo = e.Repo
	}
	k, err := repo.GetKey(ctx, keyID)
	if err != nil {
		return APIKey{}, err
	}
	if k.PartnerID != partnerID {
		return APIKey{}, ErrNotFound
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	now := e.now()
	k.RevokedAt = &now
	k.RevokedBy = actorUserID
	if err := repo.UpdateKey(ctx, k); err != nil {
		return APIKey{}, err
	}
	return k, nil
}

// Authenticate verifies a Bearer mtr_… token. Failures are indistinguishable
// (revoked, expired, suspended, wrong environment, bad MAC) except via typed
// errors for tests; HTTP maps them all to 401.
func (e *Engine) Authenticate(ctx context.Context, token string) (Identity, error) {
	if e == nil || e.Box == nil || e.Repo == nil {
		return Identity{}, ErrUnauthenticated
	}
	token = strings.TrimSpace(token)
	if err := ValidatePlaintext(token); err != nil {
		return Identity{}, ErrUnauthenticated
	}
	env, _, ok := EnvForPrefix(token)
	if !ok {
		return Identity{}, ErrUnauthenticated
	}
	prefix, err := LookupPrefix(token)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	computed := HashSecret(e.Box, token)
	k, err := e.Repo.GetKeyByPrefix(ctx, prefix)
	if err != nil {
		_ = EqualHash(computed, computed)
		return Identity{}, ErrUnauthenticated
	}
	if !EqualHash(k.SecretHash, computed) {
		return Identity{}, ErrUnauthenticated
	}
	if k.Environment != env {
		return Identity{}, ErrEnvMismatch
	}
	if k.Environment != e.processEnv() {
		return Identity{}, ErrEnvMismatch
	}
	now := e.now()
	if k.RevokedAt != nil && !k.RevokedAt.After(now) {
		return Identity{}, ErrKeyRevoked
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return Identity{}, ErrKeyExpired
	}
	if k.OverlapUntil != nil && !k.OverlapUntil.After(now) {
		return Identity{}, ErrKeyExpired
	}
	p, err := e.Repo.GetPartner(ctx, k.PartnerID)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	if p.Status == StatusSuspended {
		return Identity{}, ErrSuspended
	}
	if p.Status != StatusActive {
		return Identity{}, ErrNotActive
	}
	if p.Environment != k.Environment {
		return Identity{}, ErrEnvMismatch
	}
	return Identity{Partner: p, Key: k}, nil
}
