package migrate_test

import (
	"context"
	"os"
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
