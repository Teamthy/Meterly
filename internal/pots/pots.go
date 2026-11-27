// Package pots models the reusable Funding Pot engine (M8 Split the Meter and
// M10 Landlord Bulk Top-Up).
//
// Security & privacy invariants:
//   - Share codes carry 128 bits of cryptographic randomness encoded in
//     unpadded RFC 4648 base32; only their domain-separated HMAC-SHA256
//     (`pot-code:<code>`) is stored and compared in constant time.
//   - Contributor names, emails and optional SMS phones are AES-256-GCM
//     encrypted bound to their owning contribution ID. Public progress views
//     expose pre-redacted initials only (e.g. "T.O." or "Anonymous").
package pots

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/payrequests"
	"github.com/teamthy/meterly/internal/secure"
)

type Kind string

const (
	KindSplitMeter   Kind = "SPLIT_METER"
	KindLandlordBulk Kind = "LANDLORD_BULK"
)

type Status string

const (
	StatusPendingApproval Status = "PENDING_APPROVAL"
	StatusOpen            Status = "OPEN"
	StatusFunded          Status = "FUNDED"
	StatusAllocating      Status = "ALLOCATING"
	StatusSettled         Status = "SETTLED"
	StatusExpired         Status = "EXPIRED"
	StatusCancelled       Status = "CANCELLED"
	StatusRefunding       Status = "REFUNDING"
	StatusRefunded        Status = "REFUNDED"
)

type ContributionStatus string

const (
	ContribPending       ContributionStatus = "PENDING"
	ContribPaid          ContributionStatus = "PAID"
	ContribRefundPending ContributionStatus = "REFUND_PENDING"
	ContribRefunded      ContributionStatus = "REFUNDED"
	ContribFailed        ContributionStatus = "FAILED"
)

type AllocationStatus string

const (
	AllocActive   AllocationStatus = "ACTIVE"
	AllocReturned AllocationStatus = "RETURNED"
)

const (
	DefaultMaxHold     = 72 * time.Hour
	HardMaxHold        = 7 * 24 * time.Hour
	MaxSuggestedShares = 50
)

var (
	ErrInvalidTransition = errors.New("pots: invalid state transition")
	ErrInvalidKind       = errors.New("pots: unsupported pot kind")
	ErrInvalidDeadline   = errors.New("pots: deadline must be in the future and within POT_MAX_HOLD")
	ErrInvalidShares     = errors.New("pots: suggested_shares must be 0 or between 2 and 50")
)

var potTransitions = map[Status][]Status{
	StatusPendingApproval: {StatusOpen, StatusCancelled, StatusExpired},
	StatusOpen:            {StatusFunded, StatusExpired, StatusCancelled},
	StatusFunded:          {StatusAllocating, StatusRefunding},
	StatusAllocating:      {StatusSettled, StatusRefunding},
	StatusExpired:         {StatusRefunding},
	StatusCancelled:       {StatusRefunding},
	StatusRefunding:       {StatusRefunded},
	// Late payment arriving after a pot already finished refunding re-enters REFUNDING to reverse it.
	StatusRefunded: {StatusRefunding},
}

var contribTransitions = map[ContributionStatus][]ContributionStatus{
	ContribPending:       {ContribPaid, ContribRefundPending, ContribFailed},
	ContribPaid:          {ContribRefundPending},
	ContribRefundPending: {ContribPaid, ContribRefunded},
	// Late payment arriving at PSP after a pending checkout was marked FAILED at expiry.
	ContribFailed: {ContribRefundPending},
}

