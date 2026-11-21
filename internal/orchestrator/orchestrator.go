// Package orchestrator owns the purchase workflow across payment, vending
// and delivery (spec §6.5). Rules enforced here:
//
//   - Vending starts only after payment is verified server-side and the
//     verified amount equals the quote total; a mismatch goes to review.
//   - A provider timeout is UNKNOWN, never FAILED. Unknown operations are
//     resolved by status queries and re-sent only when the provider
//     confirms it has no record of them.
//   - Remote-credit ACCEPTED is not delivery. Only meter confirmation is.
//   - Every state change commits atomically with its history, ledger
//     posting and outbox events (one store.InTx). Provider calls never run
//     inside a database transaction.
//   - Any step can crash; Resume continues a transaction from whatever
//     state was last committed, without double-charging, double-vending or
//     double-crediting.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	tx "github.com/teamthy/meterly/internal/transactions"
)

type Config struct {
	FeeMinor        int64         // flat convenience fee per purchase
	MinAmountMinor  int64         // minimum electricity value
	MaxAmountMinor  int64         // maximum per purchase
	QuoteTTL        time.Duration // how long a quote is valid
	ProviderTimeout time.Duration
	IdempotencyTTL  time.Duration

	// PaymentExpiry: a payment still unpaid (per the provider) after this
	// long is marked FAILED. A later successful payment goes to review.
	PaymentExpiry time.Duration
	// Resume worker scheduling.
	ResumeStaleAfter  time.Duration // only touch transactions idle this long
	ResumeLease       time.Duration // claim lease so workers don't overlap
	ResumeBaseBackoff time.Duration
	ResumeMaxBackoff  time.Duration
	ResumeMaxAttempts int // then escalate to MANUAL_REVIEW
	ResumeBatch       int
}

func DefaultConfig() Config {
	return Config{
		FeeMinor: 10000, MinAmountMinor: 50000, MaxAmountMinor: 50000000,
		QuoteTTL: 15 * time.Minute, ProviderTimeout: 20 * time.Second, IdempotencyTTL: 24 * time.Hour,
		PaymentExpiry:    2 * time.Hour,
		ResumeStaleAfter: 2 * time.Minute, ResumeLease: 2 * time.Minute,
		ResumeBaseBackoff: 30 * time.Second, ResumeMaxBackoff: 30 * time.Minute,
		ResumeMaxAttempts: 12, ResumeBatch: 50,
	}
}

type DeliveryPreference string

const (
	PrefAuto   DeliveryPreference = "AUTO"
	PrefRemote DeliveryPreference = "REMOTE"
	PrefToken  DeliveryPreference = "TOKEN"
)

var (
	ErrAmountOutOfRange  = errors.New("amount out of allowed range")
	ErrNotEligible       = errors.New("meter is not eligible for remote credit delivery")
	ErrQuoteExpired      = errors.New("quote expired or already used")
	ErrQuoteNotFound     = errors.New("quote not found")
	ErrPaymentMismatch   = errors.New("verified payment does not match the transaction")
	ErrTokenUnavailable  = errors.New("token not available for this transaction")
	ErrInvalidPreference = errors.New("unknown delivery preference")
	// ErrProviderDisabled: an operator switched the provider off. The step
	// is paused, not failed; Resume retries once it is re-enabled.
	ErrProviderDisabled = errors.New("provider disabled by an operator")
)

// errSkip aborts a mutate without writing: the transaction is already past
// the step (another worker or request got there first).
var errSkip = errors.New("skip")

type Orchestrator struct {
	cfg      Config
	st       store.Store
	Meters   *registry.Registry
	box      *secure.Box
	payments providers.PaymentProvider
	vending  providers.VendingProvider
	remote   providers.RemoteCreditProvider // nil if no AMI/HES integration
	log      *slog.Logger
	Now      func() time.Time
}

