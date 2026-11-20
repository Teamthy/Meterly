package transactions

import (
	"errors"
	"testing"
)

func fresh() *Transaction {
	return &Transaction{Payment: PayCreated, Vending: VendNotStarted, Delivery: DelNotStarted}
}

func TestCannotVendBeforePayment(t *testing.T) {
	x := fresh()
	_ = x.SetPayment(PayPending, "")
	if err := x.SetVending(VendPending, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("got %v", err)
	}
}

func TestCannotDeliverBeforeVend(t *testing.T) {
	x := fresh()
	_ = x.SetPayment(PayPending, "")
	_ = x.SetPayment(PayConfirmed, "")
	if err := x.SetDelivery(DelPending, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownCannotBeRefundedDirectly(t *testing.T) {
	x := fresh()
	_ = x.SetPayment(PayPending, "")
	_ = x.SetPayment(PayConfirmed, "")
	_ = x.SetVending(VendPending, "")
	_ = x.SetVending(VendUnknown, "")
	if x.Status() != OverallReconciliation {
		t.Fatalf("got %s", x.Status())
	}
	if err := x.SetPayment(PayFailed, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("confirmed payment must not become FAILED")
	}
}

func TestConfirmedIsTerminalForDelivery(t *testing.T) {
	x := fresh()
	_ = x.SetPayment(PayPending, "")
	_ = x.SetPayment(PayConfirmed, "")
	_ = x.SetVending(VendPending, "")
	_ = x.SetVending(VendConfirmed, "")
	_ = x.SetDelivery(DelPending, "")
	_ = x.SetDelivery(DelConfirmed, "")
	if x.Status() != OverallCompleted {
		t.Fatalf("got %s", x.Status())
	}
	if err := x.SetDelivery(DelFailed, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("confirmed delivery must be terminal")
	}
	if len(x.History) != 6 {
		t.Fatalf("history %d", len(x.History))
	}
}