// Pot is a pooled funding container for one or more electricity purchases.
type Pot struct {
	ID              uuid.UUID      `json:"id"`
	Kind            Kind           `json:"kind"`
	OwnerID         string         `json:"-"`
	MeterID         uuid.UUID      `json:"meter_id,omitempty"`
	TargetMinor     int64          `json:"target_minor"`
	FeeMinor        int64          `json:"fee_minor"`
	Currency        money.Currency `json:"currency"`
	SuggestedShares int            `json:"suggested_shares,omitempty"`
	NoteCiphertext  []byte         `json:"-"`
	NoteKeyVersion  string         `json:"-"`
	Status          Status         `json:"status"`
	CodeHMAC        string         `json:"-"`
	Deadline        time.Time      `json:"deadline"`
	ReviewReason    string         `json:"review_reason,omitempty"`
	CheckAttempts   int            `json:"-"`
	NextCheckAt     time.Time      `json:"-"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

func (p *Pot) Settled() bool {
	if p.ReviewReason != "" {
		return true
	}
	return p.Status == StatusSettled || p.Status == StatusRefunded
}

func (p *Pot) ElectricityAmountMinor() int64 {
	if p.TargetMinor <= p.FeeMinor {
		return 0
	}
	return p.TargetMinor - p.FeeMinor
}

func (p *Pot) SuggestedShareMinor() int64 {
	if p.SuggestedShares < 2 {
		return 0
	}
	return (p.TargetMinor + int64(p.SuggestedShares) - 1) / int64(p.SuggestedShares)
}

func (p *Pot) FlagForReview(reason string, now time.Time) {
	p.ReviewReason = reason
	p.UpdatedAt = now.UTC()
}

func (s Status) Terminal() bool {
	return s == StatusSettled || s == StatusRefunded
}

func ValidateShares(n int) error {
	if n == 0 || (n >= 2 && n <= MaxSuggestedShares) {
		return nil
	}
	return ErrInvalidShares
}

type Summary struct {
	FundedMinor        int64
	RefundedMinor      int64
	PaidCount          int
	RefundPendingCount int
}

func SummarizeContributions(cs []*Contribution) Summary {
	var s Summary
	for _, c := range cs {
		s.FundedMinor += c.RetainedMinor
		s.RefundedMinor += c.RefundedMinor
		if c.RetainedMinor > 0 {
			s.PaidCount++
		}
		if c.Status == ContribRefundPending && c.PendingRefundMinor() > 0 {
			s.RefundPendingCount++
		}
	}
	return s
}

func (p *Pot) Transition(to Status, now time.Time) error {
	for _, s := range potTransitions[p.Status] {
		if s == to {
			p.Status = to
			p.UpdatedAt = now.UTC()
			return nil
		}
	}
	return fmt.Errorf("%w: pot %s -> %s", ErrInvalidTransition, p.Status, to)
}

// Contribution is one user or guest payment into a Pot.
type Contribution struct {
	ID                  uuid.UUID          `json:"id"`
	PotID               uuid.UUID          `json:"pot_id"`
	PayerUserID         string             `json:"-"`
	EmailCiphertext     []byte             `json:"-"`
	EmailKeyVersion     string             `json:"-"`
	NameCiphertext      []byte             `json:"-"`
	NameKeyVersion      string             `json:"-"`
	Initials            string             `json:"initials"`
	PhoneCiphertext     []byte             `json:"-"`
	PhoneKeyVersion     string             `json:"-"`
	OptInSMS            bool               `json:"-"`
	AmountMinor         int64              `json:"amount_minor"`
	RetainedMinor       int64              `json:"retained_minor"`
	RefundAmountMinor   int64              `json:"refund_amount_minor"`
	RefundedMinor       int64              `json:"refunded_minor"`
	RefundStep          int                `json:"-"`
	Currency            money.Currency     `json:"currency"`
	Status              ContributionStatus `json:"status"`
	PaymentProviderName string             `json:"-"`
	PaymentProviderRef  string             `json:"-"`
	CheckoutURL         string             `json:"checkout_url,omitempty"`
	RefundProviderRef   string             `json:"-"`
	RefundClaimUntil    time.Time          `json:"-"`
	PaidAt              *time.Time         `json:"paid_at,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

func (c *Contribution) PendingRefundMinor() int64 {
	if c.RefundAmountMinor <= c.RefundedMinor {
		return 0
	}
	return c.RefundAmountMinor - c.RefundedMinor
}

// RefundReference returns the deterministic idempotency key for the active
// contribution refund step (`refund:contribution:<id>` for single-step refunds).
func (c *Contribution) RefundReference() string {
	base := "refund:contribution:" + c.ID.String()
	if c.RefundStep <= 1 {
		return base
	}
	return base + ":" + strconv.Itoa(c.RefundStep)
}

func (c *Contribution) Transition(to ContributionStatus, now time.Time) error {
	for _, s := range contribTransitions[c.Status] {
		if s == to {
			c.Status = to
			c.UpdatedAt = now.UTC()
			return nil
		}
	}
	return fmt.Errorf("%w: contribution %s -> %s", ErrInvalidTransition, c.Status, to)
}

// Allocation binds funds held in a Pot to a POT-funded electricity transaction.
type Allocation struct {
	ID            uuid.UUID        `json:"id"`
	PotID         uuid.UUID        `json:"pot_id"`
	TransactionID uuid.UUID        `json:"transaction_id"`
	MeterID       uuid.UUID        `json:"meter_id"`
	AmountMinor   int64            `json:"amount_minor"`
	Currency      money.Currency   `json:"currency"`
	Status        AllocationStatus `json:"status"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func (a *Allocation) MarkReturned(now time.Time) {
	a.Status = AllocReturned
	a.UpdatedAt = now.UTC()
}

func GenerateCode() (string, error) {
	return payrequests.GenerateCode()
}

func CodeHMAC(box *secure.Box, rawCode string) (string, bool) {
	norm, ok := payrequests.NormalizeCode(rawCode)
	if !ok || box == nil {
		return "", false
	}
	return box.Hash("pot-code:" + norm), true
}

// BulkCodeHMAC computes a domain-separated code HMAC for non-public
// LANDLORD_BULK pots so public Split-the-Meter lookups never match them.
func BulkCodeHMAC(box *secure.Box, rawCode string) (string, bool) {
	norm, ok := payrequests.NormalizeCode(rawCode)
	if !ok || box == nil {
		return "", false
	}
	return box.Hash("pot-bulk:" + norm), true
}

func ConstantTimeEqualHMAC(a, b string) bool {
	return payrequests.ConstantTimeEqualHMAC(a, b)
}

// FormatInitials reduces a display name to privacy-preserving initials
// (up to 3 uppercase letters with trailing dots, or "Anonymous" if empty).
func FormatInitials(name string) string {
	words := strings.Fields(strings.TrimSpace(name))
	if len(words) == 0 {
		return "Anonymous"
	}
	var b strings.Builder
	count := 0
	for _, w := range words {
		for _, r := range w {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				b.WriteRune(unicode.ToUpper(r))
				b.WriteByte('.')
				count++
				break
			}
		}
		if count >= 3 {
			break
		}
	}
	if count == 0 || !utf8.ValidString(b.String()) {
		return "Anonymous"
	}
	return b.String()
}

func SealPotNote(box *secure.Box, potID uuid.UUID, note string) ([]byte, string, error) {
	if note == "" {
		return nil, "", nil
	}
	ct, err := box.EncryptBound(note, []byte("pot-note:"+potID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenPotNote(box *secure.Box, potID uuid.UUID, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	return box.DecryptBound(ct, []byte("pot-note:"+potID.String()))
}

func SealContributorEmail(box *secure.Box, contribID uuid.UUID, email string) ([]byte, string, error) {
	ct, err := box.EncryptBound(email, []byte("pot-contributor-email:"+contribID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenContributorEmail(box *secure.Box, contribID uuid.UUID, ct []byte) (string, error) {
	return box.DecryptBound(ct, []byte("pot-contributor-email:"+contribID.String()))
}

func SealContributorName(box *secure.Box, contribID uuid.UUID, name string) ([]byte, string, error) {
	if name == "" {
		return nil, "", nil
	}
	ct, err := box.EncryptBound(name, []byte("pot-contributor-name:"+contribID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenContributorName(box *secure.Box, contribID uuid.UUID, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	return box.DecryptBound(ct, []byte("pot-contributor-name:"+contribID.String()))
}

func SealContributorPhone(box *secure.Box, contribID uuid.UUID, phone string) ([]byte, string, error) {
	if phone == "" {
		return nil, "", nil
	}
	ct, err := box.EncryptBound(phone, []byte("pot-contributor-phone:"+contribID.String()))
	if err != nil {
		return nil, "", err
	}
	return ct, box.KeyVersion, nil
}

func OpenContributorPhone(box *secure.Box, contribID uuid.UUID, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	return box.DecryptBound(ct, []byte("pot-contributor-phone:"+contribID.String()))
}
