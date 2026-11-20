package ledger

import (
	"errors"
	"testing"

	"github.com/teamthy/meterly/internal/money"
)

func ngn(v int64) money.Amount { return money.New(v, money.NGN) }

func TestUnbalancedRejected(t *testing.T) {
	l := New()
	_, err := l.Post(Transaction{Reference: "x", Entries: []Entry{
		{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(100)},
		{Account: CustomerFundsClearing, Direction: Credit, Amount: ngn(99)},
	}})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("got %v", err)
	}
}

func TestPostingIsIdempotent(t *testing.T) {
	l := New()
	e := []Entry{
		{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(100)},
		{Account: CustomerFundsClearing, Direction: Credit, Amount: ngn(100)},
	}
	a, _ := l.Post(Transaction{Reference: "pay:1", Entries: e})
	b, _ := l.Post(Transaction{Reference: "pay:1", Entries: e})
	if a.ID != b.ID || l.Balance(PSPSettlementReceivable, money.NGN) != 100 {
		t.Fatal("duplicate post must be a no-op")
	}
}

func TestNonPositiveRejected(t *testing.T) {
	err := Validate([]Entry{
		{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(0)},
		{Account: CustomerFundsClearing, Direction: Credit, Amount: ngn(0)},
	})
	if !errors.Is(err, ErrNonPositive) {
		t.Fatalf("got %v", err)
	}
}
