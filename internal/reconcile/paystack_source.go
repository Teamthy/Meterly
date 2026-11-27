package reconcile

import (
	"context"
	"time"

	"github.com/teamthy/meterly/internal/providers/paystack"
)

// PaystackSource adapts paystack.Client to PaymentStatement.
type PaystackSource struct{ Client *paystack.Client }

func (s PaystackSource) ListTransactions(ctx context.Context, from, to time.Time) ([]ListedPayment, error) {
	rows, err := s.Client.ListTransactions(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ListedPayment, 0, len(rows))
	for _, r := range rows {
		out = append(out, ListedPayment{Reference: r.Reference, Status: r.Status, Amount: r.Amount, Currency: r.Currency, PaidAt: r.PaidAt})
	}
	return out, nil
}

func (s PaystackSource) ListRefunds(ctx context.Context, from, to time.Time) ([]ListedRefund, error) {
	rows, err := s.Client.ListRefunds(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ListedRefund, 0, len(rows))
	for _, r := range rows {
		out = append(out, ListedRefund{TransactionRef: r.TransactionRef, Amount: r.Amount, Status: r.Status})
	}
	return out, nil
}

func (s PaystackSource) ListSettlements(ctx context.Context, from, to time.Time) ([]ListedSettlement, error) {
	rows, err := s.Client.ListSettlements(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ListedSettlement, 0, len(rows))
	for _, r := range rows {
		out = append(out, ListedSettlement{
			ID: r.ID, Status: r.Status, Currency: r.Currency,
			TotalAmount: r.TotalAmount, EffectiveAmount: r.EffectiveAmount, TotalFees: r.TotalFees,
			SettlementDate: r.SettlementDate,
		})
	}
	return out, nil
}
