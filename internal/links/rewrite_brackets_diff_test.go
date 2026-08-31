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

// guardedDomain reports whether the frozen V0 oracle is still authoritative for
// this input.
//
// BUG-2805 deliberately changes behaviour in two places, so the oracle cannot
// be applied blanket any more — but narrowing it to the domain BUG-2805 does
// NOT touch keeps it load-bearing for everything else, which is the large
// majority. Outside this domain the intended new behaviour is pinned by name in
// TestRewriteBracketAt_IntentionalDivergencesFromV0 and the BUG-2805 tests.
//
// The domain is: the bracket at `position` has a NON-EMPTY body containing none
// of `\`, `]`, `|`. That is exactly "no escape grammar involved", which is
// where BUG-2805 promises byte-identical behaviour.
func guardedDomain(content string, position int) bool {
	if position < 0 || position+2 > len(content) || content[position:position+2] != "[[" {
		return true // rejection paths are unchanged; still guarded
	}
	rest := content[position+2:]
	closeIdx := strings.Index(rest, "]]")
	if closeIdx < 0 {
		return true // unterminated: unchanged
	}
	body := rest[:closeIdx]
	if body == "" {
		return false // empty body — see the intentional-divergence test
	}
	return !strings.ContainsAny(body, `\]|`)
}

// TestRewriteBracketAt_IntentionalDivergencesFromV0 names every input where
// BUG-2805 deliberately departs from the frozen pre-refactor oracle, so the
// departures are a checked list rather than a gap in the differential tests.
func TestRewriteBracketAt_IntentionalDivergencesFromV0(t *testing.T) {
	cases := []struct {
		name            string
		content         string
		position        int
		targetTitle     string
		newTitle        string
		wantV0, wantNew string
		why             string
	}{
		{
			name: "empty body is not a link", content: "x [[]] y", position: 2,
			targetTitle: "", newTitle: "New",
			wantV0: "x [[New]] y", wantNew: "x [[]] y",
			why: "the parser's body production is `+`, so `[[]]` is not a link and no " +
				"position is ever recorded for one. V0 would MINT a link out of a non-link " +
				"if handed a drifted offset; the escape-aware scanner refuses.",
		},
		{
			name: "escaped body now MATCHES", content: `see [[Weird \] Title]] here`, position: 4,
			targetTitle: "Weird ] Title", newTitle: "Renamed",
			wantV0: `see [[Weird \] Title]] here`, wantNew: "see [[Renamed]] here",
			why: "BUG-2805 direction 1: V0 compared the RAW body against the unescaped " +
				"title, never matched, and left the link stale while the index flipped broken.",
		},
		{
			name: "emission now ESCAPES", content: "Ref [[Plain Target]] here.", position: 4,
			targetTitle: "Plain Target", newTitle: "New ] Name",
			wantV0: "Ref [[New ] Name]] here.", wantNew: `Ref [[New \] Name]] here.`,
			why: "BUG-2805 direction 2a: V0's plain concatenation emitted an unparseable " +
				"bracket, so the reparse deleted the index row — permanent damage.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteBracketAtV0(tc.content, tc.position, tc.targetTitle, tc.newTitle, ""); got != tc.wantV0 {
				t.Fatalf("fixture drift: V0 = %q, want %q — this test documents a divergence "+
					"FROM a specific old behaviour, so the old behaviour must be what it says", got, tc.wantV0)
			}
			if got := RewriteBracketAt(tc.content, tc.position, tc.targetTitle, tc.newTitle, ""); got != tc.wantNew {
				t.Errorf("new = %q, want %q\nwhy this diverges: %s", got, tc.wantNew, tc.why)
			}
		})
	}
}

