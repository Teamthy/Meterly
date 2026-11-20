// Package meters is the meter registry and capability engine (spec §4.3).
// Meter numbers are stored encrypted, indexed by keyed hash, and displayed
// masked. Access to a meter is granted per user; knowing a meter number
// alone never grants access to its tokens or history.
package meters

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/secure"
)

type Permission string

const (
	PermView     Permission = "VIEW"
	PermPurchase Permission = "PURCHASE"
	PermManage   Permission = "MANAGE"
)

func (p Permission) allows(want Permission) bool {
	rank := map[Permission]int{PermView: 1, PermPurchase: 2, PermManage: 3}
	return rank[p] >= rank[want]
}

// Capability distinguishes the five levels from spec FR-003. A model that
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

type Meter struct {
	ID              uuid.UUID    `json:"meter_id"`
	NumberEncrypted []byte       `json:"-"`
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

var (
	ErrInvalidMeter = errors.New("meters: meter could not be validated")
	ErrNotFound     = errors.New("meters: not found")
	ErrForbidden    = errors.New("meters: no access to this meter")
)

type Registry struct {
	mu      sync.RWMutex
	byID    map[uuid.UUID]*Meter
	byHash  map[string]uuid.UUID
	grants  []*Grant
	box     *secure.Box
	vending providers.VendingProvider
	remote  providers.RemoteCreditProvider // optional
	now     func() time.Time
}

func NewRegistry(box *secure.Box, v providers.VendingProvider, r providers.RemoteCreditProvider) *Registry {
	return &Registry{byID: map[uuid.UUID]*Meter{}, byHash: map[string]uuid.UUID{}, box: box, vending: v, remote: r, now: func() time.Time { return time.Now().UTC() }}
}

// Validate checks a meter with the vending provider and upserts the meter
// record. It grants no access by itself.
func (r *Registry) Validate(ctx context.Context, number, disco string) (*Meter, error) {
	res, err := r.vending.ValidateMeter(ctx, providers.MeterValidationRequest{MeterNumber: number, DiscoCode: disco})
	if err != nil {
		return nil, err
	}
	if !res.Valid {
		return nil, ErrInvalidMeter
	}
	now := r.now()
	caps := Capabilities{
		TokenVending:  Capability{Supported: true, Authorised: true, LastVerifiedAt: &now},
		RemoteReading: Capability{Supported: res.RemoteReadingSupported, LastVerifiedAt: &now},
		RemoteCredit:  Capability{Supported: res.RemoteCreditSupported, LastVerifiedAt: &now},
	}
	if res.RemoteCreditSupported && r.remote != nil {
		eligible, reachable, err := r.remote.CheckEligibility(ctx, number)
		if err == nil {
			caps.RemoteCredit.Authorised = eligible
			caps.RemoteCredit.Reachable = reachable
		}
	}

	hash := r.box.Hash(disco + ":" + number)
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.byHash[hash]; ok {
		m := r.byID[id]
		confirmed := m.Capabilities.RemoteCredit.Confirmed
		m.MeterType, m.CustomerName, m.Address, m.ValidatedAt = res.MeterType, res.CustomerName, res.Address, now
		m.Capabilities = caps
		m.Capabilities.RemoteCredit.Confirmed = confirmed && caps.RemoteCredit.Supported
		cp := *m
		return &cp, nil
	}
	enc, err := r.box.Encrypt(number)
	if err != nil {
		return nil, err
	}
	m := &Meter{
		ID: uuid.New(), NumberEncrypted: enc, NumberHash: hash, Masked: secure.Mask(number, 4),
		DiscoCode: disco, MeterType: res.MeterType, CustomerName: res.CustomerName, Address: res.Address,
		ValidatedAt: now, Capabilities: caps,
	}
	r.byID[m.ID] = m
	r.byHash[hash] = m.ID
	cp := *m
	return &cp, nil
}

// Register validates the meter and grants the user the given permission.
func (r *Registry) Register(ctx context.Context, userID, number, disco, label string, perm Permission) (*Meter, error) {
	m, err := r.Validate(ctx, number, disco)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.grants {
		if g.MeterID == m.ID && g.UserID == userID && g.RevokedAt == nil {
			if perm.allows(g.Permission) {
				g.Permission = perm
			}
			if label != "" {
				g.Label = label
			}
			return m, nil
		}
	}
	r.grants = append(r.grants, &Grant{MeterID: m.ID, UserID: userID, Permission: perm, Label: label, GrantedBy: userID})
	return m, nil
}

func (r *Registry) Get(id uuid.UUID) (*Meter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *m
	return &cp, nil
}

// Authorize returns the meter if the user holds at least `want` on it.
func (r *Registry) Authorize(userID string, id uuid.UUID, want Permission) (*Meter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	for _, g := range r.grants {
		if g.MeterID == id && g.UserID == userID && g.RevokedAt == nil && g.Permission.allows(want) {
			cp := *m
			return &cp, nil
		}
	}
	return nil, ErrForbidden
}

type Listed struct {
	Meter
	Label      string     `json:"label"`
	Permission Permission `json:"permission"`
}

func (r *Registry) ListForUser(userID string) []Listed {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Listed
	for _, g := range r.grants {
		if g.UserID == userID && g.RevokedAt == nil {
			out = append(out, Listed{Meter: *r.byID[g.MeterID], Label: g.Label, Permission: g.Permission})
		}
	}
	return out
}

func (r *Registry) Revoke(meterID uuid.UUID, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, g := range r.grants {
		if g.MeterID == meterID && g.UserID == userID && g.RevokedAt == nil {
			g.RevokedAt = &now
		}
	}
}

// Number decrypts the meter number for provider calls only.
func (r *Registry) Number(m *Meter) (string, error) { return r.box.Decrypt(m.NumberEncrypted) }

// MarkRemoteCreditConfirmed records evidence that remote delivery works.
func (r *Registry) MarkRemoteCreditConfirmed(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.byID[id]; ok {
		m.Capabilities.RemoteCredit.Confirmed = true
	}
}
