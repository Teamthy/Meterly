// Package orchestrator owns the purchase workflow across payment, vending
// and delivery (spec §6.5). Rules enforced here:
//
//   - Vending starts only after payment is verified server-side, and the
//     verified amount matches the quote.
//   - A provider timeout is UNKNOWN, never FAILED. Unknown operations are
//     resolved by status queries; they are re-sent only when the provider
//     confirms it has no record of them.
//   - Remote-credit ACCEPTED is not delivery. Only meter confirmation is.
//   - Every money movement is a balanced, idempotent ledger posting.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/secure"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Config struct {
	FeeMinor        int64         // flat convenience fee per purchase
	MinAmountMinor  int64         // minimum electricity value
	MaxAmountMinor  int64         // maximum per purchase
	QuoteTTL        time.Duration // how long a quote is valid
	ProviderTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{FeeMinor: 10000, MinAmountMinor: 50000, MaxAmountMinor: 50000000, QuoteTTL: 15 * time.Minute, ProviderTimeout: 20 * time.Second}
}

type DeliveryPreference string

const (
	PrefAuto   DeliveryPreference = "AUTO"
	PrefRemote DeliveryPreference = "REMOTE"
	PrefToken  DeliveryPreference = "TOKEN"
)

type Quote struct {
	ID              uuid.UUID           `json:"quote_id"`
	CustomerID      string              `json:"-"`
	MeterID         uuid.UUID           `json:"meter_id"`
	Amount          money.Amount        `json:"amount"`
	Fee             money.Amount        `json:"fee"`
	Total           money.Amount        `json:"total"`
	DeliveryMethod  tx.DeliveryMethod   `json:"delivery_method"`
	DeliveryOptions []tx.DeliveryMethod `json:"delivery_options"`
	ExpiresAt       time.Time           `json:"expires_at"`
	used            bool
}

var (
	ErrAmountOutOfRange = errors.New("amount out of allowed range")
	ErrNotEligible      = errors.New("meter is not eligible for remote credit delivery")
	ErrQuoteExpired     = errors.New("quote expired or already used")
	ErrQuoteNotFound    = errors.New("quote not found")
	ErrPaymentMismatch  = errors.New("verified payment does not match the transaction")
	ErrTokenUnavailable = errors.New("token not available for this transaction")
)

type Orchestrator struct {
	cfg      Config
	Meters   *meters.Registry
	Txns     *tx.Store
	Ledger   *ledger.Ledger
	Outbox   *events.Outbox
	box      *secure.Box
	payments providers.PaymentProvider
	vending  providers.VendingProvider
	remote   providers.RemoteCreditProvider // nil if no AMI/HES integration
	log      *slog.Logger

	qmu    sync.Mutex
	quotes map[uuid.UUID]*Quote
}

func New(cfg Config, m *meters.Registry, s *tx.Store, l *ledger.Ledger, o *events.Outbox, box *secure.Box,
	pay providers.PaymentProvider, vend providers.VendingProvider, remote providers.RemoteCreditProvider, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{cfg: cfg, Meters: m, Txns: s, Ledger: l, Outbox: o, box: box, payments: pay, vending: vend, remote: remote, log: log, quotes: map[uuid.UUID]*Quote{}}
}

// ---- Quotes ----------------------------------------------------------------

func (o *Orchestrator) CreateQuote(customerID string, meterID uuid.UUID, amountMinor int64, pref DeliveryPreference) (*Quote, error) {
	if amountMinor < o.cfg.MinAmountMinor || amountMinor > o.cfg.MaxAmountMinor {
		return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
	}
	m, err := o.Meters.Authorize(customerID, meterID, meters.PermPurchase)
	if err != nil {
		return nil, err
	}
	options := []tx.DeliveryMethod{tx.MethodToken}
	remoteOK := o.remote != nil && m.Capabilities.RemoteCreditUsable()
	if remoteOK {
		options = append([]tx.DeliveryMethod{tx.MethodRemote}, options...)
	}
	method := tx.MethodToken
	switch pref {
	case PrefRemote:
		if !remoteOK {
			return nil, ErrNotEligible
		}
		method = tx.MethodRemote
	case PrefAuto, "":
		if remoteOK {
			method = tx.MethodRemote
		}
	}
	amt := money.New(amountMinor, money.NGN)
	fee := money.New(o.cfg.FeeMinor, money.NGN)
	total, _ := amt.Add(fee)
	q := &Quote{ID: uuid.New(), CustomerID: customerID, MeterID: meterID, Amount: amt, Fee: fee, Total: total,
		DeliveryMethod: method, DeliveryOptions: options, ExpiresAt: time.Now().UTC().Add(o.cfg.QuoteTTL)}
	o.qmu.Lock()
	o.quotes[q.ID] = q
	o.qmu.Unlock()
	return q, nil
}

