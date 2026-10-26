package cli

import (
	"os"
	"runtime"
	"testing"
)

// pidnsHelperEnv switches the test binary into TestPIDNamespaceHelper's
// reader mode: it is set only on the child that bwrap starts.
const pidnsHelperEnv = "PAD_TEST_PIDNS_HELPER"

// TestMain pins the reader's PID namespace to the init namespace for the
// whole package (BUG-4090). Most tests here build owners by hand with no
// pid_ns, and from a non-init namespace a dead pid on such a row reads
// unknown, not dead — so without the pin the same suite would give other
// answers when `go test` runs in a sandbox (nix/package.nix's checkPhase
// runs in the Nix build sandbox's PID namespace). The namespace cases
// override the seam themselves. The bwrap helper keeps the real reader:
// it exists to be in a foreign namespace.
func TestMain(m *testing.M) {
	if runtime.GOOS == "linux" && os.Getenv(pidnsHelperEnv) != "1" {
		pidNamespaceFn = func() (uint64, error) { return initPIDNamespace, nil }
	}
	os.Exit(m.Run())
}