func New(cfg Config, st store.Store, reg *registry.Registry, box *secure.Box,
	pay providers.PaymentProvider, vend providers.VendingProvider, remote providers.RemoteCreditProvider, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{cfg: cfg, st: st, Meters: reg, box: box, payments: pay, vending: vend, remote: remote, log: log,
		Now: func() time.Time { return time.Now().UTC() }}
}

func (o *Orchestrator) Store() store.Store { return o.st }

func (o *Orchestrator) pctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, o.cfg.ProviderTimeout)
}

// Get returns a transaction by ID.
func (o *Orchestrator) Get(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.st.Repos().Transactions().Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, tx.ErrNotFound
	}
	return t, err
}

func (o *Orchestrator) ListForCustomer(ctx context.Context, customerID string) ([]*tx.Transaction, error) {
	return o.st.Repos().Transactions().ListForCustomer(ctx, customerID)
}

// mutate locks the transaction, applies fn, and saves — together with any
// ledger postings and outbox events fn wrote through r — in one DB
// transaction. If fn returns errSkip nothing is written and the current
// state is returned.
func (o *Orchestrator) mutate(ctx context.Context, id uuid.UUID, fn func(t *tx.Transaction, r store.Repos) error) (*tx.Transaction, error) {
	var out *tx.Transaction
	err := o.st.InTx(ctx, func(r store.Repos) error {
		t, err := r.Transactions().GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if err := fn(t, r); err != nil {
			return err
		}
		if err := r.Transactions().Save(ctx, t); err != nil {
			return err
		}
		out = t
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return o.Get(ctx, id)
	case errors.Is(err, store.ErrNotFound):
		return nil, tx.ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return nil, tx.ErrConflict
	}
	return out, err
}

func emit(ctx context.Context, r store.Repos, typ string, id uuid.UUID, payload map[string]any) error {
	return r.Outbox().Add(ctx, events.New(typ, id, payload))
}

func post(ctx context.Context, r store.Repos, ref string, id uuid.UUID, desc string, entries ...ledger.Entry) error {
	_, err := r.Ledger().Post(ctx, ledger.Transaction{Reference: ref, SourceType: "electricity_transaction", SourceID: id, Description: desc, Entries: entries})
	return err
}

// ---- Quotes ----------------------------------------------------------------

func (o *Orchestrator) CreateQuote(ctx context.Context, customerID string, meterID uuid.UUID, amountMinor int64, pref DeliveryPreference) (*tx.Quote, error) {
	if amountMinor < o.cfg.MinAmountMinor || amountMinor > o.cfg.MaxAmountMinor {
		return nil, fmt.Errorf("%w: %d–%d kobo", ErrAmountOutOfRange, o.cfg.MinAmountMinor, o.cfg.MaxAmountMinor)
	}
	m, err := o.Meters.Authorize(ctx, customerID, meterID, meters.PermPurchase)
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
	case PrefToken:
	default:
		return nil, fmt.Errorf("%w %q", ErrInvalidPreference, pref)
	}
	amt := money.New(amountMinor, money.NGN)
	fee := money.New(o.cfg.FeeMinor, money.NGN)
	total, _ := amt.Add(fee)
	now := o.Now()
	q := &tx.Quote{ID: uuid.New(), CustomerID: customerID, MeterID: meterID, Amount: amt, Fee: fee, Total: total,
		DeliveryMethod: method, DeliveryOptions: options, ExpiresAt: now.Add(o.cfg.QuoteTTL), CreatedAt: now}
	if err := o.st.Repos().Quotes().Create(ctx, q); err != nil {
		return nil, err
	}
	return q, nil
}

func (o *Orchestrator) GetQuote(ctx context.Context, customerID string, id uuid.UUID) (*tx.Quote, error) {
	q, err := o.st.Repos().Quotes().Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && q.CustomerID != customerID) {
		return nil, ErrQuoteNotFound
	}
	return q, err
}

// ---- Transaction creation --------------------------------------------------

