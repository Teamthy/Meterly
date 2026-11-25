// Package powerdesk models M10 Landlord Power Desk organisations, membership
// roles, properties, buildings, units, unit-meter attachments with operator
// verification, LANDLORD_BULK Funding Pot metadata, and ledger-derived monthly
// statements.
package powerdesk

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/money"
)

type Role string

const (
	RoleViewer  Role = "VIEWER"
	RoleManager Role = "MANAGER"
	RoleOwner   Role = "OWNER"
)

var roleRank = map[Role]int{
	RoleViewer:  1,
	RoleManager: 2,
	RoleOwner:   3,
}

func (r Role) Valid() bool {
	_, ok := roleRank[r]
	return ok
}

func (r Role) Allows(want Role) bool {
	held, okHeld := roleRank[r]
	needed, okNeeded := roleRank[want]
	return okHeld && okNeeded && held >= needed
}

type OrganisationType string

const (
	OrgEstate     OrganisationType = "ESTATE"
	OrgBusiness   OrganisationType = "BUSINESS"
	OrgEnterprise OrganisationType = "ENTERPRISE"
	OrgLandlord   OrganisationType = "LANDLORD"
)

func (t OrganisationType) Valid() bool {
	switch t {
	case OrgEstate, OrgBusiness, OrgEnterprise, OrgLandlord:
		return true
	default:
		return false
	}
}

type VerificationStatus string

const (
	VerificationUnverified VerificationStatus = "UNVERIFIED"
	VerificationPending    VerificationStatus = "PENDING"
	VerificationVerified   VerificationStatus = "VERIFIED"
	VerificationRejected   VerificationStatus = "REJECTED"
)

type MemberStatus string

const (
	MemberActive  MemberStatus = "ACTIVE"
	MemberRemoved MemberStatus = "REMOVED"
)

type UnitMeterStatus string

const (
	UnitMeterActive   UnitMeterStatus = "ACTIVE"
	UnitMeterDetached UnitMeterStatus = "DETACHED"
)

type VerificationMethod string

const (
	VerifyNone             VerificationMethod = ""
	VerifyAccountNameMatch VerificationMethod = "ACCOUNT_NAME_MATCH"
	VerifyEvidenceApproved VerificationMethod = "EVIDENCE_APPROVED"
)

func (m VerificationMethod) ValidForApproval() bool {
	return m == VerifyAccountNameMatch || m == VerifyEvidenceApproved
}

type ApprovalStatus string

const (
	ApprovalNotRequired ApprovalStatus = "NOT_REQUIRED"
	ApprovalPending     ApprovalStatus = "PENDING_APPROVAL"
	ApprovalApproved    ApprovalStatus = "APPROVED"
)

const (
	MaxBulkItems         = 100
	MaxNameRunes         = 120
	MaxEvidenceRefRunes  = 256
	DefaultBuildingName  = "Main Building"
	StatementDescription = "electricity purchased"
)

var (
	ErrInvalidRole               = errors.New("powerdesk: invalid role")
	ErrInvalidOrgName            = errors.New("powerdesk: organisation name is required (1–120 characters)")
	ErrInvalidOrgType            = errors.New("powerdesk: invalid organisation_type")
	ErrInvalidThreshold          = errors.New("powerdesk: manager_approval_threshold_minor must be >= 0")
	ErrInvalidPropertyName       = errors.New("powerdesk: property name is required (1–120 characters)")
	ErrInvalidUnitName           = errors.New("powerdesk: unit_name is required (1–120 characters)")
	ErrInvalidEvidenceRef        = errors.New("powerdesk: evidence_reference must be 1–256 characters and contain no URLs")
	ErrEvidenceRequired          = errors.New("powerdesk: evidence_reference is required for EVIDENCE_APPROVED")
	ErrVerificationMethodInvalid = errors.New("powerdesk: method must be ACCOUNT_NAME_MATCH or EVIDENCE_APPROVED")
	ErrAccountNameMismatch       = errors.New("powerdesk: meter DisCo account name does not match organisation name")
	ErrUnitMeterUnverified       = errors.New("powerdesk: unit meter ownership is not yet verified")
	ErrLastOwner                 = errors.New("powerdesk: cannot remove or demote the last active OWNER")
	ErrEmptyBulkItems            = errors.New("powerdesk: bulk top-up requires between 1 and 100 items")
	ErrDuplicateUnitInBulk       = errors.New("powerdesk: duplicate unit_id in bulk top-up")
	ErrNotPendingApproval        = errors.New("powerdesk: bulk top-up is not awaiting approval")
	ErrInvalidMonth              = errors.New("powerdesk: month must be YYYY-MM")
)

