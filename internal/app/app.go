// Package app wires configuration, storage, providers, the orchestrator,
// the HTTP API and the background loops. cmd/api and cmd/worker share it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/auth"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/notificationconsumer"
	"github.com/teamthy/meterly/internal/notifications"
	"github.com/teamthy/meterly/internal/notifications/africastalking"
	"github.com/teamthy/meterly/internal/notifications/resend"
	"github.com/teamthy/meterly/internal/notifications/whatsapp"
	"github.com/teamthy/meterly/internal/notificationservice"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/outbox"
	"github.com/teamthy/meterly/internal/platform/config"
	"github.com/teamthy/meterly/internal/platform/migrate"
	"github.com/teamthy/meterly/internal/providers"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/providers/paystack"
	"github.com/teamthy/meterly/internal/registry"
	"github.com/teamthy/meterly/internal/secure"
	"github.com/teamthy/meterly/internal/store"
	"github.com/teamthy/meterly/internal/store/memstore"
	"github.com/teamthy/meterly/internal/store/pgstore"
	"github.com/teamthy/meterly/migrations"
)

type App struct {
	Cfg           config.Config
	Log           *slog.Logger
	Store         store.Store
	Orch          *orchestrator.Orchestrator
	Publisher     *outbox.Publisher
	Notifications *notificationservice.Service
	pool          *pgxpool.Pool

	// Set only when the corresponding mock is in use (dev simulation).
	mockPay      *mock.Payments
	mockRemote   *mock.RemoteCredit
	MockSMS      *notifications.MockSender
	MockEmail    *notifications.MockSender
	MockWhatsApp *notifications.MockSender
	payments     providers.PaymentProvider
	authn        auth.Authenticator
}

func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, Log: log}

	encKey, hmacKey := cfg.TokenEncKey, cfg.MeterHMACKey
	if encKey == nil || hmacKey == nil {
		if cfg.Mode != config.Dev {
			return nil, errors.New("encryption keys are required outside dev")
		}
		log.Warn("using ephemeral encryption keys; stored tokens and meter numbers become unreadable after restart")
		encKey, hmacKey = secure.RandomKey(32), secure.RandomKey(32)
	}
	box, err := secure.NewBox(encKey, hmacKey, cfg.KeyVersion)
	if err != nil {
		return nil, err
	}

	if cfg.DatabaseURL == "" {
		log.Warn("using in-memory store; all data is lost on exit")
		a.Store = memstore.New()
	} else {
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("connect database: %w", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("ping database: %w", err)
		}
		a.pool = pool
		a.Store = pgstore.New(pool, cfg.ProviderEnvironment)
		if n, err := migrate.Pending(ctx, pool, migrations.FS); err != nil || n > 0 {
			pool.Close()
			if err == nil {
				err = fmt.Errorf("%d pending migration(s); run cmd/migrate first", n)
			}
			return nil, err
		}
	}

	switch cfg.PaymentProvider {
	case "paystack":
		ps, err := paystack.New(paystack.Config{SecretKey: cfg.PaystackSecretKey, BaseURL: cfg.PaystackBaseURL, CallbackURL: cfg.PaystackCallbackURL})
		if err != nil {
			return nil, err
		}
		a.payments = ps
	default:
		a.mockPay = mock.NewPayments()
		a.payments = a.mockPay
	}
	// Vending and AMI adapters are still mocks (config rejects anything else).
	vend := mock.NewVending()
	var remote providers.RemoteCreditProvider
	if cfg.RemoteProvider == "mock" {
		a.mockRemote = mock.NewRemoteCredit()
		remote = a.mockRemote
	}

	if cfg.AuthMode == "oidc" {
		o, err := auth.NewOIDC(auth.OIDCConfig{
			Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience, JWKSURL: cfg.OIDCJWKSURL,
			RolesClaim: cfg.OIDCRolesClaim, MFAValues: cfg.OIDCMFAValues, Log: log,
		})
		if err != nil {
			return nil, err
		}
		a.authn = o
	} else {
		log.Warn("using dev authentication: the bearer token is the user; operator:/admin: prefixes grant staff roles")
		a.authn = auth.DevAuth{}
	}

	reg := registry.New(a.Store, box, vend, remote)
	a.Orch = orchestrator.New(cfg.Orchestrator, a.Store, reg, box, a.payments, vend, remote, log)

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
			return nil, err
		}
		notifProvs[notifications.SMS] = at
		inboundVers[at.Name()] = at
		notifProvNames = append(notifProvNames, at.Name())
	case "mock":
		ms, err := notifications.NewMockSender()
		if err != nil {
			return nil, err
		}
		a.MockSMS = ms
		p := notifications.WrapSender("mocksms", notifications.SMS, a.MockSMS)
		notifProvs[notifications.SMS] = p
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
			return nil, err
		}
		notifProvs[notifications.Email] = re
		inboundVers[re.Name()] = re
		notifProvNames = append(notifProvNames, re.Name())
	case "mock":
		me, err := notifications.NewMockSender()
		if err != nil {
			return nil, err
		}
		a.MockEmail = me
		p := notifications.WrapSender("mockemail", notifications.Email, a.MockEmail)
		notifProvs[notifications.Email] = p
		notifProvNames = append(notifProvNames, p.Name())
	}

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
			return nil, err
		}
		notifProvs[notifications.WhatsApp] = wa
		inboundVers[wa.Name()] = wa
		notifProvNames = append(notifProvNames, wa.Name())
	case "mock":
		mw, err := notifications.NewMockSender()
		if err != nil {
			return nil, err
		}
		a.MockWhatsApp = mw
		p := notifications.WrapSender("mockwhatsapp", notifications.WhatsApp, a.MockWhatsApp)
		notifProvs[notifications.WhatsApp] = p
		notifProvNames = append(notifProvNames, p.Name())
	}

	a.Orch.RegisterNotificationProviders(notifProvNames...)

	ns, err := notificationservice.New(notificationservice.Config{
		Store:            a.Store,
		Box:              box,
		Providers:        notifProvs,
		InboundVerifiers: inboundVers,
		FeatureGiftSMS:   cfg.FeatureGiftSMS,
		HMACRetention:    cfg.NotificationHMACRetention,
		Log:              log,
	})
	if err != nil {
		return nil, err
	}
	a.Notifications = ns
	a.Publisher = outbox.NewPublisher(a.Store, outbox.Chain(outbox.LogSink(log), ns.Sink()), log)
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

