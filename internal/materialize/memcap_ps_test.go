//go:build darwin || linux

package materialize

import (
	"os"
	"testing"
)

func TestParsePSRSS(t *testing.T) {
	if n, err := parsePSRSS([]byte("  123456\n")); err != nil || n != 123456<<10 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "\n", "abc", "12 34"} {
		if _, err := parsePSRSS([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	n, err := psRSS(os.Getpid())
	if err != nil || n == 0 {
		t.Fatalf("ps on self: %d %v", n, err)
	}
}

// BenchmarkPSRSS is the cost of one poll by the macOS watchdog's ps fallback.
func BenchmarkPSRSS(b *testing.B) {
	pid := os.Getpid()
	for b.Loop() {
		if _, err := psRSS(pid); err != nil {
			b.Fatal(err)
		}
	}
}
