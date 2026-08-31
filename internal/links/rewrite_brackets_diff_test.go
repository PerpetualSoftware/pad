package links

import (
	"math/rand"
	"strings"
	"testing"
)

// BUG-2804 differential tests for the single-pass rewrite.
//
// WHY THE OBVIOUS TEST WOULD BE VACUOUS, stated because the GO condition was
// worded as "RewriteBracketAt is equivalent to RewriteBracketsAt-of-one":
// RewriteBracketAt is now IMPLEMENTED as RewriteBracketsAt-of-one, so asserting
// those two agree compares a function with its own definition. It cannot fail,
// and a test that cannot fail is not evidence.
//
// The property actually worth pinning is that behaviour did not change ACROSS
// THE REFACTOR, so the oracle here is the pre-refactor implementation, copied
// verbatim from commit 37d84d87 and frozen. That test can fail, and it fails if
// anyone changes the per-bracket decision.
//
// The second test pins what actually licenses the fix: the cascade used to fold
// RewriteBracketAt over its rows in DESCENDING position order, and now makes one
// RewriteBracketsAt call. Those two must agree for every input, or the fix is a
// behaviour change wearing a performance change's clothes.

// rewriteBracketAtV0 is the implementation as it stood before BUG-2804, copied
// verbatim. Frozen on purpose: it is an ORACLE, not live code, and it must not
// be "kept in sync" with the real one — if the two disagree, that is the test
// working.
func rewriteBracketAtV0(content string, position int, targetTitle, newTitle, collSlug string) string {
	if position < 0 || position+2 > len(content) {
		return content
	}
	if content[position:position+2] != "[[" {
		return content
	}
	rest := content[position+2:]
	closeIdx := strings.Index(rest, "]]")
	if closeIdx < 0 {
		return content
	}
	body := rest[:closeIdx]
	bracketEnd := position + 2 + closeIdx + 2

	tLower := strings.ToLower(body)
	ttLower := strings.ToLower(targetTitle)

	newSegment := newTitle
	if collSlug != "" {
		pfx := collSlug + "/"
		if strings.HasPrefix(strings.ToLower(targetTitle), strings.ToLower(pfx)) {
			newSegment = pfx + newTitle
		}
	}

	if tLower == ttLower {
		return content[:position] + "[[" + newSegment + "]]" + content[bracketEnd:]
	}

	if strings.HasPrefix(tLower, ttLower+"|") {
		displaySuffix := body[len(targetTitle):]
		return content[:position] + "[[" + newSegment + displaySuffix + "]]" + content[bracketEnd:]
	}

	return content
}

// diffCase is one (content, position, targetTitle, newTitle, collSlug) probe.
type diffCase struct {
	name        string
	content     string
	position    int
	targetTitle string
	newTitle    string
	collSlug    string
}

// behaviourCorpus enumerates the documented behaviour of RewriteBracketAt —
// every branch named in its doc comment plus the rejection paths. Enumerated
// against the FUNCTION'S CONTRACT rather than against the diff, so it covers
// cases the refactor did not happen to touch.
func behaviourCorpus() []diffCase {
	return []diffCase{
		{"whole body match", "x [[Old]] y", 2, "Old", "New", ""},
		{"case-insensitive match", "x [[oLd]] y", 2, "Old", "New", ""},
		{"pipe display preserved", "x [[Old|alias]] y", 2, "Old", "New", ""},
		{"pipe display case-insensitive", "x [[oLd|alias]] y", 2, "Old", "New", ""},
		{"empty display segment", "x [[Old|]] y", 2, "Old", "New", ""},
		{"slug-qualified", "x [[tasks/Old]] y", 2, "tasks/Old", "New", "tasks"},
		{"slug-qualified with display", "x [[tasks/Old|alias]] y", 2, "tasks/Old", "New", "tasks"},
		{"slug set but title unqualified", "x [[Old]] y", 2, "Old", "New", "tasks"},
		{"slug case-insensitive prefix", "x [[TASKS/Old]] y", 2, "TASKS/Old", "New", "tasks"},
		{"non-matching body", "x [[Other]] y", 2, "Old", "New", ""},
		{"position not at a bracket", "x [[Old]] y", 3, "Old", "New", ""},
		{"position negative", "x [[Old]] y", -1, "Old", "New", ""},
		{"position past end", "x [[Old]] y", 999, "Old", "New", ""},
		{"position at very end", "ab", 1, "Old", "New", ""},
		{"unclosed bracket", "x [[Old y", 2, "Old", "New", ""},
		{"empty content", "", 0, "Old", "New", ""},
		{"empty target title", "x [[]] y", 2, "", "New", ""},
		{"empty new title", "x [[Old]] y", 2, "Old", "", ""},
		{"bracket at offset zero", "[[Old]] tail", 0, "Old", "New", ""},
		{"bracket at end of content", "head [[Old]]", 5, "Old", "New", ""},
		{"new title longer", "x [[Old]] y", 2, "Old", strings.Repeat("N", 64), ""},
		{"new title shorter", "x [[LongOldTitle]] y", 2, "LongOldTitle", "N", ""},
		{"title with pipe in it", "x [[A|B]] y", 2, "A|B", "New", ""},
		{"nested-looking brackets", "x [[Old]] [[Old]] y", 2, "Old", "New", ""},
		{"multibyte content around", "é [[Old]] é", 3, "Old", "New", ""},
		{"multibyte title", "x [[Ünïcødé]] y", 2, "Ünïcødé", "New", ""},
	}
}