type Organisation struct {
	ID                            uuid.UUID          `json:"id"`
	Name                          string             `json:"name"`
	LegalName                     string             `json:"legal_name,omitempty"`
	RegistrationNumber            string             `json:"registration_number,omitempty"`
	OrganisationType              OrganisationType   `json:"organisation_type"`
	VerificationStatus            VerificationStatus `json:"verification_status"`
	ManagerApprovalThresholdMinor *int64             `json:"manager_approval_threshold_minor,omitempty"`
	CreatedAt                     time.Time          `json:"created_at"`
	UpdatedAt                     time.Time          `json:"updated_at"`
}

func (o *Organisation) NeedsManagerApproval(role Role, totalMinor int64) bool {
	if role != RoleManager || o.ManagerApprovalThresholdMinor == nil {
		return false
	}
	return totalMinor > *o.ManagerApprovalThresholdMinor
}

type Member struct {
	ID             uuid.UUID    `json:"id"`
	OrganisationID uuid.UUID    `json:"organisation_id"`
	UserID         string       `json:"user_id"`
	Role           Role         `json:"role"`
	Status         MemberStatus `json:"status"`
	CreatedAt      time.Time    `json:"created_at"`
}

type Property struct {
	ID             uuid.UUID `json:"id"`
	OrganisationID uuid.UUID `json:"organisation_id"`
	OwnerUserID    string    `json:"owner_user_id,omitempty"`
	Name           string    `json:"name"`
	Address        string    `json:"address,omitempty"`
	City           string    `json:"city,omitempty"`
	State          string    `json:"state,omitempty"`
	Country        string    `json:"country"`
	CreatedAt      time.Time `json:"created_at"`
}

