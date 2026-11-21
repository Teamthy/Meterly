// Command api serves the Meterly HTTP API. With RUN_WORKER=true (the
// default in MODE=dev) it also runs the resume and outbox loops in-process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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

	if cfg.RunWorker {
		go a.RunWorker(ctx)
	}

	hs := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "mode", cfg.Mode, "worker", cfg.RunWorker)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = hs.Shutdown(sctx)
}
