// Package pilot is the P4 allow-list for a controlled first cohort.
//
// PILOT_MODE=true (process config) restricts purchases to listed users and
// meters. When any DISCO row exists, the meter's DisCo must be listed too.
// Daily value caps are config, not rows. Mutations are the caller's to audit.
package pilot

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrNotFound = errors.New("pilot: not found")

type Kind string

const (
	KindUser  Kind = "USER"
	KindMeter Kind = "METER"
	KindDisco Kind = "DISCO"
)

func ParseKind(s string) (Kind, bool) {
	k := Kind(strings.ToUpper(strings.TrimSpace(s)))
	switch k {
	case KindUser, KindMeter, KindDisco:
		return k, true
	}
	return "", false
}

type Entry struct {
	Kind      Kind
	Value     string
	Reason    string
	CreatedAt time.Time
	CreatedBy string
}

type Repo interface {
	List(ctx context.Context, kind Kind) ([]Entry, error)
	Add(ctx context.Context, e Entry) error
	Remove(ctx context.Context, kind Kind, value string) error
	Has(ctx context.Context, kind Kind, value string) (bool, error)
	CountKind(ctx context.Context, kind Kind) (int, error)
}
