// Command api runs the Meterly HTTP API.
//
// This build wires in-memory stores and mock providers so the full purchase
// flow can be exercised locally. Production requires PostgreSQL-backed
// stores, real provider adapters, OIDC auth and keys from a secrets manager.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/teamthy/meterly/internal/events"
	"github.com/teamthy/meterly/internal/httpapi"
	"github.com/teamthy/meterly/internal/idempotency"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/orchestrator"
	"github.com/teamthy/meterly/internal/providers/mock"
	"github.com/teamthy/meterly/internal/secure"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if os.Getenv("MODE") != "dev" {
		log.Error("only MODE=dev is supported in this build (mock providers, dev auth, in-memory storage)")
		os.Exit(1)
	}

	box, err := secure.NewBox(key(log, "TOKEN_ENC_KEY", 32), key(log, "METER_HMAC_KEY", 32), "dev-v1")
	if err != nil {
		log.Error("crypto setup", "err", err)
		os.Exit(1)
	}

	pay, vend, remote := mock.NewPayments(), mock.NewVending(), mock.NewRemoteCredit()
	reg := meters.NewRegistry(box, vend, remote)
	orc := orchestrator.New(orchestrator.DefaultConfig(), reg, tx.NewStore(), ledger.New(), events.NewOutbox(), box, pay, vend, remote, log)

	srv := &httpapi.Server{
		O: orc, Auth: httpapi.DevAuth{}, Idem: idempotency.NewStore(24 * time.Hour), Payments: pay, Log: log,
		Dev: true, DevPay: pay, DevRemote: remote,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Reconciliation worker: resolves UNKNOWN / ACCEPTED states.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := orc.ReconcileAll(ctx); n > 0 {
					log.Info("reconciled", "count", n)
				}
			}
		}
	}()

	addr := ":" + envOr("PORT", "8080")
	hs := &http.Server{Addr: addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", "addr", addr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutdown)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// key reads a hex key from env, or generates an ephemeral one in dev.
func key(log *slog.Logger, name string, n int) []byte {
	if v := os.Getenv(name); v != "" {
		b, err := hex.DecodeString(v)
		if err == nil && len(b) == n {
			return b
		}
		log.Error("invalid key; expected hex", "name", name, "bytes", n)
		os.Exit(1)
	}
	log.Warn("generating ephemeral key; tokens will be unreadable after restart", "name", name)
	return secure.RandomKey(n)
}
