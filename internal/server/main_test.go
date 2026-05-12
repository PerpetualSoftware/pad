package server

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// TestMain lowers the bcrypt cost for the whole package. Without this,
// the suite's many bootstrapFirstUser calls run bcrypt at the production
// cost under -race (~3s per call), pushing cumulative race-step time past
// the CI 30m timeout. See BUG-1371.
func TestMain(m *testing.M) {
	store.SetBcryptCostForTesting(bcrypt.MinCost)
	os.Exit(m.Run())
}
