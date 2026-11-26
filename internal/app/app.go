// Package app wires configuration, storage, providers, the orchestrator,
// the HTTP API, observability and the background loops. cmd/api and
// cmd/worker share it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/channels/ussd"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/identity"
	"github.com/teamthy/meterly/internal/notificationconsumer"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/notifications/resend"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/observability"
	"github.com/teamthy/meterly/internal/offline"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/platform/config"
	"github.com/teamthy/meterly/internal/platform/migrate"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/heshttp"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/providers/vtpass"
	"github.com/teamthy/meterly/internal/ratelimit"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/migrations"
)

// BuildOptions allows tests to inject custom providers, stores or HTTP
// middleware without changing production wiring.
type BuildOptions struct {
	Store                store.Store
	PaymentProvider      providers.PaymentProvider
	VendingProvider      providers.VendingProvider
	RemoteProvider       providers.RemoteCreditProvider
	MeterReadingProvider providers.MeterReadingProvider
	WrapPublicHTTP       func(http.Handler) http.Handler
}

type App struct {
	Cfg           config.Config
	Log           *slog.Logger
	Store         store.Store
	Orch          *orchestrator.Orchestrator
	Publisher     *outbox.Publisher
	Notifications *notificationservice.Service
	Offline       *offline.Service
	Metrics       *observability.Registry
	Tracer        *observability.Tracer
	pool          *pgxpool.Pool

	// Set only when the corresponding mock is in use (dev simulation).
	mockPay      *mock.Payments
	mockRemote   *mock.RemoteCredit
	MockReadings *mock.MeterReadings
	MockSMS      *notifications.MockSender
	MockEmail    *notifications.MockSender
	MockWhatsApp *notifications.MockSender
	payments     providers.PaymentProvider
	remoteRaw    providers.RemoteCreditProvider
	authn        auth.Authenticator
	Identity     *identity.Engine
	wrapPublic   func(http.Handler) http.Handler
	redis        *ratelimit.Client

	closeOnce sync.Once
}

func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	return BuildWithOptions(ctx, cfg, log, BuildOptions{})
}

