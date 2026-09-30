//go:build darwin || linux

package materialize

import (
	"os"
	"os/exec"
	"testing"
)

// The parser alone: fixed strings, nothing executed, so it runs everywhere
// including the Nix build sandbox.
func TestParsePSRSS(t *testing.T) {
	if n, err := parsePSRSS([]byte("  123456\n")); err != nil || n != 123456<<10 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "\n", "abc", "12 34"} {
		if _, err := parsePSRSS([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestPSRSSLive runs the real `ps` fallback against this process. It skips
// when there is no `ps` to run (the Nix build sandbox has none), which is
// acceptable: `ps` is only the macOS watchdog's FALLBACK, the production path
// is proc_info. PAD_MATERIALIZE_TEST_NO_SKIP still turns the skip into a
// failure where CI sets it.
func TestPSRSSLive(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		skipCapTest(t, "no `ps` on PATH (the Nix build sandbox has none): "+err.Error())
	}
	n, err := psRSS(os.Getpid())
	if err != nil || n == 0 {
		t.Fatalf("ps on self: %d %v", n, err)
	}
}

// BenchmarkPSRSS is the cost of one poll by the macOS watchdog's ps fallback.
func BenchmarkPSRSS(b *testing.B) {
	if _, err := exec.LookPath("ps"); err != nil {
		b.Skip("no `ps` on PATH: " + err.Error())
	}
	pid := os.Getpid()
	for b.Loop() {
		if _, err := psRSS(pid); err != nil {
			b.Fatal(err)
		}
	}
}
