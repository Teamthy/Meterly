// Package store defines persistence contracts. All writes that belong
// together — a state transition, its history, its ledger posting and its
// outbox event — run inside one InTx call so they commit or roll back as a
// unit. Provider (network) calls must never happen inside InTx.
//
// Implementations: store/memstore (tests, local dev) and store/pgstore
// (PostgreSQL via pgx).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/audit"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/conversation"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/gifts"
	"github.com/teamthy/meterly/internal/households"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/notificationintents"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/pots"
	"github.com/teamthy/meterly/internal/powerdesk"
	"github.com/teamthy/meterly/internal/security"
	"github.com/teamthy/meterly/internal/support"
	"github.com/teamthy/meterly/internal/tokenshares"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type reposCtxKey struct{}

// WithRepos binds transaction-scoped repositories to ctx so in-transaction
// outbox consumers can write atomically in the surrounding InTx.
func WithRepos(ctx context.Context, r Repos) context.Context {
	return context.WithValue(ctx, reposCtxKey{}, r)
}

// ReposFromContext retrieves transaction-scoped repositories attached via WithRepos.
func ReposFromContext(ctx context.Context) (Repos, bool) {
	r, ok := ctx.Value(reposCtxKey{}).(Repos)
	return r, ok && r != nil
}

var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: concurrent update")
	ErrDuplicate = errors.New("store: duplicate")
)

type MetricsSnapshot struct {
	TransactionsByStatus map[tx.Overall]int64
	ManualReviewOpen     int64
	OutboxLagSeconds     float64
}

type Store interface {
	// InTx runs fn in a single database transaction. If fn returns an
	// error, every write made through the passed Repos is rolled back.
	InTx(ctx context.Context, fn func(Repos) error) error
	// Repos returns auto-commit repositories for reads and single writes.
	Repos() Repos
	// MetricsSnapshot returns aggregate operational gauges without exposing
	// any user, meter or transaction identifiers.
	MetricsSnapshot(ctx context.Context, now time.Time) (MetricsSnapshot, error)
	Close()
}

type Repos interface {
	Users() UserRepo
	Meters() MeterRepo
	Quotes() QuoteRepo
	Transactions() TransactionRepo
	Ledger() LedgerRepo
	Outbox() OutboxRepo
	Idempotency() IdempotencyRepo
	Audit() AuditRepo
	Providers() ProviderRepo
	TokenShares() TokenShareRepo
	Gifts() GiftRepo
	NotificationCandidates() NotificationCandidateRepo
	Notifications() NotificationRepo
	PayRequests() PayRequestRepo
	Pots() PotRepo
	AutoPower() AutoPowerRepo
	PowerDesk() PowerDeskRepo
	Offline() OfflineRepo
	Support() SupportRepo
	Households() HouseholdRepo
	RemoteCohorts() RemoteCohortRepo
	Auth() AuthRepo
	Readings() MeterReadingRepo
	LowUnitAlerts() LowUnitAlertRepo
}

// MeterReadingRepo stores observed remote kWh samples used to compute
// consumption rate and time-left on the server.
type MeterReadingRepo interface {
	InsertSample(ctx context.Context, s meters.ReadingSample) error
	ListSamples(ctx context.Context, meterID uuid.UUID, since time.Time, limit int) ([]meters.ReadingSample, error)
	ListDistinctMeterIDs(ctx context.Context, since time.Time) ([]uuid.UUID, error)
}

// LowUnitAlertRepo records the last low-unit notification per meter+user so
// the poller is idempotent across workers (atomic claim + cooldown).
type LowUnitAlertRepo interface {
	Claim(ctx context.Context, meterID uuid.UUID, userID string, remainingMilli int64, now time.Time, cooldown time.Duration) (claimed bool, err error)
}

// RemoteCohortRepo is the operator-attested AMI meter matrix. Longest matching
// number_prefix wins. Application code must not hard-code prefixes.
type RemoteCohortRepo interface {
	Create(ctx context.Context, c meters.RemoteCohort) error
	Get(ctx context.Context, id uuid.UUID) (meters.RemoteCohort, error)
	List(ctx context.Context) ([]meters.RemoteCohort, error)
	// Match returns the longest-prefix active authorised cohort for disco+number.
	Match(ctx context.Context, disco, meterNumber string) (meters.RemoteCohort, error)
	Revoke(ctx context.Context, id uuid.UUID, at time.Time) error
}

