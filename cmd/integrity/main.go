// Command integrity checks a restored database (trial balance, gauges, 24h window).
//
//	DATABASE_URL=postgres://... go run ./cmd/integrity
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/integrity"
	"github.com/teamthy/meterly/internal/store/pgstore"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	st := pgstore.New(pool, "SANDBOX")
	rep, err := integrity.Check(ctx, st, time.Now().UTC())
	if err != nil {
		log.Error("integrity", "err", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if !rep.OK {
		os.Exit(1)
	}
}
