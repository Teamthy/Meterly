// Package partners is the identity and access layer for approved Meterly
// partners (B4), the B5 float subledger, and B6 outbound webhooks.
package partners

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	StatusPending   = "PENDING"
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"

	EnvSandbox    = "SANDBOX"
	EnvProduction = "PRODUCTION"

	PrefixTest = "mtr_test_"
	PrefixLive = "mtr_live_"

	ScopeMetersValidate  = "meters:validate"
	ScopeQuotesCreate    = "quotes:create"
	ScopePurchasesCreate = "purchases:create"
	ScopePurchasesRead   = "purchases:read"
	ScopeWebhooksManage  = "webhooks:manage"
	ScopeFloatRead       = "float:read"

	DefaultRatePerMinute = 60
	DefaultOverlap       = 24 * time.Hour
	LookupSecretChars    = 8
	SecretBytes          = 32

	MaxWebhookEndpoints = 10
	MaxWebhookURLBytes  = 2048
	WebhookSecretPrefix = "whsec_"
	WebhookReplayWindow = 5 * time.Minute
	WebhookTimeout      = 5 * time.Second
	WebhookMaxBody      = 64 << 10
	WebhookRetryHorizon = 24 * time.Hour
	WebhookStaleAfter   = 30 * time.Second

	EventPaymentConfirmed = "purchase.payment_confirmed"
	EventVended           = "purchase.vended"
	EventDelivered        = "purchase.delivered"
	EventFailed           = "purchase.failed"
	EventRefunded         = "purchase.refunded"
	EventUnknown          = "purchase.unknown"
	EventPing             = "endpoint.ping"

	DeliveryPending    = "PENDING"
	DeliveryDelivering = "DELIVERING"
	DeliveryDelivered  = "DELIVERED"
	DeliveryFailed     = "FAILED"
	DeliveryDeadLetter = "DEAD_LETTER"
)

// AllScopes is the closed set of partner scopes. B5/B6 bind these to routes.
var AllScopes = []string{
	ScopeMetersValidate,
	ScopeQuotesCreate,
	ScopePurchasesCreate,
	ScopePurchasesRead,
	ScopeWebhooksManage,
	ScopeFloatRead,
}

var (
	ErrNotFound        = errors.New("partners: not found")
	ErrSameApprover    = errors.New("partners: proposer cannot approve")
	ErrNotPending      = errors.New("partners: not pending activation")
	ErrNotActive       = errors.New("partners: partner is not active")
	ErrSuspended       = errors.New("partners: partner is suspended")
	ErrInvalidScope    = errors.New("partners: invalid scope")
	ErrInvalidEnv      = errors.New("partners: invalid environment")
	ErrAlreadyActive   = errors.New("partners: already active")
	ErrInvalidKey      = errors.New("partners: invalid api key")
	ErrKeyRevoked      = errors.New("partners: api key revoked")
	ErrKeyExpired      = errors.New("partners: api key expired")
	ErrEnvMismatch     = errors.New("partners: key environment mismatch")
	ErrUnauthenticated = errors.New("partners: unauthenticated")
	ErrInvalidInput    = errors.New("partners: invalid input")
	ErrReceiptConflict = errors.New("partners: receipt reference conflict")
	ErrNotPendingTopup = errors.New("partners: top-up is not pending")
	ErrFloatConflict   = errors.New("partners: concurrent float update")
	ErrBlockedURL      = errors.New("partners: webhook url is not allowed")
	ErrNotHTTPS        = errors.New("partners: webhook url must be https")
	ErrWebhookLimit    = errors.New("partners: webhook endpoint limit reached")
	ErrRedirect        = errors.New("partners: webhook redirects are not followed")
)

type Partner struct {
	ID                   uuid.UUID
	Organisation         string
	Status               string
	Environment          string
	ContactRef           string
	DailyValueCapMinor   int64
	RateLimitPerMinute   int
	CreatedBy            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	ProposedActivationBy string
	ProposedActivationAt *time.Time
	ActivatedBy          string
	ActivatedAt          *time.Time
	SuspendedBy          string
	SuspendedAt          *time.Time
}

type APIKey struct {
	ID           uuid.UUID
	PartnerID    uuid.UUID
	Prefix       string
	SecretHash   string
	Environment  string
	Scopes       []string
	CreatedBy    string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
	RevokedBy    string
	OverlapUntil *time.Time
	RotatedFrom  *uuid.UUID
}

type Usage struct {
	PartnerID    uuid.UUID
	Day          time.Time // UTC date
	RequestCount int64
	ValueMinor   int64
}

// Identity is a verified partner caller. Mapped to auth.Principal by PartnerThen.
type Identity struct {
	Partner Partner
	Key     APIKey
}

