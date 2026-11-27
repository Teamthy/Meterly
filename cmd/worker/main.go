// Command worker runs the background loops: the resume/reconciliation
// sweep, the outbox publisher and notification delivery loops. Run any
// number of replicas; claims use SKIP LOCKED so they never process the same
// row at once.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/teamthy/meterly/internal/app"
	"github.com/teamthy/meterly/internal/platform/config"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if cfg.DatabaseURL == "" {
		log.Error("cmd/worker needs DATABASE_URL; with the in-memory store run the worker inside cmd/api (RUN_WORKER=true)")
		os.Exit(1)
	}
	if cfg.UsesMocks() {
		// Mock providers keep their state in the process that called them, so
		// a separate worker would see none of the API's simulated payments or
		// vends and could re-send requests. Fail instead of misbehaving.
		log.Error("cmd/worker cannot run with mock providers; in dev run the worker inside cmd/api (RUN_WORKER=true)")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	a, err := app.Build(bctx, cfg, log)
	cancel()
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer a.Close()
	log.Info("worker started", "mode", cfg.Mode)
	if err := a.ServeWorker(ctx); err != nil {
		log.Error("worker failed", "err", err)
		os.Exit(1)
	}
}
