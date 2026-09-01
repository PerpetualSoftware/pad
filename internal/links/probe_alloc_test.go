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
			_ = RewriteBracketAt(body, 0, "A", "A", tc.newTitle, "")
		}
		runtime.ReadMemStats(&after)
		per := (after.TotalAlloc - before.TotalAlloc) / n
		t.Logf("%-12s per call=%-9d (=%.2fx the %d-byte body)",
			tc.name, per, float64(per)/float64(len(body)), len(body))
	}
}

// TestProjectRewrittenLenDoesNotAllocatePerBracket pins codex R5's finding.
//
// ProjectRewrittenLen runs once per rewrite BEFORE the cascade's cap can fire.
// An earlier bracketRewriteAt composed `collSlug + "/" + newTitle` on every
// call — before the match check, so even the leave-alone exit paid — which made
// the projection allocate newTitle bytes PER BRACKET while measuring something
// it was about to refuse.
//
// It is not a micro-optimisation: BUG-2831 established that item titles carry
// no validation bound, so a multi-megabyte newTitle is admissible, and a
// bracket-dense body then projects gigabytes of immediately-discarded
// allocation ahead of the refusal that exists to prevent it.
//
// The ceiling is expressed per bracket and in units of newTitle, so it stays
// meaningful if the sizes move. Measured after the fix: ~0.00x.
func TestProjectRewrittenLenDoesNotAllocatePerBracket(t *testing.T) {
	const brackets = 4096
	newTitle := strings.Repeat("N", 1<<20) // 1 MiB, no validation bound exists
	content := strings.Repeat("[[A]]", brackets)

	rewrites := make([]BracketRewrite, 0, brackets)
	for i := 0; i < brackets; i++ {
		rewrites = append(rewrites, BracketRewrite{Position: i * len("[[A]]"), TargetTitle: "A"})
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	length, applied := ProjectRewrittenLen(content, rewrites, NewTitleEscaper("A", newTitle, "tasks"))
	runtime.ReadMemStats(&after)

	if applied != brackets {
		t.Fatalf("applied = %d, want %d — the projection did not exercise every bracket", applied, brackets)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	perBracket := float64(allocated) / float64(brackets) / float64(len(newTitle))

	// One newTitle per bracket is the defect; a tenth of one is already far
	// below anything the old shape could achieve.
	t.Logf("projected len=%d applied=%d; allocated %d bytes = %.4f x newTitle per bracket",
		length, applied, allocated, perBracket)
	if perBracket > 0.1 {
		t.Errorf("projection allocated %.4f x newTitle per bracket (%d bytes total) — it is "+
			"materialising the replacement it was only asked to measure", perBracket, allocated)
	}
}
