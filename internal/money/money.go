// Package money represents monetary amounts as integer minor units.
// Floating-point types must never be used for money.
package money

import (
	"errors"
	"fmt"
)

// Currency is an ISO 4217 code.
type Currency string

const NGN Currency = "NGN"

// Amount is a value in minor units (kobo for NGN).
type Amount struct {
	Minor    int64    `json:"amount_minor"`
	Currency Currency `json:"currency"`
}

var ErrCurrencyMismatch = errors.New("money: currency mismatch")

func New(minor int64, c Currency) Amount { return Amount{Minor: minor, Currency: c} }

func (a Amount) Add(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, ErrCurrencyMismatch
	}
	return Amount{Minor: a.Minor + b.Minor, Currency: a.Currency}, nil
}

func (a Amount) String() string {
	return fmt.Sprintf("%s %d.%02d", a.Currency, a.Minor/100, abs(a.Minor%100))
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