type CreateRequest struct {
	CustomerID    string
	QuoteID       uuid.UUID
	Email         string
	IsGift        bool
	RecipientName string
	// IdempotencyKey and RequestHash, when set, make creation idempotent:
	// the reservation, the transaction and the completion commit together.
	IdempotencyKey string
	RequestHash    string
}

const opCreateTransaction = "create_transaction"

// CreateTransaction consumes a quote, creates the transaction, then
// initialises payment. A replayed idempotent request returns the original
// transaction (replayed=true).
func (o *Orchestrator) CreateTransaction(ctx context.Context, req CreateRequest) (t *tx.Transaction, replayed bool, err error) {
	now := o.Now()
	err = o.st.InTx(ctx, func(r store.Repos) error {
		if req.IdempotencyKey != "" {
			existing, err := r.Idempotency().Reserve(ctx, idempotency.Record{
				Scope: req.CustomerID, Operation: opCreateTransaction, Key: req.IdempotencyKey, RequestHash: req.RequestHash,
				CreatedAt: now, ExpiresAt: now.Add(o.cfg.IdempotencyTTL),
			})
			if err != nil {
				return err
			}
			if existing != nil {
				if existing.RequestHash != req.RequestHash {
					return idempotency.ErrConflict
				}
				if existing.ResultRef == "" {
					return idempotency.ErrInProgress
				}
				id, err := uuid.Parse(existing.ResultRef)
				if err != nil {
					return err
				}
				t, err = r.Transactions().Get(ctx, id)
				replayed = true
				return err
			}
		}
		q, err := r.Quotes().GetForUpdate(ctx, req.QuoteID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && q.CustomerID != req.CustomerID) {
			return ErrQuoteNotFound
		}
		if err != nil {
			return err
		}
		if q.Used || now.After(q.ExpiresAt) {
			return ErrQuoteExpired
		}
		// Re-check access: it may have been revoked since the quote.
		if _, err := registry.AuthorizeIn(ctx, r, req.CustomerID, q.MeterID, meters.PermPurchase); err != nil {
			return err
		}
		if err := r.Quotes().MarkUsed(ctx, q.ID); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ErrQuoteExpired
			}
			return err
		}
		t = tx.New(req.CustomerID, q, req.IsGift, req.RecipientName, now)
		if err := r.Transactions().Create(ctx, t); err != nil {
			return err
		}
		if err := emit(ctx, r, "electricity.transaction.created", t.ID, map[string]any{"gift": t.IsGift}); err != nil {
			return err
		}
		if req.IdempotencyKey != "" {
			return r.Idempotency().Complete(ctx, req.CustomerID, opCreateTransaction, req.IdempotencyKey, t.ID.String())
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if t.Payment == tx.PayCreated {
		t, err = o.EnsurePaymentInitialised(ctx, t.ID, req.Email)
	}
	return t, replayed, err
}

// EnsurePaymentInitialised creates the checkout if it has not been created
// yet. Providers deduplicate on our reference (the transaction ID), so a
// retry after a crash returns the same checkout.
func (o *Orchestrator) EnsurePaymentInitialised(ctx context.Context, id uuid.UUID, email string) (*tx.Transaction, error) {
	t, err := o.Get(ctx, id)
	if err != nil || t.Payment != tx.PayCreated {
		return t, err
	}
	if err := o.requireEnabled(ctx, o.payments.Name()); err != nil {
		return t, err
	}
	pctx, cancel := o.pctx(ctx)
	res, err := o.payments.InitializePayment(pctx, providers.PaymentInit{Reference: t.ID.String(), Amount: t.Total, Email: email})
	cancel()
	if err != nil {
		// Stays CREATED: the client may retry with the same Idempotency-Key;
		// the resume worker fails it after PaymentExpiry.
		return t, fmt.Errorf("initialise payment: %w", err)
	}
	return o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Payment != tx.PayCreated {
			return errSkip
		}
		t.PaymentProviderName, t.PaymentProviderRef, t.CheckoutURL = o.payments.Name(), res.ProviderRef, res.CheckoutURL
		if err := t.SetPayment(tx.PayPending, "checkout created via "+o.payments.Name()); err != nil {
			return err
		}
		return emit(ctx, r, "payment.pending", t.ID, nil)
	})
}