func (o *Orchestrator) GetQuote(customerID string, id uuid.UUID) (*Quote, error) {
	o.qmu.Lock()
	defer o.qmu.Unlock()
	q, ok := o.quotes[id]
	if !ok || q.CustomerID != customerID {
		return nil, ErrQuoteNotFound
	}
	cp := *q
	return &cp, nil
}

// ---- Transaction creation --------------------------------------------------

type CreateRequest struct {
	CustomerID    string
	QuoteID       uuid.UUID
	Email         string
	IsGift        bool
	RecipientName string
}

// CreateTransaction consumes a quote and initialises payment. Callers must
// wrap it with idempotency handling.
func (o *Orchestrator) CreateTransaction(ctx context.Context, req CreateRequest) (*tx.Transaction, error) {
	o.qmu.Lock()
	q, ok := o.quotes[req.QuoteID]
	if !ok || q.CustomerID != req.CustomerID {
		o.qmu.Unlock()
		return nil, ErrQuoteNotFound
	}
	if q.used || time.Now().After(q.ExpiresAt) {
		o.qmu.Unlock()
		return nil, ErrQuoteExpired
	}
	q.used = true
	quote := *q
	o.qmu.Unlock()

	// Re-check access: it may have been revoked since the quote.
	if _, err := o.Meters.Authorize(req.CustomerID, quote.MeterID, meters.PermPurchase); err != nil {
		return nil, err
	}

	t := &tx.Transaction{
		ID: uuid.New(), CustomerID: req.CustomerID, MeterID: quote.MeterID, QuoteID: quote.ID,
		IsGift: req.IsGift, RecipientName: req.RecipientName,
		Amount: quote.Amount, Fee: quote.Fee, Total: quote.Total,
		Payment: tx.PayCreated, Vending: tx.VendNotStarted, Delivery: tx.DelNotStarted, DeliveryMethod: quote.DeliveryMethod,
	}
	o.Txns.Create(t)
	o.Outbox.Add("electricity.transaction.created", t.ID, map[string]any{"status": t.Status()})

	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	res, err := o.payments.InitializePayment(pctx, providers.PaymentInit{Reference: t.ID.String(), Amount: t.Total, Email: req.Email})
	if err != nil {
		// Payment never started for this reference; the customer can retry.
		_, _ = o.Txns.Update(t.ID, func(x *tx.Transaction) error { return x.SetPayment(tx.PayFailed, "payment init failed") })
		return nil, fmt.Errorf("initialise payment: %w", err)
	}
	updated, err := o.Txns.Update(t.ID, func(x *tx.Transaction) error {
		x.PaymentProviderRef, x.CheckoutURL = res.ProviderRef, res.CheckoutURL
		return x.SetPayment(tx.PayPending, "checkout created via "+o.payments.Name())
	})
	if err != nil {
		return nil, err
	}
	o.Outbox.Add("payment.pending", t.ID, nil)
	return updated, nil
}

// ---- Payment confirmation ----------------------------------------------------

