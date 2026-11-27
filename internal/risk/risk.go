// Package risk is the P3 abuse-control engine.
//
// Decisions are ALLOW, REVIEW (capture payment, hold vending) or DENY
// (refuse before checkout). After money has moved, DENY is coerced to
// REVIEW so funds are never stranded. Observations and decisions store
// HMACs and metadata only — never raw emails, phones, IPs, meter numbers
// or tokens.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrDenied   = errors.New("risk: declined")
	ErrNotFound = errors.New("risk: not found")
)

type Outcome string

const (
	Allow  Outcome = "ALLOW"
	Review Outcome = "REVIEW"
	Deny   Outcome = "DENY"
)

type Surface string

const (
	SurfacePurchase  Surface = "purchase"
	SurfacePayForMe  Surface = "pay_for_me"
	SurfacePot       Surface = "pot"
	SurfaceSaveCard  Surface = "save_card"
	SurfaceAutoPower Surface = "autopower"
	SurfaceTokenSend Surface = "token_send"
)

type Kind string

const (
	KindCheckout      Kind = "checkout"
	KindPaymentOK     Kind = "payment_ok"
	KindPaymentFailed Kind = "payment_failed"
	KindCardSeen      Kind = "card_seen"
)

const (
	RuleVelocityUser       = "velocity_user"
	RuleVelocityCard       = "velocity_card"
	RuleVelocityEmail      = "velocity_email"
	RuleVelocityIP         = "velocity_ip"
	RuleVelocityMeter      = "velocity_meter"
	RuleVelocityDevice     = "velocity_device"
	RuleCardTesting        = "card_testing"
	RuleFirstPurchaseCap   = "first_purchase_cap"
	RuleAmountAnomaly      = "amount_anomaly"
	RulePublicLinkFailures = "public_link_failures"
	RuleDenylist           = "denylist"
	RuleLinkPaused         = "link_paused"
)

type DenyKind string

const (
	DenyCard    DenyKind = "CARD"
	DenyEmail   DenyKind = "EMAIL"
	DenyPhone   DenyKind = "PHONE"
	DenyIPRange DenyKind = "IP_RANGE"
)

type Rule struct {
	Code          string
	Enabled       bool
	Action        Outcome
	WindowSeconds int
	Threshold     int64
	Params        RuleParams
	UpdatedAt     time.Time
	UpdatedBy     string
}

type RuleParams struct {
	Multiplier int64 `json:"multiplier,omitempty"`
	MinHistory int64 `json:"min_history,omitempty"`
}

func (p RuleParams) Marshal() []byte {
	b, _ := json.Marshal(p)
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

func ParseParams(raw []byte) RuleParams {
	var p RuleParams
	if len(raw) == 0 {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

type Request struct {
	Surface       Surface
	UserID        string
	MeterID       uuid.UUID
	LinkID        uuid.UUID
	TransactionID uuid.UUID
	AmountMinor   int64
	EmailHMAC     string
	PhoneHMAC     string
	IPHMAC        string
	DeviceHMAC    string
	CardSignature string
	// IP is the raw client address used only to match IP_RANGE denylist
	// entries. It is never persisted.
	IP   string
	Paid bool
}

type Observation struct {
	ID            uuid.UUID
	ObservedAt    time.Time
	Kind          Kind
	Surface       Surface
	UserID        string
	MeterID       uuid.UUID
	LinkID        uuid.UUID
	TransactionID uuid.UUID
	EmailHMAC     string
	IPHMAC        string
	DeviceHMAC    string
	PhoneHMAC     string
	CardSignature string
	AmountMinor   int64
}

type Decision struct {
	ID            uuid.UUID
	CreatedAt     time.Time
	Outcome       Outcome
	RuleCode      string
	Surface       Surface
	UserID        string
	MeterID       uuid.UUID
	LinkID        uuid.UUID
	TransactionID uuid.UUID
	AmountMinor   int64
	EmailHMAC     string
	IPHMAC        string
	DeviceHMAC    string
	CardSignature string
}

func (d Decision) ReviewReason() string {
	if d.RuleCode == "" {
		return "risk:review"
	}
	return "risk:" + d.RuleCode
}

type DenyEntry struct {
	ID        uuid.UUID
	Kind      DenyKind
	Value     string
	Reason    string
	CreatedAt time.Time
	CreatedBy string
}

type LinkPause struct {
	LinkID   uuid.UUID
	RuleCode string
	Reason   string
	PausedAt time.Time
}

type ObservationFilter struct {
	Kind          Kind
	UserID        string
	MeterID       uuid.UUID
	LinkID        uuid.UUID
	EmailHMAC     string
	IPHMAC        string
	DeviceHMAC    string
	CardSignature string
	Since         time.Time
}

type Repo interface {
	ListRules(ctx context.Context) ([]Rule, error)
	GetRule(ctx context.Context, code string) (Rule, error)
	UpdateRule(ctx context.Context, rule Rule) error

	InsertObservation(ctx context.Context, o Observation) error
	CountObservations(ctx context.Context, f ObservationFilter) (int, error)
	DistinctCards(ctx context.Context, f ObservationFilter) (int, error)
	RecentAmounts(ctx context.Context, userID string, limit int) ([]int64, error)

	InsertDecision(ctx context.Context, d Decision) error
	BindDecisionTxn(ctx context.Context, id, txnID uuid.UUID) error
	ListDecisionsByTxn(ctx context.Context, txnID uuid.UUID) ([]Decision, error)

	ListDenylist(ctx context.Context) ([]DenyEntry, error)
	AddDenylist(ctx context.Context, e DenyEntry) error
	RemoveDenylist(ctx context.Context, id uuid.UUID) error
	MatchDenylist(ctx context.Context, kind DenyKind, value string) (DenyEntry, error)
	ListIPRanges(ctx context.Context) ([]DenyEntry, error)

	PauseLink(ctx context.Context, p LinkPause) error
	LinkPause(ctx context.Context, linkID uuid.UUID) (LinkPause, error)
	ClearLinkPause(ctx context.Context, linkID uuid.UUID) error
}

// DefaultRules are the seeded thresholds. Operators may tune them in DB.
func DefaultRules() []Rule {
	return []Rule{
		{Code: RuleVelocityUser, Enabled: true, Action: Deny, WindowSeconds: 3600, Threshold: 8},
		{Code: RuleVelocityCard, Enabled: true, Action: Review, WindowSeconds: 3600, Threshold: 5},
		{Code: RuleVelocityEmail, Enabled: true, Action: Deny, WindowSeconds: 3600, Threshold: 8},
		{Code: RuleVelocityIP, Enabled: true, Action: Review, WindowSeconds: 3600, Threshold: 20},
		{Code: RuleVelocityMeter, Enabled: true, Action: Review, WindowSeconds: 3600, Threshold: 8},
		{Code: RuleVelocityDevice, Enabled: true, Action: Review, WindowSeconds: 3600, Threshold: 12},
		{Code: RuleCardTesting, Enabled: true, Action: Review, WindowSeconds: 3600, Threshold: 3},
		{Code: RuleFirstPurchaseCap, Enabled: true, Action: Deny, Threshold: 5_000_000},
		{Code: RuleAmountAnomaly, Enabled: true, Action: Review, Params: RuleParams{Multiplier: 8, MinHistory: 5}},
		{Code: RulePublicLinkFailures, Enabled: true, Action: Deny, WindowSeconds: 900, Threshold: 8},
	}
}

func ruleByCode(rules []Rule, code string) (Rule, bool) {
	for _, r := range rules {
		if r.Code == code {
			return r, true
		}
	}
	return Rule{}, false
}
