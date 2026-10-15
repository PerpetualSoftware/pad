package diff

import (
	"math/rand"
	"strings"
	"testing"
)

// BUG-3839: LineCounts, FormatDiffSummary and Unified are reached by text
// that may not be valid UTF-8 and are not guarded the way ReversePatch is. A
// 200k-case fuzz (seed 1, the generator below) found no panic in them while
// it found go-diff's PatchApply panicking at about 4%. This pins that
// measurement: the panic shapes as seeds, then the same generator, with an
// instrument showing the generator does reach the class.

var bug3839Pieces = []string{"\xff", "\x80", "\xc3", "\xe2\x82", "\xf0\x9f\x98", "\xed\xa0\x80", "é", "😀", "\n", "a", "b", " "}

// bug3839Pairs yields the seed shapes, then n generated pairs.
func bug3839Pairs(n int) [][2]string {
	pairs := [][2]string{
		{"b\xe2\x82b\xff", "b"},
		{"\xff", ""},
		{"", "\x80"},
		{"\xe2\x82", "\xe2\x82\xac"},
		{"\xf0\x9f\x98", "\xf0\x9f\x98\x80"},
		{"\xed\xa0\x80", "a\xed\xa0\x80b"},
		{"x\n\xff\ny\n", "x\n\xc3\ny\n"},
	}
	for _, tc := range bug3839Cases {
		pairs = append(pairs, [2]string{tc.old, tc.new})
	}
	r := rand.New(rand.NewSource(1))
	gen := func() string {
		var b strings.Builder
		for i, k := 0, r.Intn(12); i < k; i++ {
			b.WriteString(bug3839Pieces[r.Intn(len(bug3839Pieces))])
		}
		return b.String()
	}
	for i := 0; i < n; i++ {
		o, nw := gen(), gen()
		if r.Intn(2) == 0 {
			nw = o[:r.Intn(len(o)+1)] + gen() + o[r.Intn(len(o)+1):]
		}
		pairs = append(pairs, [2]string{o, nw})
	}
	return pairs
}

func bug3839Panics(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
}

func TestBUG3839_CountersAndUnifiedNeverPanicOnInvalidUTF8(t *testing.T) {
	pairs := bug3839Pairs(20000)

	// Instrument: the same inputs reach go-diff's panic. PatchApply is what
	// ApplyPatch wrapped before it refused invalid text.
	reached := 0
	for _, p := range pairs {
		patches, err := dmp.PatchFromText("@@ -1,3 +1,4 @@\n a" + p[1] + "\n+b\n")
		if err != nil {
			continue
		}
		if bug3839Panics(func() { dmp.PatchApply(patches, p[0]) }) {
			reached++
		}
	}
	t.Logf("instrument: %d of %d inputs panic go-diff's PatchApply", reached, len(pairs))
	if reached == 0 {
		t.Fatal("instrument: no input reached go-diff's PatchApply panic, so a pass below shows nothing")
	}

	for _, p := range pairs {
		for name, f := range map[string]func(){
			"LineCounts":        func() { LineCounts(p[0], p[1]) },
			"FormatDiffSummary": func() { FormatDiffSummary(p[0], p[1]) },
			"Unified":           func() { Unified(p[0], p[1], 3) },
		} {
			if bug3839Panics(f) {
				t.Fatalf("%s(%q, %q) panicked", name, p[0], p[1])
			}
		}
	}
}