// HandlePaymentNotification processes a (signature-verified) webhook or a
// client "I've paid" ping. It always re-verifies with the provider; the
// notification body is never trusted for the outcome. Safe to call any
// number of times.
func (o *Orchestrator) HandlePaymentNotification(ctx context.Context, providerRef string) (*tx.Transaction, error) {
	t, err := o.Txns.ByPaymentRef(providerRef)
	if err != nil {
		return nil, err
	}
	if t.Payment != tx.PayPending {
		return o.advance(ctx, t.ID) // duplicate notification: just make progress
	}
	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	v, err := o.payments.VerifyPayment(pctx, providerRef)
	if err != nil {
		return t, fmt.Errorf("verify payment: %w", err) // stays PENDING; retry later
	}
	switch v.Status {
	case providers.PaymentPending:
		return t, nil
	case providers.PaymentFailed:
		t, err = o.Txns.Update(t.ID, func(x *tx.Transaction) error { return x.SetPayment(tx.PayFailed, "provider reports failure") })
		if err == nil {
			o.Outbox.Add("payment.failed", t.ID, nil)
		}
		return o.ignoreRace(t, err)
	case providers.PaymentSucceeded:
		if v.Amount != t.Total {
			o.Outbox.Add("payment.mismatch", t.ID, map[string]any{"expected": t.Total.Minor, "got": v.Amount.Minor})
			o.log.Warn("payment amount mismatch", "txn", t.ID, "expected", t.Total.Minor, "got", v.Amount.Minor)
			return t, ErrPaymentMismatch // leave PENDING for manual review; never vend
		}
		t, err = o.Txns.Update(t.ID, func(x *tx.Transaction) error {
			return x.SetPayment(tx.PayConfirmed, "verified with "+o.payments.Name())
		})
		if errors.Is(err, tx.ErrInvalidTransition) {
			return o.advance(ctx, t.ID) // a concurrent handler won
		}
		if err != nil {
			return nil, err
		}
		if _, err := o.Ledger.Post(ledger.Transaction{
			Reference: "pay:" + t.ID.String(), SourceType: "electricity_transaction", SourceID: t.ID,
			Description: "Customer payment confirmed",
			Entries: []ledger.Entry{
				{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: t.Total},
				{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: t.Total},
			},
		}); err != nil {
			return nil, err
		}
		o.Outbox.Add("payment.confirmed", t.ID, nil)
		return o.advance(ctx, t.ID)
	}
	return t, fmt.Errorf("unexpected payment status %q", v.Status)
}

// advance moves a transaction forward from whatever state it is in.
func (o *Orchestrator) advance(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Txns.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Payment == tx.PayConfirmed && t.Vending == tx.VendNotStarted {
		if t, err = o.vend(ctx, id); err != nil {
			return t, err
		}
	}
	if t.Vending == tx.VendConfirmed && t.Delivery == tx.DelNotStarted {
		return o.deliver(ctx, id)
	}
	return t, nil
}

func (o *Orchestrator) ignoreRace(t *tx.Transaction, err error) (*tx.Transaction, error) {
	if errors.Is(err, tx.ErrInvalidTransition) {
		return o.Txns.Get(t.ID)
	}
	return t, err
}

// ---- Vending -----------------------------------------------------------------

func vendKey(id uuid.UUID) string   { return "vend:" + id.String() }
func creditKey(id uuid.UUID) string { return "credit:" + id.String() }

func (o *Orchestrator) vend(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	// Claim: only one worker can move NOT_STARTED/UNKNOWN -> PENDING.
	t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
		return x.SetVending(tx.VendPending, "vend requested via "+o.vending.Name())
	})
	if err != nil {
		return o.ignoreRace(&tx.Transaction{ID: id}, err)
	}
	o.Outbox.Add("electricity.vending.pending", id, nil)
	m, err := o.Meters.Get(t.MeterID)
	if err != nil {
		return t, err
	}
	number, err := o.Meters.Number(m)
	if err != nil {
		return t, err
	}
	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	res, err := o.vending.Vend(pctx, providers.VendRequest{
		TransactionID: id.String(), MeterNumber: number, DiscoCode: m.DiscoCode, Amount: t.Amount, IdempotencyKey: vendKey(id),
	})
	if err != nil {
		note := "vend error: " + err.Error()
		if providers.IsAmbiguous(err) {
			note = "vend outcome unknown (timeout); reconciliation required"
		}
		// Any error after send could mean the provider vended. Never assume failure.
		t, uerr := o.Txns.Update(id, func(x *tx.Transaction) error { return x.SetVending(tx.VendUnknown, note) })
		if uerr == nil {
			o.Outbox.Add("electricity.vending.unknown", id, nil)
		}
		return t, uerr
	}
	return o.applyVendResult(ctx, id, res)
}