// ---- Payment confirmation ----------------------------------------------------

// HandlePaymentNotification processes a (signature-verified) webhook or a
// client "I've paid" ping. It always re-verifies with the provider; the
// notification is never trusted for the outcome. Safe to call any number
// of times, concurrently.
func (o *Orchestrator) HandlePaymentNotification(ctx context.Context, providerRef string) (*tx.Transaction, error) {
	t, err := o.st.Repos().Transactions().ByPaymentRef(ctx, providerRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, tx.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch t.Payment {
	case tx.PayPending, tx.PayFailed:
		return o.checkPayment(ctx, t)
	}
	return o.advance(ctx, t.ID) // duplicate notification: just make progress
}

// checkPayment verifies a PENDING payment (or a FAILED one, to catch money
// that arrived after we gave up) and applies the provider's answer.
func (o *Orchestrator) checkPayment(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	pctx, cancel := o.pctx(ctx)
	v, err := o.payments.VerifyPayment(pctx, t.PaymentProviderRef)
	cancel()
	if err != nil {
		return t, fmt.Errorf("verify payment: %w", err) // unchanged; retried later
	}
	now := o.Now()
	var mismatch bool
	t, err = o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
		if t.Payment == tx.PayFailed {
			if v.Status != providers.PaymentSucceeded || t.ReviewReason != "" {
				return errSkip
			}
			t.FlagForReview("payment received after the transaction had failed; refund or fulfil manually")
			return emit(ctx, r, "payment.late", t.ID, map[string]any{"amount_minor": v.Amount.Minor})
		}
		if t.Payment != tx.PayPending || t.ReviewReason != "" {
			return errSkip
		}
		switch v.Status {
		case providers.PaymentPending:
			if now.Sub(t.CreatedAt) < o.cfg.PaymentExpiry {
				return errSkip
			}
			if err := t.SetPayment(tx.PayFailed, "expired: provider reports unpaid after "+o.cfg.PaymentExpiry.String()); err != nil {
				return err
			}
			return emit(ctx, r, "payment.failed", t.ID, map[string]any{"reason": "expired"})
		case providers.PaymentFailed:
			if err := t.SetPayment(tx.PayFailed, "provider reports failure"); err != nil {
				return err
			}
			return emit(ctx, r, "payment.failed", t.ID, nil)
		case providers.PaymentSucceeded:
			if v.Amount != t.Total {
				mismatch = true
				t.FlagForReview(fmt.Sprintf("payment amount mismatch: expected %d %s, provider reports %d %s",
					t.Total.Minor, t.Total.Currency, v.Amount.Minor, v.Amount.Currency))
				return emit(ctx, r, "payment.mismatch", t.ID, map[string]any{"expected": t.Total.Minor, "got": v.Amount.Minor})
			}
			if err := t.SetPayment(tx.PayConfirmed, "verified with "+o.payments.Name()); err != nil {
				return err
			}
			if err := post(ctx, r, "pay:"+t.ID.String(), t.ID, "Customer payment confirmed",
				ledger.Entry{Account: ledger.PSPSettlementReceivable, Direction: ledger.Debit, Amount: t.Total},
				ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Credit, Amount: t.Total},
			); err != nil {
				return err
			}
			return emit(ctx, r, "payment.confirmed", t.ID, nil)
		}
		return fmt.Errorf("unexpected payment status %q", v.Status)
	})
	if err != nil {
		return t, err
	}
	if mismatch {
		o.log.Warn("payment amount mismatch; flagged for review", "txn", t.ID)
		return t, ErrPaymentMismatch
	}
	return o.advance(ctx, t.ID)
}