func BuildWithOptions(ctx context.Context, cfg config.Config, log *slog.Logger, opts BuildOptions) (*App, error) {
	a := &App{
		Cfg:        cfg,
		Log:        log,
		Metrics:    observability.NewRegistry(),
		Tracer:     observability.NewTracer(observability.TracerConfig{ServiceName: "meterly", OTLPEndpoint: cfg.OTelEndpoint, Log: log}),
		wrapPublic: opts.WrapPublicHTTP,
	}
	fail := func(err error) (*App, error) {
		_ = a.Tracer.Shutdown(context.Background())
		if a.redis != nil {
			a.redis.Close()
		}
		if a.pool != nil {
			a.pool.Close()
		}
		return nil, err
	}

	encKey, hmacKey := cfg.TokenEncKey, cfg.MeterHMACKey
	if encKey == nil || hmacKey == nil {
		if cfg.Mode != config.Dev {
			return fail(errors.New("encryption keys are required outside dev"))
		}
		log.Warn("using ephemeral encryption keys; stored tokens and meter numbers become unreadable after restart")
		encKey, hmacKey = secure.RandomKey(32), secure.RandomKey(32)
	}
	box, err := secure.NewBox(encKey, hmacKey, cfg.KeyVersion)
	if err != nil {
		return fail(err)
	}

	switch {
	case opts.Store != nil:
		a.Store = opts.Store
	case cfg.DatabaseURL == "":
		log.Warn("using in-memory store; all data is lost on exit")
		a.Store = memstore.New()
	default:
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return fail(fmt.Errorf("connect database: %w", err))
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return fail(fmt.Errorf("ping database: %w", err))
		}
		a.pool = pool
		a.Store = pgstore.New(pool, cfg.ProviderEnvironment)
		if n, err := migrate.Pending(ctx, pool, migrations.FS); err != nil || n > 0 {
			if err == nil {
				err = fmt.Errorf("%d pending migration(s); run cmd/migrate first", n)
			}
			return fail(err)
		}
	}

	var rawPay providers.PaymentProvider
	switch {
	case opts.PaymentProvider != nil:
		rawPay = opts.PaymentProvider
	case cfg.PaymentProvider == "paystack":
		ps, err := paystack.New(paystack.Config{SecretKey: cfg.PaystackSecretKey, BaseURL: cfg.PaystackBaseURL, CallbackURL: cfg.PaystackCallbackURL})
		if err != nil {
			return fail(err)
		}
		rawPay = ps
	default:
		a.mockPay = mock.NewPayments()
		rawPay = a.mockPay
	}
	a.payments = observability.WrapPaymentProvider(rawPay, a.Metrics, a.Tracer)

	var rawVend providers.VendingProvider = opts.VendingProvider
	if rawVend == nil {
		switch cfg.VendingProvider {
		case "vtpass":
			vp, err := vtpass.New(vtpass.Config{
				APIKey:       cfg.VTPassAPIKey,
				SecretKey:    cfg.VTPassSecretKey,
				PublicKey:    cfg.VTPassPublicKey,
				BaseURL:      cfg.VTPassBaseURL,
				DefaultPhone: cfg.VTPassDefaultPhone,
			})
			if err != nil {
				return fail(err)
			}
			rawVend = vp
		default:
			rawVend = mock.NewVending()
		}
	}
	vend := observability.WrapVendingProvider(rawVend, a.Metrics, a.Tracer)

	var rawRemote providers.RemoteCreditProvider = opts.RemoteProvider
	if rawRemote == nil {
		switch cfg.RemoteProvider {
		case "mock":
			a.mockRemote = mock.NewRemoteCredit()
			rawRemote = a.mockRemote
		case "http":
			rc, err := heshttp.New(heshttp.Config{BaseURL: cfg.RemoteHESBaseURL, HMACSecret: cfg.RemoteHESHMACSecret})
			if err != nil {
				return fail(err)
			}
			rawRemote = rc
		}
	}
	a.remoteRaw = rawRemote
	remote := observability.WrapRemoteCreditProvider(rawRemote, a.Metrics, a.Tracer)

	var rawReadings providers.MeterReadingProvider = opts.MeterReadingProvider
	if rawReadings == nil && cfg.MeterReadingProvider == "mock" {
		a.MockReadings = mock.NewMeterReadings()
		rawReadings = a.MockReadings
	}
	readings := observability.WrapMeterReadingProvider(rawReadings, a.Metrics, a.Tracer)

	if cfg.AuthMode == "oidc" {
		o, err := auth.NewOIDC(auth.OIDCConfig{
			Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience, JWKSURL: cfg.OIDCJWKSURL,
			RolesClaim: cfg.OIDCRolesClaim, MFAValues: cfg.OIDCMFAValues, Log: log,
		})
		if err != nil {
			return fail(err)
		}
		a.authn = o
	} else {
		log.Warn("using dev authentication: the bearer token is the user; operator:/admin: prefixes grant staff roles")
		a.authn = auth.DevAuth{}
	}

	a.Identity = &identity.Engine{
		Users:   a.Store.Repos().Users(),
		Auth:    a.Store.Repos().Auth(),
		Box:     box,
		RPName:  "Meterly",
		Origins: cfg.WebAuthnOrigins,
		Dev:     cfg.Mode == config.Dev,
	}
	a.authn = auth.SessionThen(a.authn, a.Identity)

	if cfg.RedisURL != "" {
		rc, err := ratelimit.Dial(cfg.RedisURL)
		if err != nil {
			return fail(fmt.Errorf("REDIS_URL: %w", err))
		}
		a.redis = rc
		log.Info("rate limits shared via redis")
	}

	reg := registry.New(a.Store, box, vend, remote)
	a.Orch = orchestrator.New(cfg.Orchestrator, a.Store, reg, box, a.payments, vend, remote, log)
	a.Orch.SetMetrics(a.Metrics)
	a.Orch.SetMeterReadingProvider(readings)
	a.Orch.SetFeatureGiftSMS(cfg.FeatureGiftSMS)

	notifProvs := map[notifications.Channel]notifications.Provider{}
	inboundVers := map[string]notifications.InboundVerifier{}
	var notifProvNames []string

	switch cfg.SMSProvider {
	case "africastalking":
		at, err := africastalking.New(africastalking.Config{
			Username:      cfg.AfricasTalkingUsername,
			APIKey:        cfg.AfricasTalkingAPIKey,
			SenderID:      cfg.AfricasTalkingSenderID,
			WebhookSecret: cfg.AfricasTalkingWebhookSecret,
			BaseURL:       cfg.AfricasTalkingBaseURL,
		})
		if err != nil {
			return fail(err)
		}
		notifProvs[notifications.SMS] = observability.WrapNotificationProvider(at, a.Metrics, a.Tracer)
		inboundVers[at.Name()] = at
		notifProvNames = append(notifProvNames, at.Name())
	case "mock":
		ms, err := notifications.NewMockSender()
		if err != nil {
			return fail(err)
		}
		a.MockSMS = ms
		p := notifications.WrapSender("mocksms", notifications.SMS, a.MockSMS)
		notifProvs[notifications.SMS] = observability.WrapNotificationProvider(p, a.Metrics, a.Tracer)
		notifProvNames = append(notifProvNames, p.Name())
	}

	switch cfg.EmailProvider {
	case "resend":
		re, err := resend.New(resend.Config{
			APIKey:        cfg.ResendAPIKey,
			From:          cfg.ResendFrom,
			WebhookSecret: cfg.ResendWebhookSecret,
			BaseURL:       cfg.ResendBaseURL,
		})
		if err != nil {
			return fail(err)
		}
		notifProvs[notifications.Email] = observability.WrapNotificationProvider(re, a.Metrics, a.Tracer)
		inboundVers[re.Name()] = re
		notifProvNames = append(notifProvNames, re.Name())
	case "mock":
		me, err := notifications.NewMockSender()
		if err != nil {
			return fail(err)
		}
		a.MockEmail = me
		p := notifications.WrapSender("mockemail", notifications.Email, a.MockEmail)
		notifProvs[notifications.Email] = observability.WrapNotificationProvider(p, a.Metrics, a.Tracer)
		notifProvNames = append(notifProvNames, p.Name())
	}

	var waParser offline.WhatsAppInboundParser
	switch cfg.WhatsAppProvider {
	case "whatsapp":
		wa, err := whatsapp.New(whatsapp.Config{
			PhoneNumberID: cfg.WhatsAppPhoneNumberID,
			AccessToken:   cfg.WhatsAppAccessToken,
			AppSecret:     cfg.WhatsAppAppSecret,
			VerifyToken:   cfg.WhatsAppVerifyToken,
			APIVersion:    cfg.WhatsAppAPIVersion,
			BaseURL:       cfg.WhatsAppBaseURL,
		})
		if err != nil {
			return fail(err)
		}
		waParser = wa
		notifProvs[notifications.WhatsApp] = observability.WrapNotificationProvider(wa, a.Metrics, a.Tracer)
		inboundVers[wa.Name()] = wa
		notifProvNames = append(notifProvNames, wa.Name())
	case "mock":
		mw, err := notifications.NewMockSender()
		if err != nil {
			return fail(err)
		}
		a.MockWhatsApp = mw
		p := notifications.WrapSender("mockwhatsapp", notifications.WhatsApp, a.MockWhatsApp)
		notifProvs[notifications.WhatsApp] = observability.WrapNotificationProvider(p, a.Metrics, a.Tracer)
		notifProvNames = append(notifProvNames, p.Name())
		appSecret := cfg.WhatsAppAppSecret
		verifyTok := cfg.WhatsAppVerifyToken
		if cfg.Mode == config.Dev {
			if appSecret == "" {
				appSecret = "dev-whatsapp-app-secret"
			}
			if verifyTok == "" {
				verifyTok = "dev-whatsapp-verify-token"
			}
		}
		waParser = offline.HMACWhatsAppParser{AppSecret: appSecret, VerifyToken: verifyTok}
	}

	var ussdAdp *ussd.Adapter
	if cfg.USSDSharedSecret != "" && len(cfg.USSDAllowedIPs) > 0 {
		adp, uErr := ussd.New(ussd.Config{
			SharedSecret: cfg.USSDSharedSecret,
			AllowedCIDRs: cfg.USSDAllowedIPs,
		})
		if uErr != nil {
			return fail(uErr)
		}
		ussdAdp = adp
	}

	a.Orch.RegisterNotificationProviders(notifProvNames...)

	ns, err := notificationservice.New(notificationservice.Config{
		Store:            a.Store,
		Box:              box,
		Providers:        notifProvs,
		InboundVerifiers: inboundVers,
		FeatureGiftSMS:   cfg.FeatureGiftSMS,
		HMACRetention:    cfg.NotificationHMACRetention,
		Metrics:          a.Metrics,
		Log:              log,
	})
	if err != nil {
		return fail(err)
	}
	a.Notifications = ns

	var phoneLim ratelimit.Limiter
	if a.redis != nil {
		phoneLim = a.redis.Limiter("offline-phone", 30, 15)
	}
	offSvc, err := offline.New(offline.Config{
		Store:                   a.Store,
		Orch:                    a.Orch,
		Box:                     box,
		SMSProvider:             notifProvs[notifications.SMS],
		WhatsAppProvider:        notifProvs[notifications.WhatsApp],
		USSDAdapter:             ussdAdp,
		WhatsAppParser:          waParser,
		MinAmountMinor:          cfg.Orchestrator.MinAmountMinor,
		MaxAmountMinor:          cfg.Orchestrator.MaxAmountMinor,
		FeeMinor:                cfg.Orchestrator.FeeMinor,
		UnverifiedDailyCapMinor: cfg.OfflineUnverifiedDailyMaxMinor,
		VerifiedDailyCapMinor:   cfg.OfflineVerifiedDailyMaxMinor,
		ReverifyAfter:           cfg.OfflineReverifyAfter,
		PhoneLimiter:            phoneLim,
		Log:                     log,
	})
	if err != nil {
		return fail(err)
	}
	a.Offline = offSvc
	a.Publisher = outbox.NewPublisher(a.Store, outbox.Chain(outbox.LogSink(log), ns.Sink()), log)
	a.RefreshMetrics(ctx)
	return a, nil
}

