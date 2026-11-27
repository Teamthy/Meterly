// Package meters holds the meter domain types. Meter numbers are stored
// encrypted, indexed by keyed hash, and displayed masked. Access to a meter
// is granted per user; knowing a meter number alone never grants access.
package meters

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type Permission string

const (
	PermView     Permission = "VIEW"
	PermPurchase Permission = "PURCHASE"
	PermManage   Permission = "MANAGE"
)

var permRank = map[Permission]int{PermView: 1, PermPurchase: 2, PermManage: 3}

// Allows reports whether holding p satisfies a requirement of want.
func (p Permission) Allows(want Permission) bool {
	held, okHeld := permRank[p]
	needed, okNeeded := permRank[want]
	return okHeld && okNeeded && held >= needed
}

// Capability distinguishes the levels from spec FR-003. A model that
// theoretically supports remote credit is not automatically reachable.
type Capability struct {
	Supported      bool       `json:"supported"`        // meter model / provider reports support
	Authorised     bool       `json:"authorised"`       // we have an authorised integration for it
	Reachable      *bool      `json:"reachable"`        // nil = not observable
	Confirmed      bool       `json:"confirmed"`        // proven by a confirmed delivery
	LastVerifiedAt *time.Time `json:"last_verified_at"` // nil = never checked
}

type Capabilities struct {
	TokenVending  Capability `json:"token_vending"`
	RemoteCredit  Capability `json:"remote_credit"`
	RemoteReading Capability `json:"remote_reading"`
}

// RemoteCreditUsable reports whether remote delivery may be attempted.
func (c Capabilities) RemoteCreditUsable() bool {
	r := c.RemoteCredit
	return r.Supported && r.Authorised && (r.Reachable == nil || *r.Reachable)
}

// RemoteReadingConfirmed reports whether AutoPower low-balance mode is allowed:
// remote_reading must be supported, authorised, confirmed and not marked unreachable.
func (c Capabilities) RemoteReadingConfirmed() bool {
	r := c.RemoteReading
	return r.Supported && r.Authorised && r.Confirmed && (r.Reachable == nil || *r.Reachable)
}

type Meter struct {
	ID              uuid.UUID    `json:"meter_id"`
	NumberEncrypted []byte       `json:"-"`
	KeyVersion      string       `json:"-"`
	NumberHash      string       `json:"-"`
	Masked          string       `json:"masked_meter_number"`
	DiscoCode       string       `json:"disco"`
	MeterType       string       `json:"meter_type"`
	CustomerName    string       `json:"customer_name"`
	Address         string       `json:"address"`
	ValidatedAt     time.Time    `json:"validated_at"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Grant struct {
	MeterID    uuid.UUID
	UserID     string
	Permission Permission
	Label      string
	GrantedBy  string
	RevokedAt  *time.Time
}

// Listed is a meter as seen by one user.
type Listed struct {
	Meter
	Label      string     `json:"label"`
	Permission Permission `json:"permission"`
}

var (
	ErrInvalidMeter = errors.New("meters: meter could not be validated")
	ErrNotFound     = errors.New("meters: not found")
	ErrForbidden    = errors.New("meters: no access to this meter")
)