func TestRewriteBracketAt_MatchesPreRefactorImplementation(t *testing.T) {
	for _, tc := range behaviourCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			if !guardedDomain(tc.content, tc.position) {
				t.Skip("outside the V0-guarded domain — see TestRewriteBracketAt_IntentionalDivergencesFromV0")
			}
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

		if !guardedDomain(content, pos) {
			continue
		}
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
			// rewriteBracketAtV0, NOT RewriteBracketAt. The latter now delegates
			// to RewriteBracketsAt, so folding it would build the oracle out of
			// the code under test and the comparison would be circular — the
			// same vacuity that made the one-element assertion worthless, in
			// the multi-bracket case, which I missed when I caught it there
			// (codex R3). The frozen pre-refactor primitive is the only
			// genuinely independent oracle available.
			want = rewriteBracketAtV0(want, p, target, newTitle, "")
		}

		// SHUFFLED on purpose. bracketOffsets returns ascending order, but the
		// real caller passes the cascade's `ORDER BY wl.position DESC` rows —
		// so an ascending-only fixture cannot detect a missing internal sort.
		// Found by mutation: dropping the sort left this test green until the
		// order fed in stopped being the order the implementation wanted.
		shuffled := make([]int, len(positions))
		copy(shuffled, positions)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		rewrites := make([]BracketRewrite, 0, len(shuffled))
		for _, p := range shuffled {
			rewrites = append(rewrites, BracketRewrite{Position: p, TargetTitle: target})
		}
		got, applied := RewriteBracketsAt(content, rewrites, NewTitleEscaper(newTitle, ""))

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
	got, applied := RewriteBracketsAt(content, nil, NewTitleEscaper("New", ""))
	if got != content || applied != 0 {
		t.Fatalf("empty rewrites: got %q applied=%d, want %q applied=0", got, applied, content)
	}
	got, applied = RewriteBracketsAt(content, []BracketRewrite{{Position: 8, TargetTitle: "Old"}}, NewTitleEscaper("New", ""))
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

	if _, applied := RewriteBracketsAt(content, rewrites, NewTitleEscaper("New", "")); applied != 3 {
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
	got, applied := RewriteBracketsAt(content, rewrites, NewTitleEscaper("New", ""))
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

// TestRewriteBracketsAt_ByteIdenticalRewriteIsNotAChange pins that `applied`
// counts CHANGES, not matches (codex R1 P2).
//
// The cascade uses the count to decide whether to write the row at all. The
// pre-BUG-2804 code compared whole bodies for this, so a rewrite that
// reproduced the content byte-for-byte wrote nothing; counting matches instead
// would rewrite content, bump seq, and emit an item.bulk_updated event for
// every linker on a rename that changed nothing.
//
// REACHABILITY, stated rather than implied: I have NOT established that
// cascadeTitleRename can reach this today — it returns early when
// oldTitle == newTitle, which blocks the obvious route. This pins the
// FUNCTION's contract, which the cascade depends on, without claiming a live
// user-visible bug.
func TestRewriteBracketsAt_ByteIdenticalRewriteIsNotAChange(t *testing.T) {
	for _, tc := range []struct {
		name        string
		content     string
		targetTitle string
		newTitle    string
		collSlug    string
	}{
		{"plain body already equals the new title", "x [[Old]] y", "Old", "Old", ""},
		{"qualified body already equals", "x [[tasks/Old]] y", "tasks/Old", "Old", "tasks"},
		{"display suffix preserved, title unchanged", "x [[Old|alias]] y", "Old", "Old", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offs := bracketOffsets(tc.content)
			if len(offs) != 1 {
				t.Fatalf("fixture: %d brackets, want 1", len(offs))
			}
			got, applied := RewriteBracketsAt(tc.content,
				[]BracketRewrite{{Position: offs[0], TargetTitle: tc.targetTitle}}, NewTitleEscaper(tc.newTitle, tc.collSlug))
			if got != tc.content {
				t.Errorf("content changed: got %q, want %q", got, tc.content)
			}
			if applied != 0 {
				t.Errorf("applied = %d, want 0 — a byte-identical rewrite is not a change, and the "+
					"cascade uses this count to decide whether to write the row", applied)
			}
		})
	}
}