// Ready reports whether the app can serve traffic: database reachable and
// migrations current.
func (a *App) Ready(ctx context.Context) error {
	if a.pool == nil {
		return nil
	}
	if err := a.pool.Ping(ctx); err != nil {
		return err
	}
	n, err := migrate.Pending(ctx, a.pool, migrations.FS)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d pending migration(s)", n)
	}
	return nil
}

// RefreshMetrics snapshots transaction status counts, open manual reviews and
// outbox lag from the store into the Prometheus registry.
func (a *App) RefreshMetrics(ctx context.Context) {
	if a.Store == nil || a.Metrics == nil {
		return
	}
	snap, err := a.Store.MetricsSnapshot(ctx, time.Now().UTC())
	if err != nil {
		a.Log.Warn("metrics snapshot failed", "err", err)
		return
	}
	byStatus := make(map[string]int64, len(snap.TransactionsByStatus))
	for st, cnt := range snap.TransactionsByStatus {
		byStatus[string(st)] = cnt
	}
	a.Metrics.SetTransactionGauges(byStatus, snap.ManualReviewOpen, snap.OutboxLagSeconds)
}

func (a *App) Handler() http.Handler {
	s := &httpapi.Server{
		O: a.Orch, Auth: a.authn, Identity: a.Identity, Payments: a.payments, Notifications: a.Notifications, Offline: a.Offline,
		Metrics: a.Metrics, Tracer: a.Tracer, Log: a.Log, Ready: a.Ready,
		Limits: httpapi.Limits{
			WritesPerMinute: a.Cfg.RateWritesPerMin, AuthFailuresPerMinute: a.Cfg.RateAuthFailuresPerMin,
			WebhooksPerMinute: a.Cfg.RateWebhooksPerMin, PublicPayPerMinute: a.Cfg.RatePublicPayPerMin,
			PublicLinkPerMinute: a.Cfg.RatePublicLinkPerMin, ClientIPHeader: a.Cfg.ClientIPHeader,
		},
		PayForMeDisabled:   !a.Cfg.FeaturePayForMe,
		SplitMeterDisabled: !a.Cfg.FeatureSplitMeter,
		AutoPowerDisabled:  !a.Cfg.FeatureAutoPower,
		PowerDeskDisabled:  !a.Cfg.FeaturePowerDesk,
		USSDDisabled:       !a.Cfg.FeatureUSSD,
		WhatsAppDisabled:   !a.Cfg.FeatureWhatsApp,
		SupportDisabled:    !a.Cfg.FeatureSupport,
		HouseholdsDisabled: !a.Cfg.FeatureHouseholds,
		PublicBaseURL:      a.Cfg.PublicBaseURL,
		Remote:             a.remoteRaw,
		Dev:                a.Cfg.Mode == config.Dev, DevPay: a.mockPay, DevRemote: a.mockRemote, DevReadings: a.MockReadings,
	}
	if a.redis != nil {
		s.NewLimiter = a.redis.Limiter
	}
	h := s.Routes()
	if a.wrapPublic != nil {
		h = a.wrapPublic(h)
	}
	return h
}

