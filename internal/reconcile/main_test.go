package reconcile_test

import (
	"os"
	"testing"

	"github.com/teamthy/meterly/internal/store/pgstore/pgtest"
)

// TestMain drops the migrated template database pgtest builds, so test runs
// leave no meterly_tmpl_* databases behind.
func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.DropTemplate()
	os.Exit(code)
}