// advance moves a confirmed-payment transaction forward as far as it can.
func (o *Orchestrator) advance(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	t, err := o.Get(ctx, id)
	if err != nil || t.ReviewReason != "" || t.Payment != tx.PayConfirmed {
		return t, err
	}
	if t.Vending == tx.VendNotStarted {
		if t, err = o.vend(ctx, id); err != nil {
			return t, err
		}
	}
	if t.Vending == tx.VendFailed && t.Payment == tx.PayConfirmed {
		return o.refund(ctx, id, "vending failed: "+t.FailureCode)
	}
	if t.Vending == tx.VendConfirmed && t.Delivery == tx.DelNotStarted {
		return o.deliver(ctx, id)
	}
	return t, nil
}

// ---- Vending -----------------------------------------------------------------

func vendKey(id uuid.UUID) string   { return "vend:" + id.String() }
func creditKey(id uuid.UUID) string { return "credit:" + id.String() }

// vend claims the vending step (NOT_STARTED -> PENDING) and calls the provider.
func (o *Orchestrator) vend(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	if err := o.requireEnabled(ctx, o.vending.Name()); err != nil {
		t, _ := o.Get(ctx, id)
		return t, err
	}
	claimed := false
	t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Vending != tx.VendNotStarted || t.Payment != tx.PayConfirmed || t.ReviewReason != "" {
			return errSkip
		}
		t.VendProviderName = o.vending.Name()
		if err := t.SetVending(tx.VendPending, "vend requested via "+o.vending.Name()); err != nil {
			return err
		}
		claimed = true
		return emit(ctx, r, "electricity.vending.pending", t.ID, nil)
	})
	// Only the worker that committed the claim may call the provider; a
	// loser sees PENDING too, but must leave the call to the winner.
	if err != nil || !claimed {
		return t, err
	}
	return o.callVend(ctx, t)
}

// callVend sends (or re-sends, under the same idempotency key) the vend
// request for a transaction whose vending is PENDING.
func (o *Orchestrator) callVend(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	m, err := o.Meters.Get(ctx, t.MeterID)
	if err != nil {
		return t, err
	}
	number, err := o.Meters.Number(m)
	if err != nil {
		return t, err
	}
	pctx, cancel := o.pctx(ctx)
	res, err := o.vending.Vend(pctx, providers.VendRequest{
		TransactionID: t.ID.String(), MeterNumber: number, DiscoCode: m.DiscoCode, Amount: t.Amount, IdempotencyKey: vendKey(t.ID),
	})
	cancel()
	if err != nil {
		// Any error after send could mean the provider vended. Never assume failure.
		note := "vend outcome unknown: " + err.Error()
		if providers.IsAmbiguous(err) {
			note = "vend outcome unknown (timeout); reconciliation required"
		}
		return o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
			if t.Vending != tx.VendPending {
				return errSkip
			}
			if err := t.SetVending(tx.VendUnknown, note); err != nil {
				return err
			}
			return emit(ctx, r, "electricity.vending.unknown", t.ID, nil)
		})
	}
	return o.applyVendResult(ctx, t.ID, res)
}