// MetricsHandler serves Prometheus metrics for the internal observability port.
// It is never mounted on Handler().
func (a *App) MetricsHandler() http.Handler {
	return a.Metrics.Handler()
}

func (a *App) shutdownTimeout() time.Duration {
	if a.Cfg.ShutdownTimeout > 0 {
		return a.Cfg.ShutdownTimeout
	}
	return 15 * time.Second
}

func (a *App) metricsRefreshInterval() time.Duration {
	if a.Cfg.MetricsRefreshInterval > 0 {
		return a.Cfg.MetricsRefreshInterval
	}
	return time.Minute
}

// Serve binds the public API listener (and optional internal metrics listener),
// runs background worker/metrics loops, and performs ordered graceful shutdown
// when ctx is cancelled:
//  1. drain in-flight public HTTP requests (stop accepting new connections),
//  2. stop worker loops and wait for all in-flight claims to finish,
//  3. drain the internal metrics HTTP listener,
//  4. flush the OTel tracer and close the database store.
func (a *App) Serve(ctx context.Context) error {
	pubLn, err := net.Listen("tcp", a.Cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", a.Cfg.HTTPAddr, err)
	}
	var metLn net.Listener
	if a.Cfg.MetricsAddr != "" {
		metLn, err = net.Listen("tcp", a.Cfg.MetricsAddr)
		if err != nil {
			_ = pubLn.Close()
			return fmt.Errorf("listen metrics %s: %w", a.Cfg.MetricsAddr, err)
		}
	}
	return a.ServeListeners(ctx, pubLn, metLn)
}

