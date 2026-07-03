package server

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"golang.org/x/crypto/bcrypt"
)

// TestMain lowers the bcrypt cost for the whole package. Without this,
// the suite's many bootstrapFirstUser calls run bcrypt at the production
// cost under -race (~3s per call), pushing cumulative race-step time past
// the CI 30m timeout. See BUG-1371.
//
// It also tears down storetest's process-wide template DB (IDEA-1914)
// after the suite finishes, so the lazily-built template file doesn't
// linger past this binary's lifetime.
func TestMain(m *testing.M) {
	store.SetBcryptCostForTesting(bcrypt.MinCost)
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}
