package ledger

import (
	"errors"
	"testing"

	"github.com/teamthy/meterly/internal/money"
)

func ngn(v int64) money.Amount { return money.New(v, money.NGN) }

func TestUnbalancedRejected(t *testing.T) {
	err := Validate([]Entry{
		{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(100)},
		{Account: CustomerFundsClearing, Direction: Credit, Amount: ngn(99)},
	})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("got %v", err)
	}
}

func TestMixedCurrencyMustBalancePerCurrency(t *testing.T) {
	err := Validate([]Entry{
		{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(100)},
		{Account: CustomerFundsClearing, Direction: Credit, Amount: money.New(100, "USD")},
	})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("got %v", err)
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

func TestSingleEntryRejected(t *testing.T) {
	if err := Validate([]Entry{{Account: PSPSettlementReceivable, Direction: Debit, Amount: ngn(1)}}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v", err)
	}
}