type AuditRepo interface {
	Add(ctx context.Context, e audit.Event) error
	ForResource(ctx context.Context, resourceType string, id uuid.UUID) ([]audit.Event, error)
	Recent(ctx context.Context, limit int) ([]audit.Event, error)
}

// ProviderRepo holds operator switches for integrations. A provider with
// no record is enabled.
type ProviderRepo interface {
	Enabled(ctx context.Context, name string) (bool, error)
	SetEnabled(ctx context.Context, name, providerType string, enabled bool) error
}

// TxnFilter selects transactions for operators. Zero values match all.
type TxnFilter struct {
	ReviewOnly bool
	Payment    tx.PaymentState
	Vending    tx.VendingState
	Delivery   tx.DeliveryState
	CustomerID string
	MeterID    uuid.UUID
	Before     time.Time // created strictly before (keyset pagination)
	Limit      int
}

type UserRepo interface {
	// EnsureByExternalID maps an identity-provider subject to an internal
	// user ID (UUID string), creating the user on first sight.
	EnsureByExternalID(ctx context.Context, externalID string) (string, error)
	// LockForUpdate acquires a transaction-scoped row lock on the user so
	// per-user spend caps serialize across concurrent AutoPower rules.
	LockForUpdate(ctx context.Context, userID string) error
	// UpsertContacts updates encrypted verified contact fields on an existing
	// user. Non-empty fields replace existing values; empty fields leave the
	// stored value untouched.
	UpsertContacts(ctx context.Context, c notifications.UserContacts) error
	// Contacts returns the user's encrypted contact record, or ErrNotFound.
	Contacts(ctx context.Context, userID string) (notifications.UserContacts, error)
	Profile(ctx context.Context, userID string) (identity.Profile, error)
	SetDisplayName(ctx context.Context, userID, name string) error
	SetOnboarding(ctx context.Context, userID, step string, completedAt *time.Time) error
	ExternalID(ctx context.Context, userID string) (string, error)
}

// AuthRepo persists WebAuthn credentials, login challenges and sessions.
type AuthRepo interface {
	PutChallenge(ctx context.Context, c identity.Challenge) error
	TakeChallenge(ctx context.Context, challenge []byte, now time.Time) (identity.Challenge, error)
	InsertCredential(ctx context.Context, c identity.Credential) error
	GetCredentialByCredID(ctx context.Context, credID []byte) (identity.Credential, error)
	ListCredentials(ctx context.Context, userID string) ([]identity.Credential, error)
	UpdateCredentialUse(ctx context.Context, credID []byte, signCount uint32, at time.Time) error
	DeleteCredential(ctx context.Context, userID string, id uuid.UUID) error
	InsertSession(ctx context.Context, s identity.Session) error
	GetSessionByHMAC(ctx context.Context, hmac string) (identity.Session, error)
	GetSession(ctx context.Context, id uuid.UUID) (identity.Session, error)
	ListSessions(ctx context.Context, userID string) ([]identity.Session, error)
	RevokeSession(ctx context.Context, id uuid.UUID, at time.Time) error
	RevokeOtherSessions(ctx context.Context, userID string, keep uuid.UUID, at time.Time) error
	MarkSessionStepUp(ctx context.Context, id uuid.UUID, at time.Time) error
	TouchSession(ctx context.Context, id uuid.UUID, ipHMAC, uaHMAC string, at time.Time) error
	InsertSecurityEvent(ctx context.Context, e security.Event) error
	ListSecurityEvents(ctx context.Context, userID string, limit int) ([]security.Event, error)
}

