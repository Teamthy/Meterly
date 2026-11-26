package meters

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidCohort is a malformed operator-attested remote-credit cohort.
var ErrInvalidCohort = errors.New("meters: invalid remote credit cohort")

// RemoteCohort is an operator-attested AMI meter group. It answers "does this
// DisCo+prefix look like a smart meter we are allowed to push credit to?"
// It is not proof of a live HES session: Authorised on the meter snapshot
// still requires a configured RemoteCreditProvider and CheckEligibility.
type RemoteCohort struct {
	ID           uuid.UUID  `json:"id"`
	DiscoCode    string     `json:"disco"`
	NumberPrefix string     `json:"number_prefix"`
	Manufacturer string     `json:"manufacturer,omitempty"`
	Authorised   bool       `json:"authorised"`
	Evidence     string     `json:"evidence"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

// NewRemoteCohort validates and builds a cohort. Prefix is digits only so
// matching cannot accidentally treat a glob as "all meters".
func NewRemoteCohort(disco, prefix, manufacturer, evidence, createdBy string, now time.Time) (RemoteCohort, error) {
	var zero RemoteCohort
	disco = strings.ToUpper(strings.TrimSpace(disco))
	prefix = strings.TrimSpace(prefix)
	manufacturer = strings.TrimSpace(manufacturer)
	evidence = strings.TrimSpace(evidence)
	createdBy = strings.TrimSpace(createdBy)
	if !validDisco(disco) || !validPrefix(prefix) || createdBy == "" {
		return zero, ErrInvalidCohort
	}
	if l := runeLen(evidence); l < 5 || l > 500 {
		return zero, ErrInvalidCohort
	}
	if runeLen(manufacturer) > 64 {
		return zero, ErrInvalidCohort
	}
	return RemoteCohort{
		ID:           uuid.New(),
		DiscoCode:    disco,
		NumberPrefix: prefix,
		Manufacturer: manufacturer,
		Authorised:   true,
		Evidence:     evidence,
		CreatedBy:    createdBy,
		CreatedAt:    now.UTC(),
	}, nil
}

// Matches reports whether an unrevoked authorised cohort covers disco+number.
func (c RemoteCohort) Matches(disco, meterNumber string) bool {
	if c.RevokedAt != nil || !c.Authorised {
		return false
	}
	if c.DiscoCode != strings.ToUpper(strings.TrimSpace(disco)) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(meterNumber), c.NumberPrefix)
}

func validDisco(s string) bool {
	if len(s) < 2 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validPrefix(s string) bool {
	if len(s) < 1 || len(s) > 13 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