func TestRewriteBracketAt_MatchesPreRefactorImplementation(t *testing.T) {
	for _, tc := range behaviourCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			want := rewriteBracketAtV0(tc.content, tc.position, tc.targetTitle, tc.newTitle, tc.collSlug)
			got := RewriteBracketAt(tc.content, tc.position, tc.targetTitle, tc.newTitle, tc.collSlug)
			if got != want {
				t.Errorf("behaviour changed across the refactor\n content=%q pos=%d target=%q new=%q slug=%q\n  v0=%q\n new=%q",
					tc.content, tc.position, tc.targetTitle, tc.newTitle, tc.collSlug, want, got)
			}
		})
	}
}

// TestRewriteBracketAt_MatchesPreRefactorOnRandomInput widens the corpus beyond
// the cases a human thought of. The generator deliberately produces positions
// that are often WRONG (mid-bracket, past the end, pointing at prose), because
// the rejection paths are where a refactor is most likely to diverge and the
// hand-written corpus can only cover the ones I anticipated.
func TestRewriteBracketAt_MatchesPreRefactorOnRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(20260831))
	titles := []string{"Old", "old", "tasks/Old", "A|B", "", "Ünïcødé", "LongOldTitle"}
	news := []string{"New", "", "N", strings.Repeat("N", 40)}
	slugs := []string{"", "tasks", "docs"}

	const iterations = 20000
	for i := 0; i < iterations; i++ {
		content := randomBracketContent(rng)
		pos := rng.Intn(len(content)+4) - 2 // includes negatives and past-end
		target := titles[rng.Intn(len(titles))]
		newTitle := news[rng.Intn(len(news))]
		slug := slugs[rng.Intn(len(slugs))]

		want := rewriteBracketAtV0(content, pos, target, newTitle, slug)
		got := RewriteBracketAt(content, pos, target, newTitle, slug)
		if got != want {
			t.Fatalf("divergence at iteration %d\n content=%q pos=%d target=%q new=%q slug=%q\n  v0=%q\n new=%q",
				i, content, pos, target, newTitle, slug, want, got)
		}
	}
}

// TestRewriteBracketsAt_MatchesSequentialDescendingFold is the test that
// licenses the fix.
//
// The cascade's old inner loop was exactly this fold: rows arrive ORDER BY
// position DESC, and each RewriteBracketAt call operated on the output of the
// last. Descending order is what kept the earlier offsets valid. The new code
// makes ONE RewriteBracketsAt call, and the two must agree on every input or
// this is a silent behaviour change.
//
// Positions fed in are the REAL bracket offsets plus deliberate corruptions, so
// the skip paths (non-matching shape, overlap, duplicates) are exercised rather
// than assumed unreachable.
func TestRewriteBracketsAt_MatchesSequentialDescendingFold(t *testing.T) {
	rng := rand.New(rand.NewSource(20260901))
	titles := []string{"Old", "old", "A|B", "Other"}
	news := []string{"New", "N", strings.Repeat("N", 24), ""}

	const iterations = 5000
	for i := 0; i < iterations; i++ {
		content := randomBracketContent(rng)
		target := titles[rng.Intn(len(titles))]
		newTitle := news[rng.Intn(len(news))]

		positions := bracketOffsets(content)
		// Corrupt some of the time so skip paths get exercised.
		if rng.Intn(3) == 0 && len(content) > 0 {
			positions = append(positions, rng.Intn(len(content)))
		}
		if rng.Intn(4) == 0 && len(positions) > 0 {
			positions = append(positions, positions[rng.Intn(len(positions))]) // duplicate
		}
		if len(positions) == 0 {
			continue
		}

		// Old behaviour: fold RewriteBracketAt over positions DESCENDING.
		desc := make([]int, len(positions))
		copy(desc, positions)
		for a := 0; a < len(desc); a++ {
			for b := a + 1; b < len(desc); b++ {
				if desc[b] > desc[a] {
					desc[a], desc[b] = desc[b], desc[a]
				}
			}
		}
		want := content
		for _, p := range desc {
			want = RewriteBracketAt(want, p, target, newTitle, "")
		}

		rewrites := make([]BracketRewrite, 0, len(positions))
		for _, p := range positions {
			rewrites = append(rewrites, BracketRewrite{Position: p, TargetTitle: target})
		}
		got, applied := RewriteBracketsAt(content, rewrites, newTitle, "")

		if got != want {
			t.Fatalf("single pass diverges from the descending fold at iteration %d\n content=%q positions=%v target=%q new=%q\n fold=%q\n pass=%q (applied=%d)",
				i, content, positions, target, newTitle, want, got, applied)
		}
		if want == content && applied != 0 {
			t.Fatalf("iteration %d: content unchanged but applied=%d — callers use the count to decide whether a write is owed",
				i, applied)
		}
	}
}