// ServeListeners runs the same lifecycle as Serve on pre-bound listeners.
func (a *App) ServeListeners(ctx context.Context, pubLn, metLn net.Listener) error {
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	drainRootCtx, cancelDrainRoot := context.WithCancel(context.Background())
	defer cancelDrainRoot()

	a.RefreshMetrics(runCtx)

	var bgWG sync.WaitGroup
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		a.loop(runCtx, "metrics-gauges", a.metricsRefreshInterval(), func(ctx context.Context) {
			a.RefreshMetrics(ctx)
		})
	}()

	if a.Cfg.RunWorker {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			a.RunWorkerWithDrain(runCtx, drainRootCtx)
		}()
	}

	var serveErr error
	var errOnce sync.Once
	recordErr := func(err error) {
		errOnce.Do(func() {
			serveErr = err
			stopRun()
		})
	}

	var metSrv *http.Server
	if metLn != nil {
		metSrv = &http.Server{Handler: a.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			a.Log.Info("metrics listening", "addr", metLn.Addr().String())
			if err := metSrv.Serve(metLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("metrics server", "err", err)
				recordErr(err)
			}
		}()
	}

	pubSrv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		a.Log.Info("listening", "addr", pubLn.Addr().String(), "mode", a.Cfg.Mode, "worker", a.Cfg.RunWorker)
		if err := pubSrv.Serve(pubLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Log.Error("http server", "err", err)
			recordErr(err)
		}
	}()

	<-runCtx.Done()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), a.shutdownTimeout())
	defer cancelDrain()
	stopAfter := context.AfterFunc(drainCtx, cancelDrainRoot)
	defer stopAfter()

	// 1. Drain public HTTP server before closing DB or tracer.
	_ = pubSrv.Shutdown(drainCtx)
	// 2. Wait for worker loops and in-flight claims to finish.
	bgWG.Wait()
	// 3. Refresh gauges one final time if drainCtx allows, then close metrics server.
	if metSrv != nil {
		_ = metSrv.Shutdown(drainCtx)
	}
	// 4. Flush OTel tracer and close DB store.
	a.Shutdown(drainCtx)
	return serveErr
}

