package mcp

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TestMain tears down storetest's process-wide template DB (IDEA-1914)
// after the suite finishes, so the lazily-built template file doesn't
// linger past this binary's lifetime.
func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}
