package store_test

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TestMain lowers the bcrypt cost for the whole package. See BUG-1371
// and internal/server/main_test.go for context — the same reasoning
// applies to internal/store tests that call CreateUser directly.
//
// It also tears down the process-wide template databases after the suite
// finishes: testStoreSQLite's template FILE (IDEA-1914, store_test.go) and
// testStorePostgres's template DATABASE (TASK-2900, same file), plus the
// separate templates storetest builds for the external tests that call
// storetest.NewSQLite / NewPostgres (TASK-4424). The Postgres teardowns are
// no-ops when the suite ran on SQLite, since no template was built.
//
// TestMain lives in the external test package because storetest imports
// store, so the internal package cannot import storetest. The internal
// teardowns are reached through export_test.go.
func TestMain(m *testing.M) {
	store.SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	store.RemoveSQLiteTemplate()
	store.RemovePostgresTemplate()
	storetest.Cleanup()
	os.Exit(code)
}