func (o *Orchestrator) applyVendResult(ctx context.Context, id uuid.UUID, res providers.VendResult) (*tx.Transaction, error) {
	inFlight := func(t *tx.Transaction) bool { return t.Vending == tx.VendPending || t.Vending == tx.VendUnknown }
	switch res.Status {
	case providers.VendSucceeded:
		ct, err := o.box.Encrypt(res.Token)
		if err != nil {
			return nil, err
		}
		t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
			if !inFlight(t) {
				return errSkip
			}
			t.VendProviderRef, t.TokenCiphertext, t.TokenKeyVersion, t.UnitsKWh = res.ProviderRef, ct, o.box.KeyVersion, res.UnitsKWh
			if err := t.SetVending(tx.VendConfirmed, "provider ref "+res.ProviderRef); err != nil {
				return err
			}
			entries := []ledger.Entry{
				{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
				{Account: ledger.ProviderPayable, Direction: ledger.Credit, Amount: t.Amount},
			}
			if t.Fee.Minor > 0 {
				entries = append(entries, ledger.Entry{Account: ledger.PlatformFeeRevenue, Direction: ledger.Credit, Amount: t.Fee})
			}
			if err := post(ctx, r, "vend:"+t.ID.String(), t.ID, "Electricity vended", entries...); err != nil {
				return err
			}
			return emit(ctx, r, "electricity.vending.completed", t.ID, map[string]any{"units_kwh": res.UnitsKWh})
		})
		if err != nil {
			return t, err
		}
		return o.deliver(ctx, id)
	case providers.VendFailed:
		t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
			if !inFlight(t) {
				return errSkip
			}
			t.FailureCode = res.FailureCode
			if err := t.SetVending(tx.VendFailed, "provider failure "+res.FailureCode); err != nil {
				return err
			}
			return emit(ctx, r, "electricity.vending.failed", t.ID, map[string]any{"code": res.FailureCode})
		})
		if err != nil {
			return t, err
		}
		return o.refund(ctx, id, "vending failed: "+res.FailureCode)
	case providers.VendPending:
		return o.Get(ctx, id) // provider still working; Resume picks it up
	}
	return nil, fmt.Errorf("unexpected vend status %q", res.Status)
}

// ---- Delivery -------------------------------------------------------------------

func (o *Orchestrator) deliver(ctx context.Context, id uuid.UUID) (*tx.Transaction, error) {
	cur, err := o.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.DeliveryMethod == tx.MethodRemote && cur.Delivery == tx.DelNotStarted {
		if err := o.requireEnabled(ctx, o.remote.Name()); err != nil {
			return cur, err
		}
	}
	claimed := false
	t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Delivery != tx.DelNotStarted || t.Vending != tx.VendConfirmed {
			return errSkip
		}
		if err := t.SetDelivery(tx.DelPending, "method "+string(t.DeliveryMethod)); err != nil {
			return err
		}
		if t.DeliveryMethod == tx.MethodRemote {
			t.DeliveryProviderName = o.remote.Name()
			claimed = true
			return nil
		}
		// Token path: delivery means the token is securely available to the
		// customer; confirmed in the same commit so it can't get stuck.
		// Recipient notification is tracked separately (outbox).
		if err := t.SetDelivery(tx.DelConfirmed, "token available"); err != nil {
			return err
		}
		if err := emit(ctx, r, "electricity.delivery.confirmed", t.ID, map[string]any{"method": "TOKEN"}); err != nil {
			return err
		}
		return emit(ctx, r, "token.available", t.ID, map[string]any{"gift": t.IsGift})
	})
	if err != nil || !claimed {
		return t, err
	}
	return o.sendRemoteCredit(ctx, t)
}

// sendRemoteCredit sends (or re-sends, under the same idempotency key) the
// credit command for a transaction whose remote delivery is PENDING.
func (o *Orchestrator) sendRemoteCredit(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	m, err := o.Meters.Get(ctx, t.MeterID)
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
	pctx, cancel := o.pctx(ctx)
	res, err := o.remote.InitiateCredit(pctx, providers.RemoteCreditRequest{
		TransactionID: t.ID.String(), MeterNumber: number, Token: token, Amount: t.Amount, IdempotencyKey: creditKey(t.ID),
	})
	cancel()
	if err != nil {
		return o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
			if t.Delivery != tx.DelPending {
				return errSkip
			}
			if err := t.SetDelivery(tx.DelUnknown, "remote credit outcome unknown; do not resend before reconciling"); err != nil {
				return err
			}
			return emit(ctx, r, "electricity.delivery.unknown", t.ID, nil)
		})
	}
	return o.applyCreditResult(ctx, t.ID, res)
}

