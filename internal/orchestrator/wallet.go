package orchestrator

import (
	"context"
	"time"

	"github.com/teamthy/meterly/internal/providers"
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
	paused := bal <= 0 || (o.walletMin > 0 && bal < o.walletMin)
	if o.metrics != nil {
		o.metrics.SetVendingHealth(bal, paused)
	}
	if paused {
		return VendingHealth{Paused: true, Reason: "wallet_low"}
	}
	return VendingHealth{}
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
			paused := o.walletBal <= 0 || (o.walletMin > 0 && o.walletBal < o.walletMin)
			if paused {
				return o.walletBal, true
			}
		}
		return 0, false
	}
	o.walletBal = got.Minor
	o.walletAt = now
	o.walletOK = true
	if got.Minor <= 0 || (o.walletMin > 0 && got.Minor < o.walletMin) {
		o.log.Warn("vending wallet below floor")
	}
	return got.Minor, true
}