type MeterRepo interface {
	Get(ctx context.Context, id uuid.UUID) (*meters.Meter, error)
	// ListAll is for the MODE=dev console. Production admin investigates via transactions.
	ListAll(ctx context.Context, limit int) ([]*meters.Meter, error)
	// GetForUpdate serializes registration and self-removal for this meter.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*meters.Meter, error)
	GetByHash(ctx context.Context, hash string) (*meters.Meter, error)
	// Insert returns ErrDuplicate if a meter with the same hash exists.
	Insert(ctx context.Context, m *meters.Meter) error
	Update(ctx context.Context, m *meters.Meter) error
	// UpsertGrant creates an active grant, or raises the permission and
	// updates the label of an existing active grant. Never lowers it.
	// Grant-changing workflows must hold the meter lock in the same InTx
	// before calling this method, to serialize with purchase authorization.
	UpsertGrant(ctx context.Context, g meters.Grant) error
	ActiveGrant(ctx context.Context, meterID uuid.UUID, userID string) (*meters.Grant, error)
	ListForUser(ctx context.Context, userID string) ([]meters.Listed, error)
	// ListActiveUserIDs returns user IDs with an un-revoked grant on the meter.
	ListActiveUserIDs(ctx context.Context, meterID uuid.UUID) ([]string, error)
	// Revoke callers must hold the meter lock in the surrounding InTx.
	Revoke(ctx context.Context, meterID uuid.UUID, userID string, at time.Time) error
}

type QuoteRepo interface {
	Create(ctx context.Context, q *tx.Quote) error
	Get(ctx context.Context, id uuid.UUID) (*tx.Quote, error)
	// GetForUpdate locks the quote until the surrounding transaction ends.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Quote, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
}

type TransactionRepo interface {
	Create(ctx context.Context, t *tx.Transaction) error
	Get(ctx context.Context, id uuid.UUID) (*tx.Transaction, error)
	// GetForUpdate locks the row until the surrounding transaction ends.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*tx.Transaction, error)
	ByPaymentRef(ctx context.Context, ref string) (*tx.Transaction, error)
	// Save persists t if nobody saved since it was loaded (optimistic
	// concurrency), appends new history, and increments t.Version.
	Save(ctx context.Context, t *tx.Transaction) error
	ListForCustomer(ctx context.Context, customerID string) ([]*tx.Transaction, error)
	// Search returns matches newest first.
	Search(ctx context.Context, f TxnFilter) ([]*tx.Transaction, error)
	Count(ctx context.Context, f TxnFilter) (int, error)
	// ClaimDue returns unsettled transactions whose NextCheckAt <= now and
	// that have not changed since staleBefore, and pushes their NextCheckAt
	// to now+lease so concurrent workers skip them.
	ClaimDue(ctx context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)
}

type LedgerRepo interface {
	// Post validates and records a balanced posting. Posting an existing
	// reference again is a no-op that returns the original.
	Post(ctx context.Context, t ledger.Transaction) (ledger.Transaction, error)
	BySource(ctx context.Context, sourceID uuid.UUID) ([]ledger.Transaction, error)
	Balance(ctx context.Context, acct ledger.AccountCode, c money.Currency) (int64, error)
	TrialBalance(ctx context.Context) (int64, error)
}

type OutboxRepo interface {
	Add(ctx context.Context, e events.Event) error
	// ClaimUnpublished locks up to limit unpublished events (skipping ones
	// locked by other workers) for the surrounding transaction.
	ClaimUnpublished(ctx context.Context, limit int) ([]events.Event, error)
	MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error
	ForAggregate(ctx context.Context, id uuid.UUID) ([]events.Event, error)
}

type IdempotencyRepo interface {
	// Reserve inserts rec. If an unexpired record exists for the same
	// scope/operation/key, it returns that record instead (and inserts
	// nothing). Expired records are replaced.
	Reserve(ctx context.Context, rec idempotency.Record) (existing *idempotency.Record, err error)
	Complete(ctx context.Context, scope, op, key, resultRef string) error
}

