package store

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestMain lowers the bcrypt cost for the whole package. See BUG-1371
// and internal/server/main_test.go for context — the same reasoning
// applies to internal/store tests that call CreateUser directly.
func TestMain(m *testing.M) {
	SetBcryptCostForTesting(bcrypt.MinCost)
	os.Exit(m.Run())
}