// ServeWorker runs the standalone worker process (and optional internal
// metrics server) with ordered graceful shutdown.
func (a *App) ServeWorker(ctx context.Context) error {
	var metLn net.Listener
	if a.Cfg.MetricsAddr != "" {
		var err error
		metLn, err = net.Listen("tcp", a.Cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("listen metrics %s: %w", a.Cfg.MetricsAddr, err)
		}
	}

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	drainRootCtx, cancelDrainRoot := context.WithCancel(context.Background())
	defer cancelDrainRoot()

	a.RefreshMetrics(runCtx)

	var bgWG sync.WaitGroup
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		a.loop(runCtx, "metrics-gauges", a.metricsRefreshInterval(), func(ctx context.Context) {
			a.RefreshMetrics(ctx)
		})
	}()

	var serveErr error
	var errOnce sync.Once
	var metSrv *http.Server
	if metLn != nil {
		metSrv = &http.Server{Handler: a.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			a.Log.Info("metrics listening", "addr", metLn.Addr().String())
			if err := metSrv.Serve(metLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("metrics server", "err", err)
				errOnce.Do(func() { serveErr = err; stopRun() })
			}
		}()
	}

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		a.RunWorkerWithDrain(runCtx, drainRootCtx)
	}()

	<-runCtx.Done()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), a.shutdownTimeout())
	defer cancelDrain()
	stopAfter := context.AfterFunc(drainCtx, cancelDrainRoot)
	defer stopAfter()

	bgWG.Wait()
	if metSrv != nil {
		_ = metSrv.Shutdown(drainCtx)
	}
	a.Shutdown(drainCtx)
	return serveErr
}

// RunWorker runs the resume and outbox loops until ctx is cancelled, allowing
// in-flight claims up to ShutdownTimeout to finish.
func (a *App) RunWorker(ctx context.Context) {
	drainRootCtx, cancelDrainRoot := context.WithCancel(context.Background())
	defer cancelDrainRoot()
	stopAfter := context.AfterFunc(ctx, func() {
		time.AfterFunc(a.shutdownTimeout(), cancelDrainRoot)
	})
	defer stopAfter()
	a.RunWorkerWithDrain(ctx, drainRootCtx)
}