// TokenShareRepo persists metadata only, never token values or plaintext destinations.
type TokenShareRepo interface {
	AddCopy(context.Context, tokenshares.Share) error
	GetCopy(context.Context, uuid.UUID, uuid.UUID, string) (tokenshares.Share, error)
	ListCopies(context.Context, uuid.UUID, string) ([]tokenshares.Share, error)
	AddShare(context.Context, tokenshares.Share) error
	GetShare(context.Context, uuid.UUID) (tokenshares.Share, error)
	ListShares(context.Context, uuid.UUID, string) ([]tokenshares.Share, error)
	CountSends(ctx context.Context, userID string, start, end time.Time) (int, error)
}

// GiftRepo stores encrypted contact metadata; no raw recipient contact.
type GiftRepo interface {
	Create(context.Context, gifts.Gift) error
	Get(context.Context, uuid.UUID) (gifts.Gift, error)
}

// NotificationCandidateRepo is a held, non-deliverable record of relevant
// outbox events. Claim and Record must run within the same Store.InTx.
type NotificationCandidateRepo interface {
	Claim(context.Context, time.Time, int) ([]notificationintents.Intent, error)
	Record(context.Context, notificationintents.Intent) error
	Get(context.Context, uuid.UUID) (notificationintents.Intent, error)
}

// NotificationRepo persists notification preferences, global opt-outs,
// WhatsApp service windows and delivery job metadata (never message bodies or
// plaintext destinations).
type NotificationRepo interface {
	GetPreferences(ctx context.Context, userID string) (notifications.Preferences, error)
	SetPreferences(ctx context.Context, userID string, p notifications.Preferences, now time.Time) error
	AddOptOut(ctx context.Context, o notifications.OptOut) (inserted bool, err error)
	IsOptedOut(ctx context.Context, destinationHMAC string, ch notifications.Channel) (bool, error)
	RecordWhatsAppInbound(ctx context.Context, destinationHMAC string, at time.Time) error
	HasActiveWhatsAppWindow(ctx context.Context, destinationHMAC string, now time.Time) (bool, error)
	CreateDelivery(ctx context.Context, d *notifications.Delivery) (inserted bool, err error)
	GetDelivery(ctx context.Context, id uuid.UUID) (*notifications.Delivery, error)
	GetDeliveryForUpdate(ctx context.Context, id uuid.UUID) (*notifications.Delivery, error)
	UpdateDelivery(ctx context.Context, d *notifications.Delivery) error
	ListDueDeliveries(ctx context.Context, now, staleSendingBefore time.Time, limit int) ([]uuid.UUID, error)
	ForTransaction(ctx context.Context, txnID uuid.UUID) ([]notifications.Delivery, error)
	PurgeOldDestinationHMACs(ctx context.Context, cutoff time.Time) (int, error)
}

// PayRequestRepo persists Pay-for-Me payment requests and guest transaction
// payers. Codes and plaintext contacts are never stored.
type PayRequestRepo interface {
	Create(ctx context.Context, p *payrequests.PaymentRequest) error
	Get(ctx context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error)
	GetForUpdate(ctx context.Context, id uuid.UUID) (*payrequests.PaymentRequest, error)
	GetByCodeHMAC(ctx context.Context, codeHMAC string) (*payrequests.PaymentRequest, error)
	GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*payrequests.PaymentRequest, error)
	Update(ctx context.Context, p *payrequests.PaymentRequest) error
	ListForRequester(ctx context.Context, requesterID string) ([]*payrequests.PaymentRequest, error)
	CreatePayer(ctx context.Context, payer payrequests.Payer) error
	GetPayer(ctx context.Context, txnID uuid.UUID) (payrequests.Payer, error)
	GetPayerForUpdate(ctx context.Context, txnID uuid.UUID) (payrequests.Payer, error)
	GetPayerByStatusHMAC(ctx context.Context, statusHMAC string) (payrequests.Payer, error)
	MarkPayerSlotReleased(ctx context.Context, txnID uuid.UUID) error
}

