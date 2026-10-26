//go:build linux

package main

import (
	"os"
	"syscall"
	"testing"
)

// testPIDNamespace is this test process's PID namespace inode, for stamping
// hand-written registry rows as same-namespace rows (BUG-4090). Without the
// stamp, a dead pid on such a row reads unknown, not dead, whenever `go test`
// runs outside the init namespace — as the Nix build's checkPhase does. With
// no procfs at all the namespace cannot be read, and the test cannot say
// which verdict is right there, so it skips.
func testPIDNamespace(t *testing.T) uint64 {
	t.Helper()
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("no /proc/self/ns/pid (%v): the namespace verdicts cannot be checked here", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		t.Skip("no pid namespace identity here")
	}
	return st.Ino
}

// inInitPIDNamespace reports whether this test runs in the host's PID
// namespace (PROC_PID_INIT_INO), where an unstamped row is judged by the
// pre-BUG-4090 rule.
func inInitPIDNamespace(t *testing.T) bool {
	t.Helper()
	return testPIDNamespace(t) == 0xEFFFFFFC
}