func (o *Orchestrator) applyCreditResult(ctx context.Context, id uuid.UUID, res providers.RemoteCreditResult) (*tx.Transaction, error) {
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
	return o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		switch t.Delivery {
		case tx.DelPending, tx.DelAccepted, tx.DelUnknown:
		default:
			return errSkip
		}
		if t.Delivery == to {
			return errSkip
		}
		if res.ProviderRef != "" {
			t.DeliveryProviderRef = res.ProviderRef
		}
		if res.FailureCode != "" {
			t.FailureCode = res.FailureCode
		}
		if err := t.SetDelivery(to, "remote credit "+string(res.Status)); err != nil {
			return err
		}
		if to == tx.DelConfirmed {
			if err := registry.MarkRemoteCreditConfirmedIn(ctx, r, t.MeterID); err != nil {
				return err
			}
		}
		return emit(ctx, r, evt, t.ID, map[string]any{"method": "REMOTE"})
	})
}

// requireEnabled returns ErrProviderDisabled if an operator switched the
// provider off. Only new outbound work is gated; status queries are not.
func (o *Orchestrator) requireEnabled(ctx context.Context, name string) error {
	ok, err := o.st.Repos().Providers().Enabled(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderDisabled, name)
	}
	return nil
}

// ---- Refunds -----------------------------------------------------------------------

// refund is used only when vending definitively failed, so the customer
// provably received no electricity.
func (o *Orchestrator) refund(ctx context.Context, id uuid.UUID, reason string) (*tx.Transaction, error) {
	claimed := false
	t, err := o.mutate(ctx, id, func(t *tx.Transaction, r store.Repos) error {
		if t.Payment != tx.PayConfirmed || t.Vending != tx.VendFailed || t.ReviewReason != "" {
			return errSkip
		}
		t.RefundReason, t.RefundAmount = reason, t.Total
		if err := t.SetPayment(tx.PayRefundPending, reason); err != nil {
			return err
		}
		if err := post(ctx, r, "refund-due:"+t.ID.String(), t.ID, "Refund owed: "+reason,
			ledger.Entry{Account: ledger.CustomerFundsClearing, Direction: ledger.Debit, Amount: t.Total},
			ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Credit, Amount: t.Total},
		); err != nil {
			return err
		}
		claimed = true
		return emit(ctx, r, "electricity.refund.pending", t.ID, nil)
	})
	if err != nil || !claimed {
		return t, err // a concurrent worker owns the refund call (or Resume retries it)
	}
	return o.executeRefund(ctx, t)
}

// executeRefund asks the PSP to refund (idempotent by our reference) and
// records completion. On failure it stays REFUND_PENDING for Resume.
func (o *Orchestrator) executeRefund(ctx context.Context, t *tx.Transaction) (*tx.Transaction, error) {
	pctx, cancel := o.pctx(ctx)
	ref, err := o.payments.RefundPayment(pctx, providers.RefundRequest{ProviderRef: t.PaymentProviderRef, Amount: t.RefundAmount, Reference: "refund:" + t.ID.String()})
	cancel()
	if err != nil {
		o.log.Warn("refund call failed; stays REFUND_PENDING", "txn", t.ID, "err", err)
		return t, nil
	}
	return o.mutate(ctx, t.ID, func(t *tx.Transaction, r store.Repos) error {
		if t.Payment != tx.PayRefundPending {
			return errSkip
		}
		t.RefundProviderRef = ref
		if err := t.SetPayment(tx.PayRefunded, "refund "+ref); err != nil {
			return err
		}
		if err := post(ctx, r, "refund-paid:"+t.ID.String(), t.ID, "Refund paid",
			ledger.Entry{Account: ledger.RefundPayable, Direction: ledger.Debit, Amount: t.RefundAmount},
			ledger.Entry{Account: ledger.PSPSettlementReceivable, Direction: ledger.Credit, Amount: t.RefundAmount},
		); err != nil {
			return err
		}
		return emit(ctx, r, "electricity.refund.completed", t.ID, nil)
	})
}
