// Package integrity runs post-restore checks: trial balance, snapshot gauges,
// and a bounded transaction window. It never prints tokens or meter numbers.
package integrity

import (
	"context"
	"fmt"
	"time"

	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Finding struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Count  int64  `json:"count,omitempty"`
}

type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	OK          bool      `json:"ok"`
	Findings    []Finding `json:"findings"`
}

func Check(ctx context.Context, st store.Store, now time.Time) (Report, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep := Report{GeneratedAt: now.UTC(), OK: true}
	add := func(f Finding) {
		if !f.OK {
			rep.OK = false
		}
		rep.Findings = append(rep.Findings, f)
	}

	tb, err := st.Repos().Ledger().TrialBalance(ctx)
	if err != nil {
		return rep, err
	}
	add(Finding{Name: "trial_balance", OK: tb == 0, Detail: fmt.Sprintf("sum=%d", tb), Count: tb})

	snap, err := st.MetricsSnapshot(ctx, now)
	if err != nil {
		return rep, err
	}
	var txnN int64
	for _, n := range snap.TransactionsByStatus {
		txnN += n
	}
	add(Finding{
		Name: "metrics_snapshot", OK: true,
		Detail: fmt.Sprintf("txns=%d review_open=%d outbox_lag_s=%.0f", txnN, snap.ManualReviewOpen, snap.OutboxLagSeconds),
		Count:  txnN,
	})
	add(Finding{
		Name: "outbox_lag", OK: snap.OutboxLagSeconds < 3600,
		Detail: fmt.Sprintf("%.0fs", snap.OutboxLagSeconds),
	})

	from := now.Add(-24 * time.Hour)
	list, err := st.Repos().Transactions().ListCreatedBetween(ctx, from, now)
	if err != nil {
		return rep, err
	}
	var unknown, confirmed int64
	for _, t := range list {
		if t == nil {
			continue
		}
		if t.Vending == tx.VendUnknown || t.Delivery == tx.DelUnknown {
			unknown++
		}
		if t.Payment == tx.PayConfirmed {
			confirmed++
		}
	}
	add(Finding{
		Name: "last_24h", OK: true,
		Detail: fmt.Sprintf("rows=%d payment_confirmed=%d unknown=%d", len(list), confirmed, unknown),
		Count:  int64(len(list)),
	})
	return rep, nil
}