type Repo interface {
	CreatePartner(ctx context.Context, p Partner) error
	GetPartner(ctx context.Context, id uuid.UUID) (Partner, error)
	ListPartners(ctx context.Context, limit int) ([]Partner, error)
	UpdatePartner(ctx context.Context, p Partner) error

	InsertKey(ctx context.Context, k APIKey) error
	GetKey(ctx context.Context, id uuid.UUID) (APIKey, error)
	GetKeyByPrefix(ctx context.Context, prefix string) (APIKey, error)
	ListKeys(ctx context.Context, partnerID uuid.UUID) ([]APIKey, error)
	UpdateKey(ctx context.Context, k APIKey) error
	TouchKeyUsed(ctx context.Context, id uuid.UUID, at time.Time) error

	IncrementUsage(ctx context.Context, partnerID uuid.UUID, day time.Time, requests, valueMinor int64) error
	ListUsage(ctx context.Context, partnerID uuid.UUID, from, to time.Time) ([]Usage, error)
	GetUsage(ctx context.Context, partnerID uuid.UUID, day time.Time) (Usage, error)

	GetFloat(ctx context.Context, partnerID uuid.UUID) (Float, error)
	GetFloatForUpdate(ctx context.Context, partnerID uuid.UUID) (Float, error)
	EnsureFloatForUpdate(ctx context.Context, partnerID uuid.UUID, now time.Time) (Float, error)
	UpdateFloat(ctx context.Context, f Float) error

	CreateTopup(ctx context.Context, t FloatTopup) error
	GetTopup(ctx context.Context, id uuid.UUID) (FloatTopup, error)
	GetTopupForUpdate(ctx context.Context, id uuid.UUID) (FloatTopup, error)
	GetTopupByReceipt(ctx context.Context, receiptRef string) (FloatTopup, error)
	UpdateTopup(ctx context.Context, t FloatTopup) error

	CreateWebhookEndpoint(ctx context.Context, ep WebhookEndpoint) error
	GetWebhookEndpoint(ctx context.Context, id uuid.UUID) (WebhookEndpoint, error)
	GetWebhookEndpointForUpdate(ctx context.Context, id uuid.UUID) (WebhookEndpoint, error)
	ListWebhookEndpoints(ctx context.Context, partnerID uuid.UUID) ([]WebhookEndpoint, error)
	UpdateWebhookEndpoint(ctx context.Context, ep WebhookEndpoint) error

	InsertWebhookDelivery(ctx context.Context, d WebhookDelivery) error
	GetWebhookDelivery(ctx context.Context, id uuid.UUID) (WebhookDelivery, error)
	GetWebhookDeliveryForUpdate(ctx context.Context, id uuid.UUID) (WebhookDelivery, error)
	ListWebhookDeliveries(ctx context.Context, partnerID, endpointID uuid.UUID, limit int) ([]WebhookDelivery, error)
	ListDueWebhookDeliveries(ctx context.Context, now, staleDeliveringBefore time.Time, limit int) ([]uuid.UUID, error)
	UpdateWebhookDelivery(ctx context.Context, d WebhookDelivery) error
}

// Route describes a partner API route and the scope it will require.
// B4 enforces this catalogue in tests; B5 mounts the purchase routes.
type Route struct {
	Method string
	Path   string
	Scope  string
}

// RouteCatalog is every partner route × required scope. GET /v1/partner/me
// needs a valid key only. The rest are B5/B6 surfaces reserved here so the
// scope matrix exists before those batches land.
func RouteCatalog() []Route {
	return []Route{
		{Method: "GET", Path: "/v1/partner/me", Scope: ""},
		{Method: "POST", Path: "/v1/partner/meters/validate", Scope: ScopeMetersValidate},
		{Method: "POST", Path: "/v1/partner/quotes", Scope: ScopeQuotesCreate},
		{Method: "POST", Path: "/v1/partner/purchases", Scope: ScopePurchasesCreate},
		{Method: "GET", Path: "/v1/partner/purchases/{id}", Scope: ScopePurchasesRead},
		{Method: "GET", Path: "/v1/partner/purchases", Scope: ScopePurchasesRead},
		{Method: "GET", Path: "/v1/partner/float", Scope: ScopeFloatRead},
		{Method: "POST", Path: "/v1/partner/webhooks", Scope: ScopeWebhooksManage},
		{Method: "GET", Path: "/v1/partner/webhooks", Scope: ScopeWebhooksManage},
		{Method: "GET", Path: "/v1/partner/webhooks/{id}", Scope: ScopeWebhooksManage},
		{Method: "PATCH", Path: "/v1/partner/webhooks/{id}", Scope: ScopeWebhooksManage},
		{Method: "DELETE", Path: "/v1/partner/webhooks/{id}", Scope: ScopeWebhooksManage},
		{Method: "POST", Path: "/v1/partner/webhooks/{id}/rotate", Scope: ScopeWebhooksManage},
		{Method: "POST", Path: "/v1/partner/webhooks/{id}/ping", Scope: ScopeWebhooksManage},
		{Method: "GET", Path: "/v1/partner/webhooks/{id}/deliveries", Scope: ScopeWebhooksManage},
		{Method: "POST", Path: "/v1/partner/webhooks/{id}/deliveries/{delivery_id}/redeliver", Scope: ScopeWebhooksManage},
	}
}

func ValidEnvironment(env string) bool {
	return env == EnvSandbox || env == EnvProduction
}

func ValidStatus(st string) bool {
	return st == StatusPending || st == StatusActive || st == StatusSuspended
}

func KnownScope(s string) bool {
	for _, x := range AllScopes {
		if x == s {
			return true
		}
	}
	return false
}

func NormalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, ErrInvalidScope
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !KnownScope(s) {
			return nil, ErrInvalidScope
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, ErrInvalidScope
	}
	return out, nil
}

func HasScope(have []string, need string) bool {
	if need == "" {
		return true
	}
	for _, s := range have {
		if s == need {
			return true
		}
	}
	return false
}

func PrefixForEnv(env string) (string, error) {
	switch env {
	case EnvSandbox:
		return PrefixTest, nil
	case EnvProduction:
		return PrefixLive, nil
	}
	return "", ErrInvalidEnv
}

func EnvForPrefix(token string) (string, string, bool) {
	switch {
	case len(token) > len(PrefixTest) && token[:len(PrefixTest)] == PrefixTest:
		return EnvSandbox, PrefixTest, true
	case len(token) > len(PrefixLive) && token[:len(PrefixLive)] == PrefixLive:
		return EnvProduction, PrefixLive, true
	}
	return "", "", false
}