func (o *Orchestrator) applyVendResult(ctx context.Context, id uuid.UUID, res providers.VendResult) (*tx.Transaction, error) {
	switch res.Status {
	case providers.VendSucceeded:
		ct, err := o.box.Encrypt(res.Token)
		if err != nil {
			return nil, err
		}
		t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
			x.VendProviderRef, x.TokenCiphertext, x.TokenKeyVersion, x.UnitsKWh = res.ProviderRef, ct, o.box.KeyVersion, res.UnitsKWh
			return x.SetVending(tx.VendConfirmed, "provider ref "+res.ProviderRef)
		})
		if err != nil {
			return o.ignoreRace(&tx.Transaction{ID: id}, err)
		}
		entries := []ledger.Entry{
			{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
			{Account: ledger.ProviderPayable, Direction: ledger.Credit, Amount: t.Amount},
		}
		if t.Fee.Minor > 0 {
			entries = append(entries, ledger.Entry{Account: ledger.PlatformFeeRevenue, Direction: ledger.Credit, Amount: t.Fee})
		}
		if _, err := o.Ledger.Post(ledger.Transaction{Reference: "vend:" + id.String(), SourceType: "electricity_transaction", SourceID: id, Description: "Electricity vended", Entries: entries}); err != nil {
			return nil, err
		}
		o.Outbox.Add("electricity.vending.completed", id, map[string]any{"units_kwh": res.UnitsKWh})
		return o.deliver(ctx, id)
	case providers.VendFailed:
		t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
			x.FailureCode = res.FailureCode
			return x.SetVending(tx.VendFailed, "provider failure "+res.FailureCode)
		})
		if err != nil {
			return o.ignoreRace(&tx.Transaction{ID: id}, err)
		}
		o.Outbox.Add("electricity.vending.failed", id, map[string]any{"code": res.FailureCode})
		return o.refund(ctx, t.ID, "vending failed: "+res.FailureCode)
	case providers.VendPending:
		return o.Txns.Get(id) // provider still working; reconciliation will pick it up
	}
	return nil, fmt.Errorf("unexpected vend status %q", res.Status)
}

// ---- Delivery -------------------------------------------------------------------

func (o *Orchestrator) deliver(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Txns.Update(id, func(x *tx.Transaction) error { return x.SetDelivery(tx.DelPending, "method "+string(x.DeliveryMethod)) })
	if err != nil {
		return o.ignoreRace(&tx.Transaction{ID: id}, err)
	}
	if t.DeliveryMethod == tx.MethodToken {
		// Token path: delivery means the token is securely available to the
		// customer. Recipient notification is tracked separately.
		t, err = o.Txns.Update(id, func(x *tx.Transaction) error { return x.SetDelivery(tx.DelConfirmed, "token available") })
		if err == nil {
			o.Outbox.Add("electricity.delivery.confirmed", id, map[string]any{"method": "TOKEN"})
			o.Outbox.Add("token.available", id, map[string]any{"gift": t.IsGift})
		}
		return t, err
	}
	return o.sendRemoteCredit(ctx, t)
}

func (o *Orchestrator) sendRemoteCredit(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	id := t.ID
	m, err := o.Meters.Get(t.MeterID)
	if err != nil {
		return t, err
	}
	number, err := o.Meters.Number(m)
	if err != nil {
		return t, err
	}
	token, err := o.box.Decrypt(t.TokenCiphertext)
	if err != nil {
		return t, err
	}
	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	res, err := o.remote.InitiateCredit(pctx, providers.RemoteCreditRequest{
		TransactionID: id.String(), MeterNumber: number, Token: token, Amount: t.Amount, IdempotencyKey: creditKey(id),
	})
	if err != nil {
		t, uerr := o.Txns.Update(id, func(x *tx.Transaction) error {
			return x.SetDelivery(tx.DelUnknown, "remote credit outcome unknown; do not resend before reconciling")
		})
		if uerr == nil {
			o.Outbox.Add("electricity.delivery.unknown", id, nil)
		}
		return t, uerr
	}
	return o.applyCreditResult(id, res)
}