// TestRewriteBracketsAt_NoRewritesReturnsInputUnchanged pins the allocation-free
// no-op path the cascade relies on to skip a write.
func TestRewriteBracketsAt_NoRewritesReturnsInputUnchanged(t *testing.T) {
	const content = "nothing [[Other]] to do here"
	got, applied := RewriteBracketsAt(content, nil, "New", "")
	if got != content || applied != 0 {
		t.Fatalf("empty rewrites: got %q applied=%d, want %q applied=0", got, applied, content)
	}
	got, applied = RewriteBracketsAt(content, []BracketRewrite{{Position: 8, TargetTitle: "Old"}}, "New", "")
	if got != content || applied != 0 {
		t.Fatalf("non-matching rewrite: got %q applied=%d, want %q applied=0", got, applied, content)
	}
}

// TestRewriteBracketsAt_DoesNotMutateCallerSlice pins that the internal sort
// works on a copy. The cascade reuses its row slice, and a silent reordering
// would be the kind of defect that only shows up under a second caller.
func TestRewriteBracketsAt_DoesNotMutateCallerSlice(t *testing.T) {
	content := "[[Old]] a [[Old]] b [[Old]]"
	offs := bracketOffsets(content)
	rewrites := []BracketRewrite{
		{Position: offs[2], TargetTitle: "Old"},
		{Position: offs[0], TargetTitle: "Old"},
		{Position: offs[1], TargetTitle: "Old"},
	}
	before := make([]BracketRewrite, len(rewrites))
	copy(before, rewrites)

	if _, applied := RewriteBracketsAt(content, rewrites, "New", ""); applied != 3 {
		t.Fatalf("applied = %d, want 3", applied)
	}
	for i := range before {
		if rewrites[i] != before[i] {
			t.Fatalf("caller slice was reordered: index %d was %+v, now %+v", i, before[i], rewrites[i])
		}
	}
}

// TestRewriteBracketsAt_AppliesEveryBracketInOnePass is the plain positive case:
// N brackets, one call, all rewritten.
func TestRewriteBracketsAt_AppliesEveryBracketInOnePass(t *testing.T) {
	const n = 500
	content := strings.Repeat("[[Old]] filler ", n)
	offs := bracketOffsets(content)
	if len(offs) != n {
		t.Fatalf("fixture: %d offsets, want %d", len(offs), n)
	}
	rewrites := make([]BracketRewrite, 0, n)
	for _, p := range offs {
		rewrites = append(rewrites, BracketRewrite{Position: p, TargetTitle: "Old"})
	}
	got, applied := RewriteBracketsAt(content, rewrites, "New", "")
	if applied != n {
		t.Fatalf("applied = %d, want %d", applied, n)
	}
	if want := strings.Repeat("[[New]] filler ", n); got != want {
		t.Fatalf("got %q, want %q", got[:80], want[:80])
	}
}

// randomBracketContent builds a short body mixing prose, matching brackets,
// non-matching brackets, unclosed brackets and multibyte runes.
func randomBracketContent(rng *rand.Rand) string {
	pieces := []string{
		"[[Old]]", "[[old]]", "[[Old|alias]]", "[[Other]]", "[[tasks/Old]]",
		"[[A|B]]", "[[", "]]", "[[Old", "prose ", " ", "é", "[[]]", "[[Old|]]",
	}
	var b strings.Builder
	n := rng.Intn(8)
	for i := 0; i < n; i++ {
		b.WriteString(pieces[rng.Intn(len(pieces))])
	}
	return b.String()
}

// bracketOffsets returns the byte offset of every `[[` that has a matching `]]`
// after it, scanning the way the parser does.
func bracketOffsets(content string) []int {
	var out []int
	for i := 0; i+1 < len(content); i++ {
		if content[i] == '[' && content[i+1] == '[' {
			if strings.Index(content[i+2:], "]]") >= 0 {
				out = append(out, i)
			}
		}
	}
	return out
}