// PotRepo persists Funding Pots, contributions and POT-funded allocations.
type PotRepo interface {
	Create(ctx context.Context, p *pots.Pot) error
	Get(ctx context.Context, id uuid.UUID) (*pots.Pot, error)
	GetForUpdate(ctx context.Context, id uuid.UUID) (*pots.Pot, error)
	GetByCodeHMAC(ctx context.Context, codeHMAC string) (*pots.Pot, error)
	GetByCodeHMACForUpdate(ctx context.Context, codeHMAC string) (*pots.Pot, error)
	Update(ctx context.Context, p *pots.Pot) error
	ClaimDue(ctx context.Context, now, staleBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)

	CreateContribution(ctx context.Context, c *pots.Contribution) error
	GetContribution(ctx context.Context, id uuid.UUID) (*pots.Contribution, error)
	GetContributionForUpdate(ctx context.Context, id uuid.UUID) (*pots.Contribution, error)
	ContributionByPaymentRef(ctx context.Context, ref string) (*pots.Contribution, error)
	UpdateContribution(ctx context.Context, c *pots.Contribution) error
	ListContributions(ctx context.Context, potID uuid.UUID) ([]*pots.Contribution, error)
	ListContributionsForUpdate(ctx context.Context, potID uuid.UUID) ([]*pots.Contribution, error)

	CreateAllocation(ctx context.Context, a *pots.Allocation) error
	GetAllocationByTxn(ctx context.Context, txnID uuid.UUID) (*pots.Allocation, error)
	GetAllocationByTxnForUpdate(ctx context.Context, txnID uuid.UUID) (*pots.Allocation, error)
	UpdateAllocation(ctx context.Context, a *pots.Allocation) error
	ListAllocations(ctx context.Context, potID uuid.UUID) ([]*pots.Allocation, error)
}

// AutoPowerRepo persists saved payment methods, AutoPower rules and slot runs.
type AutoPowerRepo interface {
	CreatePaymentMethod(ctx context.Context, m *autopower.SavedPaymentMethod) error
	GetPaymentMethod(ctx context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error)
	GetPaymentMethodForUpdate(ctx context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error)
	GetActivePaymentMethodBySourceTxn(ctx context.Context, userID string, txnID uuid.UUID) (*autopower.SavedPaymentMethod, error)
	ListPaymentMethods(ctx context.Context, userID string) ([]*autopower.SavedPaymentMethod, error)
	DeletePaymentMethod(ctx context.Context, id uuid.UUID, at time.Time) error

	CreateRule(ctx context.Context, r *autopower.Rule) error
	GetRule(ctx context.Context, id uuid.UUID) (*autopower.Rule, error)
	GetRuleForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Rule, error)
	UpdateRule(ctx context.Context, r *autopower.Rule) error
	ListRulesForUser(ctx context.Context, userID string) ([]*autopower.Rule, error)
	ListRulesByPaymentMethodForUpdate(ctx context.Context, paymentMethodID uuid.UUID) ([]*autopower.Rule, error)
	ClaimDueRulesForMaterialize(ctx context.Context, horizon, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)
	ClaimDueLowBalanceRules(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)

	CreateRun(ctx context.Context, run *autopower.Run) error
	GetRun(ctx context.Context, id uuid.UUID) (*autopower.Run, error)
	GetRunForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Run, error)
	GetRunBySlot(ctx context.Context, ruleID uuid.UUID, scheduledFor time.Time) (*autopower.Run, error)
	GetRunByTxn(ctx context.Context, txnID uuid.UUID) (*autopower.Run, error)
	UpdateRun(ctx context.Context, run *autopower.Run) error
	ListRunsForRule(ctx context.Context, ruleID uuid.UUID, limit int) ([]*autopower.Run, error)
	CancelScheduledRunsForRule(ctx context.Context, ruleID uuid.UUID, now time.Time) error
	ClaimDueRuns(ctx context.Context, headsUpHorizon, now, staleChargingBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error)
	SumRuleSpendMinor(ctx context.Context, ruleID uuid.UUID, startUTC, endUTC time.Time) (int64, error)
	SumUserSpendMinor(ctx context.Context, userID string, startUTC, endUTC time.Time) (int64, error)
}

