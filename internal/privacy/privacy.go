// Package privacy is Meterly's NDPA foundation: a machine-readable data map,
// per-table retention policies, a batched purge job, and the consent ledger.
package privacy

import (
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/notifications"
)

const (
	PurposeTransactional = "TRANSACTIONAL"
	PurposeMarketing     = "MARKETING"

	StatusGranted   = "GRANTED"
	StatusWithdrawn = "WITHDRAWN"

	SourceUser        = "user"
	SourceDefault     = "default"
	SourceInboundStop = "inbound_stop"
	SourcePreferences = "preferences"

	ActionDelete    = "DELETE"
	ActionAnonymise = "ANONYMISE"

	BasisContract           = "contract"
	BasisConsent            = "consent"
	BasisLegitimateInterest = "legitimate_interest"
	BasisLegalObligation    = "legal_obligation"
	BasisUnused             = "unused"

	StoragePlaintext = "plaintext"
	StorageEncrypted = "encrypted"
	StorageHMAC      = "hmac"
	StorageUnused    = "unused"
)

var channels = []notifications.Channel{notifications.SMS, notifications.Email, notifications.WhatsApp}

// Column describes one schema column that looks like personal data.
type Column struct {
	Table     string
	Column    string
	Personal  bool
	What      string
	Purpose   string
	Basis     string
	Retention string
	Storage   string
}

func (c Column) Key() string { return c.Table + "." + c.Column }

// Policy is a per-table retention rule. Durations are overridable via env.
type Policy struct {
	Table      string
	Action     string // DELETE or ANONYMISE
	Duration   time.Duration
	BatchSize  int
	Financial  bool
	LegalHold  bool // skip rows whose user is on an active legal hold
	CutoffFrom string
}

// Record is one append-only consent event.
type Record struct {
	ID        uuid.UUID
	UserID    string
	Purpose   string
	Channel   notifications.Channel
	Status    string
	Source    string
	CreatedAt time.Time
}

// Hold pauses retention deletes for one user until ReleasedAt is set.
type Hold struct {
	ID         uuid.UUID
	UserID     string
	Reason     string
	CreatedAt  time.Time
	CreatedBy  string
	ReleasedAt *time.Time
}

// RetentionRun is one retention-job execution (counts only, no PII).
type RetentionRun struct {
	ID         uuid.UUID
	StartedAt  time.Time
	FinishedAt *time.Time
	DryRun     bool
	Status     string
	Counts     map[string]int
	Error      string
}

// Settings are process defaults; all durations come from config/env.
type Settings struct {
	OTP              time.Duration
	Challenge        time.Duration
	Session          time.Duration
	Idempotency      time.Duration
	RiskObservation  time.Duration
	Support          time.Duration
	Financial        time.Duration
	NotificationHMAC time.Duration
	BatchSize        int
	DryRun           bool
}

func DefaultSettings() Settings {
	return Settings{
		OTP:              7 * 24 * time.Hour,
		Challenge:        7 * 24 * time.Hour,
		Session:          90 * 24 * time.Hour,
		Idempotency:      30 * 24 * time.Hour,
		RiskObservation:  90 * 24 * time.Hour,
		Support:          730 * 24 * time.Hour, // 2 years; founder/legal to confirm
		Financial:        7 * 365 * 24 * time.Hour,
		NotificationHMAC: 365 * 24 * time.Hour,
		BatchSize:        500,
	}
}

func (s Settings) withDefaults() Settings {
	d := DefaultSettings()
	if s.OTP <= 0 {
		s.OTP = d.OTP
	}
	if s.Challenge <= 0 {
		s.Challenge = d.Challenge
	}
	if s.Session <= 0 {
		s.Session = d.Session
	}
	if s.Idempotency <= 0 {
		s.Idempotency = d.Idempotency
	}
	if s.RiskObservation <= 0 {
		s.RiskObservation = d.RiskObservation
	}
	if s.Support <= 0 {
		s.Support = d.Support
	}
	if s.Financial <= 0 {
		s.Financial = d.Financial
	}
	if s.NotificationHMAC <= 0 {
		s.NotificationHMAC = d.NotificationHMAC
	}
	if s.BatchSize <= 0 {
		s.BatchSize = d.BatchSize
	}
	return s
}

// Policies returns the retention registry. Financial rows are ANONYMISE
// (no-op until B3) so the B1 job never deletes books inside the period.
func Policies(s Settings) []Policy {
	s = s.withDefaults()
	b := s.BatchSize
	return []Policy{
		{Table: "channel_otps", Action: ActionDelete, Duration: s.OTP, BatchSize: b, LegalHold: true, CutoffFrom: "created_at"},
		{Table: "auth_challenges", Action: ActionDelete, Duration: s.Challenge, BatchSize: b, LegalHold: true, CutoffFrom: "expires_at"},
		{Table: "auth_sessions", Action: ActionDelete, Duration: s.Session, BatchSize: b, LegalHold: true, CutoffFrom: "expires_at"},
		{Table: "idempotency_keys", Action: ActionDelete, Duration: s.Idempotency, BatchSize: b, CutoffFrom: "created_at"},
		{Table: "risk_observations", Action: ActionDelete, Duration: s.RiskObservation, BatchSize: b, LegalHold: true, CutoffFrom: "observed_at"},
		{Table: "support_threads", Action: ActionDelete, Duration: s.Support, BatchSize: b, LegalHold: true, CutoffFrom: "updated_at"},
		{Table: "notification_deliveries", Action: ActionAnonymise, Duration: s.NotificationHMAC, BatchSize: b, CutoffFrom: "created_at"},
		{Table: "electricity_transactions", Action: ActionAnonymise, Duration: s.Financial, Financial: true, LegalHold: true, CutoffFrom: "created_at"},
		{Table: "payments", Action: ActionAnonymise, Duration: s.Financial, Financial: true, LegalHold: true, CutoffFrom: "created_at"},
		{Table: "ledger_entries", Action: ActionAnonymise, Duration: s.Financial, Financial: true, LegalHold: true, CutoffFrom: "created_at"},
		{Table: "refunds", Action: ActionAnonymise, Duration: s.Financial, Financial: true, LegalHold: true, CutoffFrom: "created_at"},
		{Table: "disputes", Action: ActionAnonymise, Duration: s.Financial, Financial: true, LegalHold: true, CutoffFrom: "created_at"},
	}
}
