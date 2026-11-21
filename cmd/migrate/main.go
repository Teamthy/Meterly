// Command migrate applies pending database migrations.
//
//	DATABASE_URL=postgres://... go run ./cmd/migrate
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/platform/migrate"
	"github.com/teamthy/meterly/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	ran, err := migrate.Up(ctx, pool, migrations.FS)
	for _, n := range ran {
		log.Info("applied", "migration", n)
	}
	if err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	log.Info("database is up to date", "applied", len(ran))
}
