package orchestrator

import (
	"context"
	"time"

	"github.com/teamthy/meterly/internal/providers"
	tx "github.com/teamthy/meterly/internal/transactions"
)

const walletCacheTTL = 30 * time.Second

// VendingHealth is the public /readyz vending slice. Never includes the
// naira figure — that is ops-only via metrics.
type VendingHealth struct {
	Paused bool
	Reason string
}

// SetWallet attaches an optional aggregator wallet. minMinor is the floor
// in kobo below which vending pauses (0 = pause only at empty).
func (o *Orchestrator) SetWallet(p providers.WalletProvider, minMinor int64) {
	if o == nil {
		return
	}
	o.wallet = p
	o.walletMin = minMinor
	o.walletTTL = walletCacheTTL
}

func (o *Orchestrator) ensureVendingOpen(ctx context.Context) error {
	if o.VendingHealth(ctx).Paused {
		return ErrVendingPaused
	}
	return nil
}

func (o *Orchestrator) VendingHealth(ctx context.Context) VendingHealth {
	if o == nil || o.wallet == nil {
		return VendingHealth{}
	}
	bal, ok := o.cachedWallet(ctx)
	if !ok {
		return VendingHealth{}
	}
	avg := o.threeDayAvgDailySpend(ctx)
	paused := walletShouldPause(bal, o.walletMin, avg)
	if o.metrics != nil {
		o.metrics.SetVendingHealth(bal, paused)
	}
	if paused {
		return VendingHealth{Paused: true, Reason: "wallet_low"}
	}
	return VendingHealth{}
}

func walletShouldPause(bal, minMinor, avgDaily int64) bool {
	floor := minMinor
	if avgDaily > floor {
		floor = avgDaily
	}
	return bal <= 0 || (floor > 0 && bal < floor)
}

func (o *Orchestrator) cachedWallet(ctx context.Context) (int64, bool) {
	now := o.Now()
	ttl := o.walletTTL
	if ttl <= 0 {
		ttl = walletCacheTTL
	}

	o.walletMu.Lock()
	if o.walletOK && !o.walletAt.IsZero() && now.Sub(o.walletAt) < ttl {
		bal := o.walletBal
		o.walletMu.Unlock()
		return bal, true
	}
	o.walletMu.Unlock()

	pctx, cancel := o.pctx(ctx)
	got, err := o.wallet.WalletBalance(pctx)
	cancel()

	o.walletMu.Lock()
	defer o.walletMu.Unlock()
	if err != nil {
		o.log.Warn("vending wallet lookup failed", "err", err)
		if o.walletOK && !o.walletAt.IsZero() && now.Sub(o.walletAt) < 5*ttl {
			avg := o.spendAvg
			paused := walletShouldPause(o.walletBal, o.walletMin, avg)
			if paused {
				return o.walletBal, true
			}
		}
		return 0, false
	}
	o.walletBal = got.Minor
	o.walletAt = now
	o.walletOK = true
	if walletShouldPause(got.Minor, o.walletMin, o.spendAvg) {
		o.log.Warn("vending wallet below floor")
	}
	return got.Minor, true
}

// threeDayAvgDailySpend is the mean daily confirmed electricity value over
// the last 72 hours (kobo). Lookup errors fail open (return 0).
func (o *Orchestrator) threeDayAvgDailySpend(ctx context.Context) int64 {
	if o == nil || o.st == nil {
		return 0
	}
	now := o.Now()
	ttl := o.walletTTL
	if ttl <= 0 {
		ttl = walletCacheTTL
	}
	o.walletMu.Lock()
	if !o.spendAt.IsZero() && now.Sub(o.spendAt) < ttl {
		v := o.spendAvg
		o.walletMu.Unlock()
		return v
	}
	o.walletMu.Unlock()

	from := now.Add(-72 * time.Hour)
	list, err := o.st.Repos().Transactions().ListCreatedBetween(ctx, from, now.Add(time.Second))
	if err != nil {
		o.log.Warn("vending spend window lookup failed", "err", err)
		return 0
	}
	var sum int64
	for _, t := range list {
		if t == nil || t.Payment != tx.PayConfirmed {
			continue
		}
		sum += t.Amount.Minor
	}
	avg := sum / 3
	o.walletMu.Lock()
	o.spendAvg = avg
	o.spendAt = now
	o.walletMu.Unlock()
	return avg
}