func (o *Orchestrator) applyCreditResult(id uuid.UUID, res providers.RemoteCreditResult) (*tx.Transaction, error) {
	var to tx.DeliveryState
	var evt string
	switch res.Status {
	case providers.CreditAccepted:
		to, evt = tx.DelAccepted, "electricity.delivery.accepted"
	case providers.CreditConfirmed:
		to, evt = tx.DelConfirmed, "electricity.delivery.confirmed"
	case providers.CreditFailed:
		to, evt = tx.DelFailed, "electricity.delivery.failed"
	default:
		return nil, fmt.Errorf("unexpected credit status %q", res.Status)
	}
	t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
		if x.Delivery == to {
			return nil // already there
		}
		if res.ProviderRef != "" {
			x.DeliveryProviderRef = res.ProviderRef
		}
		if res.FailureCode != "" {
			x.FailureCode = res.FailureCode
		}
		return x.SetDelivery(to, "remote credit "+string(res.Status))
	})
	if err != nil {
		return o.ignoreRace(&tx.Transaction{ID: id}, err)
	}
	o.Outbox.Add(evt, id, map[string]any{"method": "REMOTE"})
	if to == tx.DelConfirmed {
		o.Meters.MarkRemoteCreditConfirmed(t.MeterID)
	}
	return t, nil
}

// FallbackToToken is an operator action after a confirmed remote-delivery
// failure: the already-vended token is released to the customer. STS tokens
// are single-use per meter, so releasing it cannot double-credit.
func (o *Orchestrator) FallbackToToken(id uuid.UUID, operator string) (*tx.Transaction, error) {
	t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
		if x.Delivery != tx.DelFailed {
			return fmt.Errorf("%w: fallback only allowed after confirmed delivery failure", tx.ErrInvalidTransition)
		}
		if err := x.SetDelivery(tx.DelPending, "fallback to token by "+operator); err != nil {
			return err
		}
		x.DeliveryMethod = tx.MethodToken
		return x.SetDelivery(tx.DelConfirmed, "token released")
	})
	if err == nil {
		o.Outbox.Add("token.available", id, map[string]any{"fallback": true})
	}
	return t, err
}

// ---- Reconciliation -------------------------------------------------------------

// Reconcile resolves ambiguous vending/delivery states by querying
// providers. It is safe to run repeatedly and concurrently.
func (o *Orchestrator) Reconcile(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Txns.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Vending == tx.VendUnknown || t.Vending == tx.VendPending {
		pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
		res, err := o.vending.GetVendStatus(pctx, vendKey(id))
		cancel()
		if err != nil {
			return t, err
		}
		if res.Status == providers.VendNotFound {
			if t.Vending != tx.VendUnknown {
				return t, nil
			}
			// Provider confirms it never vended: re-sending is safe.
			if _, err := o.Txns.Update(id, func(x *tx.Transaction) error {
				return x.SetVending(tx.VendPending, "provider has no record; safe to retry")
			}); err != nil {
				return o.ignoreRace(t, err)
			}
			return o.resendVend(ctx, id)
		}
		if t.Vending == tx.VendUnknown && res.Status != providers.VendPending {
			if _, err := o.Txns.Update(id, func(x *tx.Transaction) error {
				return x.SetVending(tx.VendPending, "reconciled: provider reports "+string(res.Status))
			}); err != nil {
				return o.ignoreRace(t, err)
			}
		}
		return o.applyVendResult(ctx, id, res)
	}
	if t.Vending == tx.VendConfirmed && t.Delivery == tx.DelNotStarted {
		return o.deliver(ctx, id)
	}
	if t.DeliveryMethod == tx.MethodRemote && (t.Delivery == tx.DelUnknown || t.Delivery == tx.DelAccepted) {
		pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
		res, err := o.remote.GetCreditStatus(pctx, creditKey(id))
		cancel()
		if err != nil {
			return t, err
		}
		if res.Status == providers.CreditNotFound {
			if t.Delivery != tx.DelUnknown {
				return t, nil
			}
			t, err = o.Txns.Update(id, func(x *tx.Transaction) error {
				return x.SetDelivery(tx.DelPending, "provider has no record; safe to resend")
			})
			if err != nil {
				return o.ignoreRace(t, err)
			}
			return o.sendRemoteCredit(ctx, t)
		}
		return o.applyCreditResult(id, res)
	}
	return t, nil
}

