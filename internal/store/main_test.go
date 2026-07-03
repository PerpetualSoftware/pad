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
// It also tears down testStoreSQLite's process-wide template DB
// (IDEA-1914, store_test.go) after the suite finishes.
func TestMain(m *testing.M) {
	SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	removeSQLiteTemplate()
	os.Exit(code)
}
