package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/providers"
)

// OutcomeOf classifies a provider call return error into Prometheus/OTel outcomes:
// ok (nil), ambiguous (timeout/5xx/lost response) or definitive (explicit rejection).
func OutcomeOf(err error) Outcome {
	if err == nil {
		return OutcomeOK
	}
	if providers.IsAmbiguous(err) {
		return OutcomeAmbiguous
	}
	return OutcomeDefinitive
}

func observeCall[T any](
	ctx context.Context,
	reg *Registry,
	tr *Tracer,
	providerName, op string,
	fn func(context.Context) (T, error),
) (T, error) {
	spanCtx, sp := tr.StartSpan(ctx, "provider."+providerName+"."+op, SpanKindClient,
		StringAttr("meterly.provider", providerName),
		StringAttr("meterly.operation", op),
	)
	start := time.Now()
	out, err := fn(spanCtx)
	dur := time.Since(start)
	outcome := OutcomeOf(err)
	reg.ObserveProviderCall(providerName, op, outcome, dur)
	if sp != nil {
		sp.SetAttr(StringAttr("meterly.outcome", string(outcome)))
		sp.End(err != nil)
	}
	return out, err
}

// ---- PaymentProvider wrapper --------------------------------------------------

type paymentWrapper struct {
	inner providers.PaymentProvider
	reg   *Registry
	tr    *Tracer
}

func WrapPaymentProvider(p providers.PaymentProvider, reg *Registry, tr *Tracer) providers.PaymentProvider {
	if p == nil || (reg == nil && !tr.Enabled()) {
		return p
	}
	return &paymentWrapper{inner: p, reg: reg, tr: tr}
}

func (w *paymentWrapper) Name() string { return w.inner.Name() }

func (w *paymentWrapper) InitializePayment(ctx context.Context, req providers.PaymentInit) (providers.PaymentInitResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "initialize_payment", func(c context.Context) (providers.PaymentInitResult, error) {
		return w.inner.InitializePayment(c, req)
	})
}

func (w *paymentWrapper) ChargeSaved(ctx context.Context, req providers.ChargeRequest) (providers.PaymentVerification, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "charge_saved", func(c context.Context) (providers.PaymentVerification, error) {
		return w.inner.ChargeSaved(c, req)
	})
}

func (w *paymentWrapper) VerifyPayment(ctx context.Context, providerRef string) (providers.PaymentVerification, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "verify_payment", func(c context.Context) (providers.PaymentVerification, error) {
		return w.inner.VerifyPayment(c, providerRef)
	})
}

func (w *paymentWrapper) RefundPayment(ctx context.Context, req providers.RefundRequest) (string, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "refund_payment", func(c context.Context) (string, error) {
		return w.inner.RefundPayment(c, req)
	})
}

func (w *paymentWrapper) ParseWebhook(h http.Header, body []byte) (providers.WebhookEvent, error) {
	return w.inner.ParseWebhook(h, body)
}

// ---- VendingProvider wrapper --------------------------------------------------

type vendingWrapper struct {
	inner providers.VendingProvider
	reg   *Registry
	tr    *Tracer
}

func WrapVendingProvider(p providers.VendingProvider, reg *Registry, tr *Tracer) providers.VendingProvider {
	if p == nil || (reg == nil && !tr.Enabled()) {
		return p
	}
	return &vendingWrapper{inner: p, reg: reg, tr: tr}
}

func (w *vendingWrapper) Name() string { return w.inner.Name() }

func (w *vendingWrapper) NewVendKey(transactionID string, at time.Time) string {
	return w.inner.NewVendKey(transactionID, at)
}

func (w *vendingWrapper) ValidateMeter(ctx context.Context, req providers.MeterValidationRequest) (providers.MeterValidationResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "validate_meter", func(c context.Context) (providers.MeterValidationResult, error) {
		return w.inner.ValidateMeter(c, req)
	})
}

func (w *vendingWrapper) Vend(ctx context.Context, req providers.VendRequest) (providers.VendResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "vend", func(c context.Context) (providers.VendResult, error) {
		return w.inner.Vend(c, req)
	})
}

func (w *vendingWrapper) GetVendStatus(ctx context.Context, idempotencyKey string) (providers.VendResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "get_vend_status", func(c context.Context) (providers.VendResult, error) {
		return w.inner.GetVendStatus(c, idempotencyKey)
	})
}

// ---- RemoteCreditProvider wrapper ---------------------------------------------

type remoteWrapper struct {
	inner providers.RemoteCreditProvider
	reg   *Registry
	tr    *Tracer
}