// RunWorkerWithDrain stops scheduling new sweeps or new claims once stopCtx is
// cancelled while allowing already-claimed operations to complete on drainCtx.
func (a *App) RunWorkerWithDrain(stopCtx, drainCtx context.Context) {
	if drainCtx == nil {
		drainCtx = stopCtx
	}
	var wg sync.WaitGroup
	start := func(name string, interval time.Duration, fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); a.loop(stopCtx, name, interval, fn) }()
	}
	defer wg.Wait()
	start("outbox", a.Cfg.OutboxInterval, func(ctx context.Context) {
		for ctx.Err() == nil {
			n, err := a.Publisher.PublishBatchWithDrain(ctx, drainCtx)
			if err != nil {
				a.Log.Warn("outbox publish failed", "err", err)
				return
			}
			if n == 0 {
				return
			}
		}
	})
	start("notification-deliveries", a.Cfg.NotificationInterval, func(ctx context.Context) {
		for ctx.Err() == nil {
			n, err := a.Notifications.ProcessBatchWithDrain(ctx, drainCtx, 50)
			if err != nil {
				a.Log.Warn("notification delivery sweep failed", "err", err)
				return
			}
			if n == 0 {
				return
			}
		}
	})
	start("notification-retention", time.Hour, func(ctx context.Context) {
		if ctx.Err() != nil {
			return
		}
		purged, err := a.Notifications.PurgeExpiredHMACs(drainCtx)
		if err != nil {
			a.Log.Warn("notification retention purge failed", "err", err)
			return
		}
		if purged > 0 {
			a.Log.Info("notification destination HMACs purged", "count", purged)
		}
	})
	if !a.Cfg.NotificationCandidatesSince.IsZero() {
		start("notification-candidates", a.Cfg.OutboxInterval, func(ctx context.Context) {
			if ctx.Err() != nil {
				return
			}
			// One bounded batch per tick; no sender or destination is involved.
			n, err := notificationconsumer.ConsumeBatch(drainCtx, a.Store, a.Cfg.NotificationCandidatesSince, 100)
			if err != nil {
				a.Log.Warn("notification candidate collection failed", "err", err)
				return
			}
			if n > 0 {
				a.Log.Info("notification candidates held", "count", n)
			}
		})
	}
	if a.Cfg.FeatureAutoPower {
		apInterval := a.Cfg.AutoPowerInterval
		if apInterval <= 0 {
			apInterval = 30 * time.Second
		}
		start("autopower-scheduler", apInterval, func(ctx context.Context) {
			if ctx.Err() != nil {
				return
			}
			n, err := a.Orch.TickAutoPower(drainCtx)
			if err != nil {
				a.Log.Warn("autopower scheduler tick failed", "err", err)
				return
			}
			if n > 0 {
				a.Log.Info("autopower scheduler tick", "processed", n)
			}
		})
		start("autopower-low-balance", apInterval, func(ctx context.Context) {
			if ctx.Err() != nil {
				return
			}
			n, err := a.Orch.PollLowBalance(drainCtx)
			if err != nil {
				a.Log.Warn("autopower low-balance poll failed", "err", err)
				return
			}
			if n > 0 {
				a.Log.Info("autopower low-balance poll", "polled", n)
			}
		})
	}
	a.loop(stopCtx, "resume", a.Cfg.ResumeInterval, func(ctx context.Context) {
		processed, progressed, err := a.Orch.ResumeDueWithDrain(ctx, drainCtx)
		if err != nil {
			a.Log.Warn("resume sweep failed", "err", err)
			return
		}
		if processed > 0 {
			a.Log.Info("resume sweep", "processed", processed, "progressed", progressed)
		}
	})
}

func (a *App) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	a.Log.Info("worker loop started", "loop", name, "every", every.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						a.Log.Error("worker loop panic", "loop", name, "panic", r)
					}
				}()
				fn(ctx)
			}()
		}
	}
}

// Shutdown flushes exported telemetry and closes persistent resources once.
func (a *App) Shutdown(ctx context.Context) {
	a.closeOnce.Do(func() {
		if a.Tracer != nil {
			if err := a.Tracer.Shutdown(ctx); err != nil {
				a.Log.Warn("tracer shutdown incomplete", "err", err)
			}
		}
		if a.redis != nil {
			a.redis.Close()
		}
		if a.Store != nil {
			a.Store.Close()
		}
	})
}

func (a *App) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout())
	defer cancel()
	a.Shutdown(ctx)
}