// PowerDeskRepo persists M10 Landlord Power Desk organisations, memberships,
// properties, buildings, units, unit-meter attachments and LANDLORD_BULK metadata.
type PowerDeskRepo interface {
	CreateOrg(ctx context.Context, org *powerdesk.Organisation) error
	GetOrg(ctx context.Context, id uuid.UUID) (*powerdesk.Organisation, error)
	GetOrgForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.Organisation, error)
	UpdateOrg(ctx context.Context, org *powerdesk.Organisation) error

	UpsertMember(ctx context.Context, m *powerdesk.Member) error
	GetActiveMemberRole(ctx context.Context, orgID uuid.UUID, userID string) (powerdesk.Role, error)
	GetMemberForUpdate(ctx context.Context, orgID uuid.UUID, userID string) (*powerdesk.Member, error)
	ListActiveMembers(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Member, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*powerdesk.Organisation, error)
	CountActiveOwners(ctx context.Context, orgID uuid.UUID) (int, error)
	RemoveMember(ctx context.Context, orgID uuid.UUID, userID string) error

	CreateProperty(ctx context.Context, p *powerdesk.Property) error
	GetProperty(ctx context.Context, id uuid.UUID) (*powerdesk.Property, error)
	ListPropertiesForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Property, error)

	EnsureDefaultBuilding(ctx context.Context, propertyID uuid.UUID, name string, now time.Time) (*powerdesk.Building, error)
	GetBuilding(ctx context.Context, id uuid.UUID) (*powerdesk.Building, error)
	ListBuildingsForProperty(ctx context.Context, propertyID uuid.UUID) ([]*powerdesk.Building, error)

	CreateUnit(ctx context.Context, u *powerdesk.Unit) error
	GetUnit(ctx context.Context, id uuid.UUID) (*powerdesk.Unit, error)
	ListUnitsForProperty(ctx context.Context, propertyID uuid.UUID) ([]*powerdesk.Unit, error)
	ListUnitsForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.Unit, error)

	AttachUnitMeter(ctx context.Context, um *powerdesk.UnitMeter) error
	GetUnitMeter(ctx context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error)
	GetUnitMeterForUpdate(ctx context.Context, id uuid.UUID) (*powerdesk.UnitMeter, error)
	GetActiveUnitMeterByUnit(ctx context.Context, unitID uuid.UUID) (*powerdesk.UnitMeter, error)
	ListUnitMetersForOrg(ctx context.Context, orgID uuid.UUID) ([]*powerdesk.UnitMeter, error)
	UpdateUnitMeter(ctx context.Context, um *powerdesk.UnitMeter) error

	CreateBulkTopup(ctx context.Context, bt *powerdesk.BulkTopup, items []*powerdesk.BulkTopupItem) error
	GetBulkTopup(ctx context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error)
	GetBulkTopupForUpdate(ctx context.Context, potID uuid.UUID) (*powerdesk.BulkTopup, error)
	GetBulkTopupByIdempotency(ctx context.Context, orgID uuid.UUID, key string) (*powerdesk.BulkTopup, error)
	UpdateBulkTopup(ctx context.Context, bt *powerdesk.BulkTopup) error
	ListBulkTopupItems(ctx context.Context, potID uuid.UUID) ([]*powerdesk.BulkTopupItem, error)
	UpdateBulkTopupItem(ctx context.Context, item *powerdesk.BulkTopupItem) error
	ListBulkTopupItemsForOrg(ctx context.Context, orgID uuid.UUID, propertyID *uuid.UUID) ([]*powerdesk.BulkTopupItem, error)
}

