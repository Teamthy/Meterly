// Package pgtest gives each test its own migrated PostgreSQL database.
// Tests are skipped unless DATABASE_URL points at a server where the user
// may create databases. A migrated template database is built once per
// test binary; each test clones it (fast) and drops it on cleanup.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/platform/migrate"
	"github.com/teamthy/meterly/migrations"
)

var (
	once     sync.Once
	template string
	initErr  error
)

func withDB(url, db string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Database = db
	return cfg, nil
}

func admin(ctx context.Context, url string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

func buildTemplate(ctx context.Context, url string) (string, error) {
	name := "meterly_tmpl_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	conn, err := admin(ctx, url)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", err
	}
	cfg, err := withDB(url, name)
	if err != nil {
		return "", err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	if _, err := migrate.Up(ctx, pool, migrations.FS); err != nil {
		return "", err
	}
	return name, nil
}

// NewPool returns a pool on a fresh, fully migrated database.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	once.Do(func() { template, initErr = buildTemplate(ctx, url) })
	if initErr != nil {
		t.Fatalf("pgtest: build template: %v", initErr)
	}
	name := "meterly_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	conn, err := admin(ctx, url)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, template))
	conn.Close(ctx)
	if err != nil {
		t.Fatalf("pgtest: create db: %v", err)
	}
	cfg, err := withDB(url, name)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := admin(ctx, url); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(ctx)
		}
	})
	return pool
}

// DropTemplate removes the template database; call from TestMain after
// m.Run() to avoid leaving it behind.
func DropTemplate() {
	url := os.Getenv("DATABASE_URL")
	if url == "" || template == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if c, err := admin(ctx, url); err == nil {
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+template+" WITH (FORCE)")
		c.Close(ctx)
	}
}
