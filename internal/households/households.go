// Package households implements FR-008: verified meter ownership and
// household membership. Knowing a meter number, or holding a legacy
// self-registered MANAGE grant, is not ownership.
package households

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
)

type Role string
type MemberStatus string
type OwnershipMethod string

const (
	RoleOwner         Role = "OWNER"
	RoleAdministrator Role = "ADMINISTRATOR"
	RoleMember        Role = "MEMBER"
	RoleViewer        Role = "VIEWER"
	RoleRecipient     Role = "RECIPIENT"

	StatusInvited  MemberStatus = "INVITED"
	StatusActive   MemberStatus = "ACTIVE"
	StatusDeclined MemberStatus = "DECLINED"
	StatusRevoked  MemberStatus = "REVOKED"

	MethodAccountNameMatch    OwnershipMethod = "ACCOUNT_NAME_MATCH"
	MethodOperatorAttestation OwnershipMethod = "OPERATOR_ATTESTATION"
)

var (
	ErrNotFound          = errors.New("households: not found")
	ErrForbidden         = errors.New("households: forbidden")
	ErrNotVerifiedOwner  = errors.New("households: verified ownership required")
	ErrNameMismatch      = errors.New("households: disco customer name does not match the verified account name")
	ErrNoDisplayName     = errors.New("households: verified display name is required for account-name match")
	ErrAlreadyMember     = errors.New("households: user is already a member")
	ErrAlreadyOwner      = errors.New("households: meter already has verified ownership for this user")
	ErrInvalidRole       = errors.New("households: invalid role")
	ErrInvalidInvite     = errors.New("households: invite is not pending")
	ErrCannotModifyOwner = errors.New("households: the household owner cannot be removed")
)

type Household struct {
	ID        uuid.UUID `json:"household_id"`
	OwnerID   string    `json:"owner_user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Member struct {
	ID          uuid.UUID    `json:"member_id"`
	HouseholdID uuid.UUID    `json:"household_id"`
	UserID      string       `json:"user_id"`
	Role        Role         `json:"role"`
	Status      MemberStatus `json:"status"`
	InvitedBy   string       `json:"invited_by,omitempty"`
	InvitedAt   time.Time    `json:"invited_at"`
	AcceptedAt  *time.Time   `json:"accepted_at,omitempty"`
	RevokedAt   *time.Time   `json:"revoked_at,omitempty"`
}

type MeterLink struct {
	HouseholdID uuid.UUID `json:"household_id"`
	MeterID     uuid.UUID `json:"meter_id"`
	AddedBy     string    `json:"added_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type Ownership struct {
	ID         uuid.UUID       `json:"ownership_id"`
	MeterID    uuid.UUID       `json:"meter_id"`
	UserID     string          `json:"user_id"`
	Method     OwnershipMethod `json:"method"`
	Reason     string          `json:"reason,omitempty"`
	VerifiedBy string          `json:"verified_by"`
	VerifiedAt time.Time       `json:"verified_at"`
	RevokedAt  *time.Time      `json:"revoked_at,omitempty"`
}

type View struct {
	Household Household    `json:"household"`
	Members   []Member     `json:"members"`
	Meters    []MeterLink  `json:"meters"`
	MyRole    Role         `json:"my_role,omitempty"`
	MyStatus  MemberStatus `json:"my_status,omitempty"`
}

func ParseRole(s string) (Role, error) {
	switch Role(strings.ToUpper(strings.TrimSpace(s))) {
	case RoleOwner, RoleAdministrator, RoleMember, RoleViewer, RoleRecipient:
		return Role(strings.ToUpper(strings.TrimSpace(s))), nil
	default:
		return "", ErrInvalidRole
	}
}

// PermissionForRole is the meter permission implied by an ACTIVE household
// role. OWNER/ADMINISTRATOR/MEMBER may purchase; VIEWER/RECIPIENT may only
// view. Household roles never mint meters.MANAGE.
func PermissionForRole(role Role) meters.Permission {
	switch role {
	case RoleOwner, RoleAdministrator, RoleMember:
		return meters.PermPurchase
	case RoleViewer, RoleRecipient:
		return meters.PermView
	default:
		return ""
	}
}

func CanAdminister(role Role) bool {
	return role == RoleOwner || role == RoleAdministrator
}

// NormalizePersonName collapses case, punctuation and whitespace so a DisCo
// customer_name can be compared to a verified OIDC display name. Empty after
// normalization never matches.
func NormalizePersonName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if unicode.IsSpace(r) || r == '-' || r == '\'' {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func NamesMatch(discoCustomerName, verifiedDisplayName string) bool {
	a, b := NormalizePersonName(discoCustomerName), NormalizePersonName(verifiedDisplayName)
	return a != "" && a == b
}