// OfflineRepo persists M11 USSD and WhatsApp conversation sessions, phone-link
// verification state, SMS OTP challenges, webhook message dedupe, channel
// purchase records and audited daily token-resend counters.
type OfflineRepo interface {
	FindUserIDByPhoneHMAC(ctx context.Context, phoneHMAC string) (string, error)

	GetPhoneLink(ctx context.Context, phoneHMAC string) (*conversation.PhoneLink, error)
	GetPhoneLinkForUpdate(ctx context.Context, phoneHMAC string) (*conversation.PhoneLink, error)
	UpsertPhoneLink(ctx context.Context, link *conversation.PhoneLink) error

	CreateOTP(ctx context.Context, otp *conversation.ChannelOTP) error
	GetLatestOTPForUpdate(ctx context.Context, phoneHMAC string) (*conversation.ChannelOTP, error)
	UpdateOTP(ctx context.Context, otp *conversation.ChannelOTP) error

	CreateSession(ctx context.Context, s *conversation.Session) error
	GetSessionByProviderIDForUpdate(ctx context.Context, ch conversation.Channel, providerSessionID string) (*conversation.Session, error)
	GetActiveSessionForPhoneForUpdate(ctx context.Context, ch conversation.Channel, phoneHMAC string, now time.Time) (*conversation.Session, error)
	UpdateSession(ctx context.Context, s *conversation.Session) error

	// RecordProcessedMessage returns inserted=false when (ch, messageID) was already seen.
	RecordProcessedMessage(ctx context.Context, ch conversation.Channel, messageID, phoneHMAC string, at time.Time) (inserted bool, err error)

	RecordPurchase(ctx context.Context, p *conversation.OfflinePurchase) error
	GetPurchase(ctx context.Context, txnID uuid.UUID) (*conversation.OfflinePurchase, error)
	SumPhoneSpendMinor(ctx context.Context, phoneHMAC string, startUTC, endUTC time.Time) (int64, error)

	RecordTokenResend(ctx context.Context, r *conversation.TokenResend) error
	CountTokenResends(ctx context.Context, userID string, startUTC, endUTC time.Time) (int, error)
}

// SupportRepo persists E2E support threads. HUMAN ciphertext is opaque;
// AGENT/SYSTEM bodies are stored already sealed by the HTTP layer.
type SupportRepo interface {
	UpsertIdentityKey(ctx context.Context, k support.IdentityKey) error
	GetIdentityKey(ctx context.Context, userID string) (support.IdentityKey, error)
	ListStaffKeys(ctx context.Context) ([]support.IdentityKey, error)

	CreateThread(ctx context.Context, t *support.Thread) error
	GetThread(ctx context.Context, id uuid.UUID) (*support.Thread, error)
	GetThreadForUpdate(ctx context.Context, id uuid.UUID) (*support.Thread, error)
	UpdateThread(ctx context.Context, t *support.Thread) error
	ListThreadsForCustomer(ctx context.Context, customerID string) ([]*support.Thread, error)
	ListThreadsForStaff(ctx context.Context, status support.ThreadStatus, limit int) ([]*support.Thread, error)

	AddMessage(ctx context.Context, m *support.Message) error
	ListMessages(ctx context.Context, threadID uuid.UUID, after time.Time, limit int) ([]*support.Message, error)

	UpsertWrappedKey(ctx context.Context, w support.WrappedKey) error
	ListWrappedKeys(ctx context.Context, threadID uuid.UUID) ([]support.WrappedKey, error)
	GetWrappedKey(ctx context.Context, threadID uuid.UUID, holderUserID string) (support.WrappedKey, error)
}

// HouseholdRepo persists verified ownership, households and membership.
// Grant rows are not used as proof of ownership.
type HouseholdRepo interface {
	Create(ctx context.Context, h households.Household) error
	Get(ctx context.Context, id uuid.UUID) (households.Household, error)
	HouseholdIDsForUser(ctx context.Context, userID string) ([]uuid.UUID, error)

	UpsertMember(ctx context.Context, m households.Member) error
	Member(ctx context.Context, householdID uuid.UUID, userID string) (households.Member, error)
	ListMembers(ctx context.Context, householdID uuid.UUID) ([]households.Member, error)

	AddMeter(ctx context.Context, link households.MeterLink) error
	ListMeters(ctx context.Context, householdID uuid.UUID) ([]households.MeterLink, error)

	UpsertOwnership(ctx context.Context, o households.Ownership) error
	ActiveOwnership(ctx context.Context, meterID uuid.UUID, userID string) (households.Ownership, error)
	ListOwnership(ctx context.Context, meterID uuid.UUID) ([]households.Ownership, error)

	// PermissionForMeter returns the highest household-implied permission
	// for an ACTIVE member on this meter, or ErrNotFound.
	PermissionForMeter(ctx context.Context, userID string, meterID uuid.UUID) (meters.Permission, error)
}
