// Command api serves the Meterly HTTP API and internal Prometheus metrics
// endpoint. With RUN_WORKER=true (the default in MODE=dev) it also runs the
// resume, outbox and notification loops in-process.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	a, err := app.Build(bctx, cfg, log)
	cancel()
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer a.Close()

	if err := a.Serve(ctx); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
