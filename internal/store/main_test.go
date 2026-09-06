package store

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestMain lowers the bcrypt cost for the whole package. See BUG-1371
// and internal/server/main_test.go for context — the same reasoning
// applies to internal/store tests that call CreateUser directly.
//
// It also tears down the process-wide template databases after the suite
// finishes: testStoreSQLite's template FILE (IDEA-1914, store_test.go) and
// testStorePostgres's template DATABASE (TASK-2900, same file). The Postgres
// teardown is a no-op when the suite ran on SQLite, since no template was
// built.
func TestMain(m *testing.M) {
	SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	removeSQLiteTemplate()
	removePostgresTemplate()
	os.Exit(code)
}
