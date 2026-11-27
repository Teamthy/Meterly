package migrate_test

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/teamthy/meterly/internal/platform/migrate"
	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
	"github.com/teamthy/meterly/migrations"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}

func TestUpIsIdempotentAndDetectsEdits(t *testing.T) {
	pool := pgtest.NewPool(t) // already migrated from the embedded files
	ctx := context.Background()
	ran, err := migrate.Up(ctx, pool, migrations.FS)
	if err != nil || len(ran) != 0 {
		t.Fatalf("re-run applied %v, err %v", ran, err)
	}
	if n, err := migrate.Pending(ctx, pool, migrations.FS); err != nil || n != 0 {
		t.Fatalf("pending %d err %v", n, err)
	}

	edited := fstest.MapFS{}
	entries, _ := migrations.FS.ReadDir(".")
	for _, e := range entries {
		b, _ := migrations.FS.ReadFile(e.Name())
		edited[e.Name()] = &fstest.MapFile{Data: b}
	}
	edited["0001_init.sql"] = &fstest.MapFile{Data: append(edited["0001_init.sql"].Data, []byte("\n-- edited\n")...)}
	if _, err := migrate.Up(ctx, pool, edited); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("want checksum error, got %v", err)
	}

	edited["0099_new.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE tmp_probe (id int); SELECT 1/0;")}
	delete(edited, "0001_init.sql")
	edited["0001_init.sql"] = &fstest.MapFile{Data: mustRead(t, "0001_init.sql")}
	if _, err := migrate.Up(ctx, pool, edited); err == nil {
		t.Fatal("want failing migration error")
	}
	var exists bool
	_ = pool.QueryRow(ctx, `SELECT to_regclass('tmp_probe') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("a failed migration must roll back completely")
	}
}

func mustRead(t *testing.T, name string) []byte {
	b, err := migrations.FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBadFileNames(t *testing.T) {
	pool := pgtest.NewPool(t)
	if _, err := migrate.Up(context.Background(), pool, fstest.MapFS{"init.sql": {Data: []byte("SELECT 1")}}); err == nil {
		t.Fatal("want naming error")
	}
}

func TestEmbeddedMigrationsIncludeP2P4(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if len(names) < 27 {
		t.Fatalf("want at least 27 migrations, got %d: %v", len(names), names)
	}
	if names[0] != "0001_init.sql" {
		t.Fatalf("first file %s", names[0])
	}
	prev := ""
	for _, n := range names {
		v, _, ok := strings.Cut(n, "_")
		if !ok || len(v) != 4 {
			t.Fatalf("want NNNN_name.sql, got %s", n)
		}
		if prev != "" && v <= prev {
			t.Fatalf("versions not strictly increasing: %s then %s", prev, n)
		}
		prev = v
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"0022_reconciliation.sql", "0023_risk.sql", "0024_pilot.sql", "0025_privacy.sql", "0026_data_export.sql", "0027_account_erasure.sql"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("embedded FS missing %s: %v", want, names)
		}
	}
}

// TestP2P4MigrationChain applies the embedded chain (pgtest already ran Up)
// and smoke-queries every table/account introduced in 0022–0024.
func TestP2P4MigrationChain(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		pool := pgtest.NewPool(t)
		ctx := context.Background()
		if n, err := migrate.Pending(ctx, pool, migrations.FS); err != nil || n != 0 {
			t.Fatalf("pending %d err %v", n, err)
		}
		var versions []string
		rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			versions = append(versions, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(versions, " ")
		for _, want := range []string{"0022_reconciliation.sql", "0023_risk.sql", "0024_pilot.sql", "0025_privacy.sql", "0026_data_export.sql"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("schema_migrations missing %s: %v", want, versions)
			}
		}
		i22, i23, i24, i25, i26 := indexOf(versions, "0022_reconciliation.sql"), indexOf(versions, "0023_risk.sql"), indexOf(versions, "0024_pilot.sql"), indexOf(versions, "0025_privacy.sql"), indexOf(versions, "0026_data_export.sql")
		if i22 < 0 || i23 < 0 || i24 < 0 || i25 < 0 || i26 < 0 || !(i22 < i23 && i23 < i24 && i24 < i25 && i25 < i26) {
			t.Fatalf("0022–0026 out of order: %v", versions)
		}

		tables := []string{
			"reconciliation_runs", "reconciliation_exceptions", "reconciliation_adjustments",
			"risk_rules", "risk_observations", "risk_decisions", "risk_denylist", "risk_link_pauses", "disputes",
			"pilot_allowlist",
			"consents", "legal_holds", "retention_runs",
			"data_exports",
		}
		for _, tbl := range tables {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
				t.Fatalf("smoke %s: %v", tbl, err)
			}
		}
		accounts := []string{
			"CASH_AT_BANK", "PSP_FEES_EXPENSE", "VENDOR_WALLET_PREFUND", "VENDOR_COMMISSION_INCOME",
			"DISPUTE_RESERVE", "DISPUTE_LOSS",
		}
		for _, code := range accounts {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts WHERE account_code = $1`, code).Scan(&n); err != nil || n != 1 {
				t.Fatalf("ledger account %s count=%d err=%v", code, n, err)
			}
		}
	})
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