// TestRewriteBracketsAt_MixedChangedAndUnchangedCountsOnlyTheChanged pins the
// count when a single body carries both kinds, which is the case a whole-body
// comparison could never have distinguished.
func TestRewriteBracketsAt_MixedChangedAndUnchangedCountsOnlyTheChanged(t *testing.T) {
	content := "[[Old]] and [[New]] and [[Old]]"
	offs := bracketOffsets(content)
	if len(offs) != 3 {
		t.Fatalf("fixture: %d brackets, want 3", len(offs))
	}
	rewrites := []BracketRewrite{
		{Position: offs[0], TargetTitle: "Old"},
		{Position: offs[1], TargetTitle: "New"}, // already reads [[New]] — no change
		{Position: offs[2], TargetTitle: "Old"},
	}
	got, applied := RewriteBracketsAt(content, rewrites, NewTitleEscaper("New", ""))
	if want := "[[New]] and [[New]] and [[New]]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if applied != 2 {
		t.Errorf("applied = %d, want 2 — the middle bracket was already [[New]]", applied)
	}
}

// TestProjectRewrittenLen_IsLockstepWithTheRealPass pins the invariant the cap
// depends on: what the projection PREDICTS must be exactly what the rewrite
// DOES, for every input including the defensive ones.
//
// This is the property codex R2 found broken. The projection advanced its
// cursor past a no-op bracket while the real pass did not, so on corrupt,
// duplicated or overlapping offsets the two disagreed about which rewrites the
// overlap guard skips — projection reporting (unchanged, 0 applied) where the
// pass actually applied one and grew the body. The cascade charges from the
// projection, so the bound's guarantee leaked on precisely the inputs the
// defensive paths exist for.
//
// The corpus deliberately includes NESTED-LOOKING brackets like `[[A[[B]]]]`,
// where two valid `[[` offsets share a closing `]]` and therefore genuinely
// overlap. Random offsets alone rarely produce that shape, which is why it is
// constructed rather than left to chance.
func TestProjectRewrittenLen_IsLockstepWithTheRealPass(t *testing.T) {
	rng := rand.New(rand.NewSource(20260902))
	titles := []string{"Old", "old", "A|B", "Other", "A[[B", "B"}
	news := []string{"New", "N", "A[[B", strings.Repeat("N", 20), ""}

	overlapping := []string{
		"[[A[[B]]]]",
		"[[Old[[Old]]]]",
		"x [[A[[B]]]] y",
		"[[A[[B]]]] [[Old]]",
	}

	check := func(t *testing.T, content string, rewrites []BracketRewrite, newTitle, collSlug string) {
		t.Helper()
		wantLen, wantApplied := ProjectRewrittenLen(content, rewrites, NewTitleEscaper(newTitle, collSlug))
		got, gotApplied := RewriteBracketsAt(content, rewrites, NewTitleEscaper(newTitle, collSlug))
		if len(got) != wantLen || gotApplied != wantApplied {
			t.Fatalf("projection and the real pass disagree\n content=%q rewrites=%+v new=%q slug=%q\n"+
				" projected len=%d applied=%d\n actual    len=%d applied=%d (result %q)",
				content, rewrites, newTitle, collSlug, wantLen, wantApplied, len(got), gotApplied, got)
		}
	}

	// MIXED target titles per position, not one title for the whole call.
	// Cascade rows carry their own target_title (one row may have stored
	// "Old" and another "tasks/Old"), and a single shared title is exactly
	// what hid this divergence from the first version of this test: the
	// reproducing case needs one bracket to be a NO-OP while an overlapping
	// one CHANGES, which a single title cannot express.
	for _, content := range overlapping {
		offs := bracketOffsets(content)
		for _, newTitle := range news {
			for ti := range titles {
				rewrites := make([]BracketRewrite, 0, len(offs))
				for j, p := range offs {
					rewrites = append(rewrites, BracketRewrite{
						Position:    p,
						TargetTitle: titles[(ti+j)%len(titles)],
					})
				}
				check(t, content, rewrites, newTitle, "")
			}
		}
	}

	// The exact shape codex R2 named, asserted directly so the regression has
	// a named home rather than depending on the random search rediscovering it:
	// bracket [0,8) is a no-op, bracket [3,8) overlaps it and changes.
	check(t, "[[A[[B]]]]", []BracketRewrite{
		{Position: 0, TargetTitle: "A[[B"},
		{Position: 3, TargetTitle: "B"},
	}, "A[[B", "")

	const iterations = 20000
	for i := 0; i < iterations; i++ {
		content := randomBracketContent(rng)
		if rng.Intn(4) == 0 {
			content += overlapping[rng.Intn(len(overlapping))]
		}
		target := titles[rng.Intn(len(titles))]
		newTitle := news[rng.Intn(len(news))]

		positions := bracketOffsets(content)
		if rng.Intn(3) == 0 && len(content) > 0 {
			positions = append(positions, rng.Intn(len(content)))
		}
		if rng.Intn(4) == 0 && len(positions) > 0 {
			positions = append(positions, positions[rng.Intn(len(positions))])
		}
		if len(positions) == 0 {
			continue
		}
		rng.Shuffle(len(positions), func(a, b int) { positions[a], positions[b] = positions[b], positions[a] })

		rewrites := make([]BracketRewrite, 0, len(positions))
		for j, p := range positions {
			tt := target
			if rng.Intn(2) == 0 {
				tt = titles[(i+j)%len(titles)]
			}
			rewrites = append(rewrites, BracketRewrite{Position: p, TargetTitle: tt})
		}
		check(t, content, rewrites, newTitle, "")
	}
}

