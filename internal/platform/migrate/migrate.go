// Package migrate applies the embedded SQL migrations in order. Each file
// runs in its own transaction together with its schema_migrations row, a
// session advisory lock serialises concurrent runners, and a checksum
// mismatch on an applied file is a hard error (never edit an applied
// migration; add a new one).
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const lockID = 7_412_200_901 // arbitrary, stable

type file struct {
	version, name, sql, checksum string
}

func load(fsys fs.FS) ([]file, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []file
	seen := map[string]bool{}
	for _, n := range names {
		v, _, ok := strings.Cut(n, "_")
		if !ok || v == "" {
			return nil, fmt.Errorf("migrate: %s must be named <version>_<name>.sql", n)
		}
		if seen[v] {
			return nil, fmt.Errorf("migrate: duplicate version %s", v)
		}
		seen[v] = true
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, file{version: v, name: n, sql: string(b), checksum: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

const ensureTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	checksum   TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

func applied(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var v, c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		m[v] = c
	}
	return m, rows.Err()
}

// Up applies all pending migrations and returns the names applied.
func Up(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	files, err := load(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID) //nolint:errcheck
	if _, err := conn.Exec(ctx, ensureTable); err != nil {
		return nil, err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, f := range files {
		if sum, ok := done[f.version]; ok {
			if sum != f.checksum {
				return ran, fmt.Errorf("migrate: %s was modified after being applied", f.name)
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(t pgx.Tx) error {
			if _, err := t.Exec(ctx, f.sql); err != nil {
				return fmt.Errorf("migrate: %s: %w", f.name, err)
			}
			_, err := t.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, f.version, f.name, f.checksum)
			return err
		})
		if err != nil {
			return ran, err
		}
		ran = append(ran, f.name)
	}
	return ran, nil
}

// Pending returns how many embedded migrations are not yet applied, or an
// error if an applied migration's checksum no longer matches.
func Pending(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) (int, error) {
	files, err := load(fsys)
	if err != nil {
		return 0, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return len(files), nil
	}
	done, err := applied(ctx, pool)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		sum, ok := done[f.version]
		if !ok {
			n++
		} else if sum != f.checksum {
			return 0, fmt.Errorf("migrate: %s was modified after being applied", f.name)
		}
	}
	return n, nil
}