func WrapRemoteCreditProvider(p providers.RemoteCreditProvider, reg *Registry, tr *Tracer) providers.RemoteCreditProvider {
	if p == nil || (reg == nil && !tr.Enabled()) {
		return p
	}
	return &remoteWrapper{inner: p, reg: reg, tr: tr}
}

func (w *remoteWrapper) Name() string { return w.inner.Name() }

func (w *remoteWrapper) CheckEligibility(ctx context.Context, meterNumber string) (bool, *bool, error) {
	type pair struct {
		eligible  bool
		reachable *bool
	}
	out, err := observeCall(ctx, w.reg, w.tr, w.inner.Name(), "check_eligibility", func(c context.Context) (pair, error) {
		el, rc, err := w.inner.CheckEligibility(c, meterNumber)
		return pair{eligible: el, reachable: rc}, err
	})
	return out.eligible, out.reachable, err
}

func (w *remoteWrapper) InitiateCredit(ctx context.Context, req providers.RemoteCreditRequest) (providers.RemoteCreditResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "initiate_credit", func(c context.Context) (providers.RemoteCreditResult, error) {
		return w.inner.InitiateCredit(c, req)
	})
}

func (w *remoteWrapper) GetCreditStatus(ctx context.Context, idempotencyKey string) (providers.RemoteCreditResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "get_credit_status", func(c context.Context) (providers.RemoteCreditResult, error) {
		return w.inner.GetCreditStatus(c, idempotencyKey)
	})
}

// ---- Notification Provider wrapper --------------------------------------------

type notifProviderWrapper struct {
	inner notifications.Provider
	reg   *Registry
	tr    *Tracer
}

type notifStatusProviderWrapper struct {
	notifProviderWrapper
	checker notifications.StatusChecker
}

func WrapNotificationProvider(p notifications.Provider, reg *Registry, tr *Tracer) notifications.Provider {
	if p == nil || (reg == nil && !tr.Enabled()) {
		return p
	}
	base := notifProviderWrapper{inner: p, reg: reg, tr: tr}
	if sc, ok := p.(notifications.StatusChecker); ok {
		return &notifStatusProviderWrapper{notifProviderWrapper: base, checker: sc}
	}
	return &base
}

func (w *notifProviderWrapper) Name() string                   { return w.inner.Name() }
func (w *notifProviderWrapper) Channel() notifications.Channel { return w.inner.Channel() }

func (w *notifProviderWrapper) SendDelivery(ctx context.Context, req notifications.SendRequest) (notifications.SendResult, error) {
	return observeCall(ctx, w.reg, w.tr, w.inner.Name(), "send_notification", func(c context.Context) (notifications.SendResult, error) {
		return w.inner.SendDelivery(c, req)
	})
}

func (w *notifStatusProviderWrapper) CheckDeliveryStatus(ctx context.Context, idempotencyKey, providerRef string) (notifications.ProviderStatus, string, error) {
	type triple struct {
		st   notifications.ProviderStatus
		code string
	}
	out, err := observeCall(ctx, w.reg, w.tr, w.inner.Name(), "check_notification_status", func(c context.Context) (triple, error) {
		st, code, err := w.checker.CheckDeliveryStatus(c, idempotencyKey, providerRef)
		return triple{st: st, code: code}, err
	})
	return out.st, out.code, err
}

// ---- MeterReadingProvider wrapper --------------------------------------------

type readingWrapper struct {
	inner providers.MeterReadingProvider
	reg   *Registry
	tr    *Tracer
}

func WrapMeterReadingProvider(p providers.MeterReadingProvider, reg *Registry, tr *Tracer) providers.MeterReadingProvider {
	if p == nil || (reg == nil && !tr.Enabled()) {
		return p
	}
	return &readingWrapper{inner: p, reg: reg, tr: tr}
}

func (w *readingWrapper) Name() string { return w.inner.Name() }

func (w *readingWrapper) LatestReading(ctx context.Context, meterNumber string) (float64, time.Time, error) {
	type pair struct {
		kwh float64
		at  time.Time
	}
	out, err := observeCall(ctx, w.reg, w.tr, w.inner.Name(), "latest_reading", func(c context.Context) (pair, error) {
		kwh, at, err := w.inner.LatestReading(c, meterNumber)
		return pair{kwh: kwh, at: at}, err
	})
	return out.kwh, out.at, err
}

var (
	_ providers.PaymentProvider      = (*paymentWrapper)(nil)
	_ providers.VendingProvider      = (*vendingWrapper)(nil)
	_ providers.RemoteCreditProvider = (*remoteWrapper)(nil)
	_ providers.MeterReadingProvider = (*readingWrapper)(nil)
	_ notifications.Provider         = (*notifProviderWrapper)(nil)
	_ notifications.StatusChecker    = (*notifStatusProviderWrapper)(nil)
)