// TestRewriteBracketsAt_EmissionRoundTripsThroughTheParser is the property that
// makes BUG-2805 closed rather than patched: whatever title a rename emits, the
// PARSER must read back exactly that title.
//
// This is the check no per-character test can give you. The repro on TASK-2826
// enumerated `]`, `\]`, `|` and bare `\` and found four different outcomes —
// destroyed, wrong-title, rescued, rescued. A property over generated titles
// covers the combinations nobody enumerated, and it fails loudly on the two
// that were real defects.
func TestRewriteBracketsAt_EmissionRoundTripsThroughTheParser(t *testing.T) {
	rng := rand.New(rand.NewSource(20260903))
	// Alphabet weighted toward the characters that carry meaning in the body
	// grammar; ordinary letters are along for realism.
	alphabet := []string{"a", "B", " ", "]", "|", `\`, `\]`, `\|`, `\\`, "/", "é", "-"}

	for i := 0; i < 4000; i++ {
		n := 1 + rng.Intn(6)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		newTitle := sb.String()

		content := "before [[Plain Target]] after"
		offs := bracketOffsets(content)
		got, applied := RewriteBracketsAt(content,
			[]BracketRewrite{{Position: offs[0], TargetTitle: "Plain Target"}},
			NewTitleEscaper(newTitle, ""))
		if applied != 1 {
			t.Fatalf("iteration %d: applied = %d, want 1 (newTitle=%q)", i, applied, newTitle)
		}

		// The emitted content must parse, and the ONE link it contains must
		// resolve to exactly the title we renamed to.
		links := ExtractWikiLinks(got)
		if len(links) != 1 {
			t.Fatalf("iteration %d: emitted %q from newTitle=%q — parser found %d links, want 1. "+
				"This is the BUG-2805 direction-2a shape: an unparseable bracket whose index row "+
				"the reparse DELETES, permanently.", i, got, newTitle, len(links))
		}
		if links[0].Title != newTitle {
			t.Fatalf("iteration %d: emitted %q from newTitle=%q — parser read title %q. "+
				"This is the direction-2b shape: valid syntax for the WRONG title.",
				i, got, newTitle, links[0].Title)
		}
	}
}

// TestScanBracketBodyAgreesWithTheParser pins the third half of BUG-2805, which
// the repro did not name.
//
// The rewriter used strings.Index(rest, "]]") to find its closing delimiter.
// That is not the grammar: in `[[A\]]]` it stops at the `]` belonging to the
// `\]` escape. It was harmless only while the rewriter never EMITTED escapes —
// and this fix makes escaped bodies routine, so a scan that disagrees with the
// parser would corrupt on the next rename what this one wrote.
//
// The parser is the oracle: for every link ExtractWikiLinks reports, the
// scanner must find the same body at the same offset.
func TestScanBracketBodyAgreesWithTheParser(t *testing.T) {
	// CONSTRUCTED FIRST, because the discriminating shape is rare under random
	// generation: the naive `strings.Index(rest, "]]")` only picks the wrong
	// close when an escaped `\]` is IMMEDIATELY followed by the real `]]`.
	// `[[Weird \] Title]]` does NOT discriminate — its `\]` is followed by a
	// space, so the naive scan happens to land correctly. Leaving this to the
	// generator let the mutant survive this test and be killed only incidentally
	// by an unrelated case.
	for _, tc := range []struct{ name, content, wantBody string }{
		{"escape immediately before the close", `[[A\]]]`, `A\]`},
		{"escaped pipe before the close", `[[A\|]]`, `A\|`},
		{"escaped backslash before the close", `[[A\\]]`, `A\\`},
		{"escape mid-body", `[[Weird \] Title]]`, `Weird \] Title`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, ok := scanBracketBody(tc.content, 0)
			if !ok {
				t.Fatalf("scanner found no bracket in %q", tc.content)
			}
			if body != tc.wantBody {
				t.Errorf("scanner body = %q, want %q — it disagrees with the grammar about "+
					"where the body ends", body, tc.wantBody)
			}
			ls := ExtractWikiLinks(tc.content)
			if len(ls) != 1 {
				t.Fatalf("fixture: parser found %d links in %q, want 1", len(ls), tc.content)
			}
			key, _, _ := splitOnUnescapedPipe(body)
			if got := unescapeWikiBody(key); got != ls[0].Title {
				t.Errorf("scanner key unescapes to %q, parser read title %q", got, ls[0].Title)
			}
		})
	}

	rng := rand.New(rand.NewSource(20260904))
	pieces := []string{
		"[[A]]", `[[A\]]]`, `[[A\|B]]`, `[[A\\]]`, "[[A|B]]", "[[", "]]", "text ",
		`[[Weird \] Title]]`, "[[tasks/X]]", "é", `\`, "]", "|",
	}

	for i := 0; i < 4000; i++ {
		var sb strings.Builder
		for j := 0; j < 1+rng.Intn(5); j++ {
			sb.WriteString(pieces[rng.Intn(len(pieces))])
		}
		content := sb.String()

		for _, l := range ExtractWikiLinks(content) {
			body, end, ok := scanBracketBody(content, l.Position)
			if !ok {
				t.Fatalf("iteration %d: parser reported a link at %d in %q but the scanner "+
					"found no bracket there", i, l.Position, content)
			}
			// The scanner returns the RAW body; unescaping it must give what the
			// parser resolved the title/key to.
			if end > len(content) || end < l.Position {
				t.Fatalf("iteration %d: scanner returned end=%d out of range for %q", i, end, content)
			}
			key, _, _ := splitOnUnescapedPipe(body)
			if unescapeWikiBody(key) != l.Title && l.Kind == WikiLinkKindTitle {
				t.Fatalf("iteration %d: content %q at %d — scanner body %q unescapes to key %q, "+
					"parser read title %q. The scan disagrees with the grammar.",
					i, content, l.Position, body, unescapeWikiBody(key), l.Title)
			}
		}
	}
}

// TestScanBracketBodyEdgeCasesMatchTheParser locks the delimiter edges down
// against the parser as oracle, one assertion per shape.
//
// The parity property above generates content; this enumerates the shapes where
// a hand-written scanner and a regex are most likely to disagree, so a failure
// names WHICH edge broke instead of printing a random string. Every row was
// verified against ExtractWikiLinks rather than reasoned about — including the
// two where BOTH refuse, which is the agreement that is easiest to lose by
// making the scanner more permissive than the grammar.
func TestScanBracketBodyEdgeCasesMatchTheParser(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		wantOK   bool
		wantBody string
		why      string
	}{
		{"escape consumes the would-be close", `[[A\]]`, false, "",
			`the \] escape eats the first ], leaving a single ] that cannot close`},
		{"escaped close then real close", `[[A\]]]`, true, `A\]`,
			"the naive strings.Index scan stops one byte early here — this is THE discriminating shape"},
		{"stray ] after the close", `[[A]]]`, true, "A",
			"close at the FIRST ]], trailing ] is ordinary text"},
		{"]] run after the close", `[[A]]]]`, true, "A",
			"same: first ]] wins, the rest is text"},
		{"trailing backslash at EOF", `[[A\`, false, "",
			`the \. production has nothing to consume`},
		{"escaped backslash then close", `[[A\\]]`, true, `A\\`,
			`\\ is one escaped backslash, so the ]] that follows is a real close`},
		{"empty body", "[[]]", false, "",
			"the body production is `+`; `[[]]` is not a link at all"},
		{"bare ] mid-body", "[[A]B]]", false, "",
			"an unescaped ] is not in the body production — both scanner and parser refuse"},
		{"escaped pipe", `[[A\|B]]`, true, `A\|B`,
			"the escaped pipe is body text, not a display separator"},
		{"body is only an escaped ]", `[[\]]]`, true, `\]`,
			"shortest body that needs escape-aware scanning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, ok := scanBracketBody(tc.content, 0)
			if ok != tc.wantOK || body != tc.wantBody {
				t.Errorf("scanner(%q) = (body %q, ok %v), want (%q, %v)\nwhy: %s",
					tc.content, body, ok, tc.wantBody, tc.wantOK, tc.why)
			}
			// The parser is the oracle: it must agree about whether a link is
			// there at all.
			ls := ExtractWikiLinks(tc.content)
			if (len(ls) > 0) != tc.wantOK {
				t.Errorf("parser found %d links in %q but the scanner says ok=%v — the scanner "+
					"and the grammar disagree about whether this is a link", len(ls), tc.content, ok)
			}
		})
	}
}

// TestBUG2805_AccidentalCompatibilitiesSurvive pins the two rescues Rook's
// repro identified as non-reproducing directions (TASK-2826 2c and 2d).
//
// Both are ACCIDENTS of other code — the legacy full-body-with-pipe fallback
// and unescapeWikiBody's leniency toward a stray backslash — and both are
// keyed on the exact bytes an escape-aware emitter changes. A fix that quietly
// broke them would trade two silent successes for two silent failures.
func TestBUG2805_AccidentalCompatibilitiesSurvive(t *testing.T) {
	t.Run("legacy unescaped-pipe content still parses to the full-body title", func(t *testing.T) {
		// Content a USER already has, written before this fix. Untouched by the
		// change — the parser side is not modified — and it must still resolve
		// via the full-body reading.
		got := ExtractWikiLinks("Ref [[New | Name]] here.")
		if len(got) != 1 {
			t.Fatalf("parser found %d links, want 1", len(got))
		}
		if got[0].Title != "New " || got[0].Display != " Name" {
			t.Errorf("split reading changed: title=%q display=%q — the store's full-body "+
				"fallback keys on exactly these values (wiki_links.go title fallback)",
				got[0].Title, got[0].Display)
		}
	})

	t.Run("escaped-pipe emission resolves without needing the fallback", func(t *testing.T) {
		// What the fix now EMITS for the same rename. Better than the rescue:
		// it parses as a single title directly, no fallback required.
		content := "Ref [[Plain Target]] here."
		offs := bracketOffsets(content)
		got, _ := RewriteBracketsAt(content,
			[]BracketRewrite{{Position: offs[0], TargetTitle: "Plain Target"}},
			NewTitleEscaper("New | Name", ""))
		if want := `Ref [[New \| Name]] here.`; got != want {
			t.Fatalf("emitted %q, want %q", got, want)
		}
		links := ExtractWikiLinks(got)
		if len(links) != 1 || links[0].Title != "New | Name" || links[0].HasDisplay {
			t.Errorf("emitted link parsed as title=%q display=%v hasDisplay=%v; want the whole "+
				"thing as one title", links[0].Title, links[0].Display, links[0].HasDisplay)
		}
	})

	t.Run("stray backslash leniency preserved end to end", func(t *testing.T) {
		content := "Go [[BS Target]] now."
		offs := bracketOffsets(content)
		got, _ := RewriteBracketsAt(content,
			[]BracketRewrite{{Position: offs[0], TargetTitle: "BS Target"}},
			NewTitleEscaper(`Odd \ Name`, ""))
		links := ExtractWikiLinks(got)
		if len(links) != 1 {
			t.Fatalf("emitted %q — parser found %d links, want 1", got, len(links))
		}
		if links[0].Title != `Odd \ Name` {
			t.Errorf("emitted %q parsed to %q, want %q — the bare-backslash title must still "+
				"round-trip (2d)", got, links[0].Title, `Odd \ Name`)
		}
	})
}
