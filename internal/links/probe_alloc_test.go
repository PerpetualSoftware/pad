package links

import (
	"runtime"
	"strings"
	"testing"
)

// Isolates RewriteBracketAt's own allocation per call, away from the cascade
// loop, to establish where the measured ~2x-body-per-bracket constant lives.
func TestProbeRewriteBracketAtAllocation(t *testing.T) {
	const c = 256 << 10
	body := strings.Repeat("[[A]]", c/5)

	for _, tc := range []struct{ name, newTitle string }{
		{"same-length", "B"},
		{"longer", "BBBBBBBBBBBBBBBB"},
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		const n = 200
		for i := 0; i < n; i++ {
			_ = RewriteBracketAt(body, 0, "A", tc.newTitle, "")
		}
		runtime.ReadMemStats(&after)
		per := (after.TotalAlloc - before.TotalAlloc) / n
		t.Logf("%-12s per call=%-9d (=%.2fx the %d-byte body)",
			tc.name, per, float64(per)/float64(len(body)), len(body))
	}
}