func (a *App) Handler() http.Handler {
	s := &httpapi.Server{
		O: a.Orch, Auth: a.authn, Payments: a.payments, Notifications: a.Notifications, Log: a.Log, Ready: a.Ready,
		Limits: httpapi.Limits{
			WritesPerMinute: a.Cfg.RateWritesPerMin, AuthFailuresPerMinute: a.Cfg.RateAuthFailuresPerMin,
			WebhooksPerMinute: a.Cfg.RateWebhooksPerMin, ClientIPHeader: a.Cfg.ClientIPHeader,
		},
		Dev: a.Cfg.Mode == config.Dev, DevPay: a.mockPay, DevRemote: a.mockRemote,
	}
	return s.Routes()
}

// RunWorker runs the resume and outbox loops until ctx is cancelled.
func (a *App) RunWorker(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(name string, interval time.Duration, fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); a.loop(ctx, name, interval, fn) }()
	}
	defer wg.Wait()
	start("outbox", a.Cfg.OutboxInterval, func(ctx context.Context) {
		for {
			n, err := a.Publisher.PublishBatch(ctx)
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
		for {
			n, err := a.Notifications.ProcessBatch(ctx, 50)
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
		purged, err := a.Notifications.PurgeExpiredHMACs(ctx)
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
			// One bounded batch per tick; no sender or destination is involved.
			n, err := notificationconsumer.ConsumeBatch(ctx, a.Store, a.Cfg.NotificationCandidatesSince, 100)
			if err != nil {
				a.Log.Warn("notification candidate collection failed", "err", err)
				return
			}
			if n > 0 {
				a.Log.Info("notification candidates held", "count", n)
			}
		})
	}
	a.loop(ctx, "resume", a.Cfg.ResumeInterval, func(ctx context.Context) {
		processed, progressed, err := a.Orch.ResumeDue(ctx)
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

func (a *App) Close() { a.Store.Close() }