type Building struct {
	ID         uuid.UUID `json:"id"`
	PropertyID uuid.UUID `json:"property_id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
}

type Unit struct {
	ID              uuid.UUID `json:"id"`
	OrganisationID  uuid.UUID `json:"organisation_id"`
	PropertyID      uuid.UUID `json:"property_id"`
	BuildingID      uuid.UUID `json:"building_id"`
	UnitName        string    `json:"unit_name"`
	OccupancyStatus string    `json:"occupancy_status"`
	CreatedAt       time.Time `json:"created_at"`
}

type UnitMeter struct {
	ID                 uuid.UUID          `json:"id"`
	OrganisationID     uuid.UUID          `json:"organisation_id"`
	PropertyID         uuid.UUID          `json:"property_id"`
	UnitID             uuid.UUID          `json:"unit_id"`
	MeterID            uuid.UUID          `json:"meter_id"`
	Status             UnitMeterStatus    `json:"status"`
	Verified           bool               `json:"verified"`
	VerificationMethod VerificationMethod `json:"verification_method,omitempty"`
	EvidenceReference  string             `json:"evidence_reference,omitempty"`
	DiscoAccountName   string             `json:"disco_account_name,omitempty"`
	VerifiedByUserID   string             `json:"verified_by_user_id,omitempty"`
	VerifiedAt         *time.Time         `json:"verified_at,omitempty"`
	VerificationReason string             `json:"verification_reason,omitempty"`
	CreatedByUserID    string             `json:"created_by_user_id"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

type BulkTopup struct {
	PotID                 uuid.UUID      `json:"pot_id"`
	OrganisationID        uuid.UUID      `json:"organisation_id"`
	CreatedByUserID       string         `json:"created_by_user_id"`
	CreatorRole           Role           `json:"creator_role"`
	ApprovalStatus        ApprovalStatus `json:"approval_status"`
	ApprovedByUserID      string         `json:"approved_by_user_id,omitempty"`
	ApprovedAt            *time.Time     `json:"approved_at,omitempty"`
	ContributionID        *uuid.UUID     `json:"contribution_id,omitempty"`
	ElectricityTotalMinor int64          `json:"electricity_total_minor"`
	FeeTotalMinor         int64          `json:"fee_total_minor"`
	TotalMinor            int64          `json:"total_minor"`
	Currency              money.Currency `json:"currency"`
	IdempotencyKey        string         `json:"-"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

type BulkTopupItem struct {
	ID             uuid.UUID      `json:"id"`
	PotID          uuid.UUID      `json:"pot_id"`
	OrganisationID uuid.UUID      `json:"organisation_id"`
	PropertyID     uuid.UUID      `json:"property_id"`
	UnitID         uuid.UUID      `json:"unit_id"`
	UnitMeterID    uuid.UUID      `json:"unit_meter_id"`
	MeterID        uuid.UUID      `json:"meter_id"`
	AmountMinor    int64          `json:"amount_minor"`
	FeeMinor       int64          `json:"fee_minor"`
	TotalMinor     int64          `json:"total_minor"`
	Currency       money.Currency `json:"currency"`
	TransactionID  *uuid.UUID     `json:"transaction_id,omitempty"`
	AllocationID   *uuid.UUID     `json:"allocation_id,omitempty"`
	FailureReason  string         `json:"failure_reason,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// FormatBulkSummary produces an honest human-readable partial-success summary
// ("10 delivered, 1 failed and refunded, 1 pending").
func FormatBulkSummary(delivered, failedRefunded, pending int) string {
	return fmt.Sprintf("%d delivered, %d failed and refunded, %d pending", delivered, failedRefunded, pending)
}

var urlLikeRe = regexp.MustCompile(`(?i)(https?://|www\.)`)

func CleanName(raw string, invalidErr error) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || utf8.RuneCountInString(s) > MaxNameRunes {
		return "", invalidErr
	}
	return s, nil
}

func ValidateEvidenceRef(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if utf8.RuneCountInString(s) > MaxEvidenceRefRunes || urlLikeRe.MatchString(s) {
		return "", ErrInvalidEvidenceRef
	}
	return s, nil
}

// AccountNamesMatch normalizes punctuation/case/whitespace so an operator's
// manual ACCOUNT_NAME_MATCH review confirms the DisCo validation customer name
// matches the organisation's legal_name or display name.
func AccountNamesMatch(discoAccountName string, org *Organisation) bool {
	if org == nil {
		return false
	}
	normDisco := normalizeEntityName(discoAccountName)
	if normDisco == "" {
		return false
	}
	if org.LegalName != "" && normalizeEntityName(org.LegalName) == normDisco {
		return true
	}
	return normalizeEntityName(org.Name) == normDisco
}

func normalizeEntityName(raw string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' || r == ',' || r == '&' || r == '/':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ParseStatementMonth parses "YYYY-MM" in Africa/Lagos (WAT, UTC+1) and returns
// the half-open UTC interval [startUTC, endUTC).
func ParseStatementMonth(month string) (startUTC, endUTC time.Time, canonical string, err error) {
	s := strings.TrimSpace(month)
	if len(s) != 7 || s[4] != '-' {
		return time.Time{}, time.Time{}, "", ErrInvalidMonth
	}
	loc, lErr := time.LoadLocation("Africa/Lagos")
	if lErr != nil {
		loc = time.FixedZone("WAT", 3600)
	}
	t, pErr := time.ParseInLocation("2006-01", s, loc)
	if pErr != nil || t.Format("2006-01") != s {
		return time.Time{}, time.Time{}, "", ErrInvalidMonth
	}
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)
	return start.UTC(), end.UTC(), s, nil
}

type UnitStatementRow struct {
	PropertyID                uuid.UUID      `json:"property_id"`
	PropertyName              string         `json:"property_name"`
	UnitID                    uuid.UUID      `json:"unit_id"`
	UnitName                  string         `json:"unit_name"`
	MaskedMeterNumber         string         `json:"masked_meter_number"`
	Disco                     string         `json:"disco"`
	Description               string         `json:"description"`
	ElectricityPurchasedMinor int64          `json:"electricity_purchased_minor"`
	FeesMinor                 int64          `json:"fees_minor"`
	RefundsMinor              int64          `json:"refunds_minor"`
	NetChargedMinor           int64          `json:"net_charged_minor"`
	Currency                  money.Currency `json:"currency"`
	VendedCount               int            `json:"vended_count"`
	RefundedCount             int            `json:"refunded_count"`
}

type PropertyStatement struct {
	PropertyID                uuid.UUID          `json:"property_id"`
	PropertyName              string             `json:"property_name"`
	Description               string             `json:"description"`
	ElectricityPurchasedMinor int64              `json:"electricity_purchased_minor"`
	FeesMinor                 int64              `json:"fees_minor"`
	RefundsMinor              int64              `json:"refunds_minor"`
	NetChargedMinor           int64              `json:"net_charged_minor"`
	Currency                  money.Currency     `json:"currency"`
	VendedCount               int                `json:"vended_count"`
	RefundedCount             int                `json:"refunded_count"`
	Units                     []UnitStatementRow `json:"units"`
}

type MonthlyStatement struct {
	OrganisationID            uuid.UUID           `json:"organisation_id"`
	OrganisationName          string              `json:"organisation_name"`
	Month                     string              `json:"month"`
	Timezone                  string              `json:"timezone"`
	Description               string              `json:"description"`
	ElectricityPurchasedMinor int64               `json:"electricity_purchased_minor"`
	FeesMinor                 int64               `json:"fees_minor"`
	RefundsMinor              int64               `json:"refunds_minor"`
	NetChargedMinor           int64               `json:"net_charged_minor"`
	Currency                  money.Currency      `json:"currency"`
	VendedCount               int                 `json:"vended_count"`
	RefundedCount             int                 `json:"refunded_count"`
	Properties                []PropertyStatement `json:"properties"`
}

// SanitizeCSVCell prevents spreadsheet formula injection (=, +, -, @, \t, \r)
// on untrusted user strings such as property or unit names.
func SanitizeCSVCell(s string) string {
	if s == "" {
		return ""
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// FormatStatementCSV renders a ledger-derived MonthlyStatement as UTF-8 CSV,
// explicitly labelling principal "electricity purchased" (never "used").
func FormatStatementCSV(st *MonthlyStatement) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{
		"month",
		"property_id",
		"property_name",
		"unit_id",
		"unit_name",
		"masked_meter_number",
		"disco",
		"description",
		"electricity_purchased_minor",
		"fees_minor",
		"refunds_minor",
		"net_charged_minor",
		"currency",
		"vended_count",
		"refunded_count",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, p := range st.Properties {
		for _, u := range p.Units {
			rec := []string{
				st.Month,
				u.PropertyID.String(),
				SanitizeCSVCell(u.PropertyName),
				u.UnitID.String(),
				SanitizeCSVCell(u.UnitName),
				u.MaskedMeterNumber,
				u.Disco,
				StatementDescription,
				strconv.FormatInt(u.ElectricityPurchasedMinor, 10),
				strconv.FormatInt(u.FeesMinor, 10),
				strconv.FormatInt(u.RefundsMinor, 10),
				strconv.FormatInt(u.NetChargedMinor, 10),
				string(u.Currency),
				strconv.Itoa(u.VendedCount),
				strconv.Itoa(u.RefundedCount),
			}
			if err := w.Write(rec); err != nil {
				return nil, err
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