// resendVend calls the provider again for a transaction already in PENDING.
func (o *Orchestrator) resendVend(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Txns.Get(id)
	if err != nil {
		return nil, err
	}
	m, err := o.Meters.Get(t.MeterID)
	if err != nil {
		return t, err
	}
	number, err := o.Meters.Number(m)
	if err != nil {
		return t, err
	}
	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	res, err := o.vending.Vend(pctx, providers.VendRequest{TransactionID: id.String(), MeterNumber: number, DiscoCode: m.DiscoCode, Amount: t.Amount, IdempotencyKey: vendKey(id)})
	if err != nil {
		t, uerr := o.Txns.Update(id, func(x *tx.Transaction) error { return x.SetVending(tx.VendUnknown, "retry outcome unknown") })
		return t, uerr
	}
	return o.applyVendResult(ctx, id, res)
}

// ReconcileAll runs Reconcile over every ambiguous transaction.
func (o *Orchestrator) ReconcileAll(ctx context.Context) (resolved int) {
	for _, id := range o.Txns.NeedsReconciliation() {
		before, _ := o.Txns.Get(id)
		after, err := o.Reconcile(ctx, id)
		if err != nil {
			o.log.Warn("reconcile failed", "txn", id, "err", err)
			continue
		}
		if before != nil && after != nil && after.Status() != before.Status() {
			resolved++
		}
	}
	return resolved
}

// ---- Refunds -----------------------------------------------------------------------

// refund is used only when vending definitively failed, so the customer
// provably received no electricity.
func (o *Orchestrator) refund(ctx context.Context, id uuid.UUID, reason string) (*tx.Transaction, error) {
	t, err := o.Txns.Update(id, func(x *tx.Transaction) error {
		if x.Vending != tx.VendFailed {
			return fmt.Errorf("%w: refund requires definitive vending failure", tx.ErrInvalidTransition)
		}
		return x.SetPayment(tx.PayRefundPending, reason)
	})
	if err != nil {
		return o.ignoreRace(&tx.Transaction{ID: id}, err)
	}
	if _, err := o.Ledger.Post(ledger.Transaction{
		Reference: "refund-due:" + id.String(), SourceType: "electricity_transaction", SourceID: id, Description: "Refund owed: " + reason,
		Entries: []ledger.Entry{
			{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
			{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: t.Total},
		},
	}); err != nil {
		return nil, err
	}
	o.Outbox.Add("electricity.refund.pending", id, nil)

	pctx, cancel := context.WithTimeout(ctx, o.cfg.ProviderTimeout)
	defer cancel()
	ref, err := o.payments.RefundPayment(pctx, providers.RefundRequest{ProviderRef: t.PaymentProviderRef, Amount: t.Total, Reference: "refund:" + id.String()})
	if err != nil {
		o.log.Warn("refund call failed; stays REFUND_PENDING", "txn", id, "err", err)
		return t, nil
	}
	t, err = o.Txns.Update(id, func(x *tx.Transaction) error {
		x.RefundProviderRef = ref
		return x.SetPayment(tx.PayRefunded, "refund "+ref)
	})
	if err != nil {
		return o.ignoreRace(&tx.Transaction{ID: id}, err)
	}
	if _, err := o.Ledger.Post(ledger.Transaction{
		Reference: "refund-paid:" + id.String(), SourceType: "electricity_transaction", SourceID: id, Description: "Refund paid",
		Entries: []ledger.Entry{
			{Account: ledger.RefundPayable, Direction: ledger.Debit, Amount: t.Total},
			{Account: ledger.PSPSettlementReceivable, Direction: ledger.Credit, Amount: t.Total},
		},
	}); err != nil {
		return nil, err
	}
	o.Outbox.Add("electricity.refund.completed", id, nil)
	return t, nil
}

// ---- Token access ----------------------------------------------------------------

// Token returns the decrypted token for the purchaser. It is available on
// the token path, or after a remote-delivery failure was resolved to token.
func (o *Orchestrator) Token(customerID string, id uuid.UUID) (string, *tx.Transaction, error) {
	t, err := o.Txns.Get(id)
	if err != nil {
		return "", nil, err
	}
	if t.CustomerID != customerID {
		return "", nil, tx.ErrNotFound // do not reveal existence
	}
	if t.Vending != tx.VendConfirmed || len(t.TokenCiphertext) == 0 || t.DeliveryMethod != tx.MethodToken {
		return "", t, ErrTokenUnavailable
	}
	tok, err := o.box.Decrypt(t.TokenCiphertext)
	return tok, t, err
}
