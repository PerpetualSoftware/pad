package store

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2798. A document rename rewrites `[[oldTitle]]` → `[[newTitle]]` in
// every linking document, and the cascade holds every rewritten body in memory
// before it writes any of them. Neither the title length nor the number of
// linking documents was bounded, so one rename could hold more content than the
// process could carry — measured at 20,000x amplification on the filing, and
// still 51.8x per document after the title bound alone.
//
// The title bound (models.MaxDocumentTitleRunes) is the cheap door. This file
// covers the wall: the cascade refuses when the TOTAL it would retain across
// the linking set — every read body plus every written body — exceeds
// MaxRenameCascadeRetainedBytes.

// linkerBody returns a body containing exactly n `[[A]]` occurrences, and the
// number of bytes the cascade would RETAIN for it when renaming "A" to a title
// of length newLen: the body it reads plus the body it writes.
//
// Deliberately computed here rather than by calling cascadeRetainedBytes — a
// test that reuses the implementation's arithmetic cannot catch that
// arithmetic being wrong.
func linkerBody(n, newLen int) (string, int) {
	body := strings.Repeat("[[A]]", n)
	rewritten := len(body) + n*(newLen-1)
	return body, len(body) + rewritten
}

// TestRenameCascade_RefusesOnProjectedTOTAL_NotPerDocument is the load-bearing
// test, and its shape is the finding it encodes: a per-document cap would not
// close this bug.
//
// Every linking document here retains comfortably UNDER the cap on its own.
// Only the total is over. A guard that tested each document in isolation would
// admit all three, allocate the sum, and pass a test that merely asserted "a
// huge single document is refused" — which is why this test asserts the
// per-document figure is under the cap as a PRECONDITION rather than trusting
// the constants to stay where they are.
func TestRenameCascade_RefusesOnProjectedTOTAL_NotPerDocument(t *testing.T) {
	const newTitleLen = 255 // the title bound; the worst title that survives it
	const linkers = 3

	// Size each linker so that linkers-1 of them fit under the cap and all of
	// them do not. Derived from the cap rather than hardcoded, so the test
	// keeps discriminating if the cap moves.
	perDocTarget := (MaxRenameCascadeRetainedBytes / linkers) + (MaxRenameCascadeRetainedBytes / (linkers * 4))
	occurrences := perDocTarget / (5 + 5 + (newTitleLen - 1))
	body, perDocRetained := linkerBody(occurrences, newTitleLen)

	// Preconditions — these are what make the test discriminate. If either
	// fails the test is no longer testing what its name says.
	if perDocRetained >= MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: per-document retention %d must be UNDER the cap %d, "+
			"otherwise a per-document guard would also pass this test",
			perDocRetained, MaxRenameCascadeRetainedBytes)
	}
	if total := perDocRetained * linkers; total <= MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: total retention %d must EXCEED the cap %d",
			total, MaxRenameCascadeRetainedBytes)
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeTotalBound")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")

	linkerIDs := make([]string, 0, linkers)
	for i := 0; i < linkers; i++ {
		d := createTestDoc(t, s, ws.ID, "Linker"+string(rune('a'+i)), body)
		linkerIDs = append(linkerIDs, d.ID)
	}

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})

	// 1. Refused, and refused as THIS error. Without the guard this returns
	//    nil, having allocated the sum.
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge", err)
	}

	// 2. Refused with the projection in the message. The only actionable
	//    information for a caller is what the rename would hold against what
	//    is allowed; an error that says "too large" and nothing else sends
	//    them guessing.
	if msg := err.Error(); !strings.Contains(msg, "maximum") || !strings.Contains(msg, "bytes") {
		t.Errorf("error message lacks the projection: %q", msg)
	}

	// 3. The rename ROLLED BACK. A guard that refused after writing some of
	//    the linkers would pass assertion 1 and leave the workspace with a
	//    half-cascaded rename — the exact inconsistency the cascade's
	//    all-or-nothing transaction exists to prevent.
	after, err := s.GetDocument(target.ID)
	if err != nil {
		t.Fatalf("read back renamed document: %v", err)
	}
	if after.Title != "A" {
		t.Errorf("title = %q after a refused rename, want it unchanged at %q", after.Title, "A")
	}
	for i, id := range linkerIDs {
		got, err := s.GetDocument(id)
		if err != nil {
			t.Fatalf("read back linker %d: %v", i, err)
		}
		if got.Content != body {
			t.Errorf("linker %d content changed by a refused rename (len %d, want %d)",
				i, len(got.Content), len(body))
		}
	}
}

// TestRenameCascade_AllowsAnOrdinaryRename is the control leg. Without it, a
// guard that refused every rename — or a cap accidentally set near zero —
// passes the test above while breaking the feature outright.
//
// The shape is deliberately the same as the refusal case with one fewer
// linker, so the only difference between green and red is the total.
func TestRenameCascade_AllowsAnOrdinaryRename(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeOrdinary")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "Linker", "before [[A]] middle [[A]] after")

	newTitle := "A Perfectly Ordinary Renamed Document"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("ordinary rename refused: %v", err)
	}

	got, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("read back linker: %v", err)
	}
	want := "before [[" + newTitle + "]] middle [[" + newTitle + "]] after"
	if got.Content != want {
		t.Errorf("cascade did not rewrite the links:\n got: %q\nwant: %q", got.Content, want)
	}
}

// TestRenameCascade_RefusesTheSingleDocumentAttack covers the k=1 case
// directly: one linking document holding the largest body a 2 MiB request can
// carry still retains 110,729,522 bytes once the title bound is in place
// (108,632,370 written plus the 2,097,152 read), which is 3.3x the cap. The
// attack is refused at every k, not only at a threshold count of documents.
//
// Kept separate from the total-versus-per-document test because it is the one
// case a per-document guard WOULD catch — asserting both makes it explicit
// that the total guard is a superset, not a replacement of unclear scope.
func TestRenameCascade_RefusesTheSingleDocumentAttack(t *testing.T) {
	const newTitleLen = 255
	occurrences := (MaxRenameCascadeRetainedBytes / (5 + 5 + (newTitleLen - 1))) * 2 // 2x the cap
	body, retained := linkerBody(occurrences, newTitleLen)
	if retained <= MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: single-document retention %d must exceed the cap %d", retained, MaxRenameCascadeRetainedBytes)
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeSingleDoc")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")
	createTestDoc(t, s, ws.ID, "Linker", body)

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge", err)
	}
}

// TestRenameCascade_CountsRetainedBytesNotJustOutput pins codex round 1's P1
// against the guard that shipped in the first commit, which summed only the
// PROJECTED OUTPUT.
//
// The counterexample is a rename to a SHORTER title. Every linker here holds a
// large body, but the rewrite shrinks it, so an output-only counter reports a
// small number while the cascade still retains every read body for its
// compare-and-set. Under the old guard the total below reported far under the
// cap and the rename proceeded; the retained-bytes guard refuses it.
//
// This is why the direction of the rename matters and why the constant is
// named for retention rather than for projection.
func TestRenameCascade_CountsRetainedBytesNotJustOutput(t *testing.T) {
	// Old title long, new title short — the shrinking direction.
	oldTitle := strings.Repeat("O", 200)
	newTitle := "n"

	// Each linker is ~2 MiB of `[[<200-char title>]]`, the shape a single 2 MiB
	// request can deliver.
	occurrencesPerDoc := (2 << 20) / (len(oldTitle) + 4)
	body := strings.Repeat("[["+oldTitle+"]]", occurrencesPerDoc)

	// Enough linkers that the RETAINED total is over the cap while the
	// projected OUTPUT total stays under it. That gap is the finding.
	linkers := (MaxRenameCascadeRetainedBytes / len(body)) + 2

	outputTotal := 0
	retainedTotal := 0
	for i := 0; i < linkers; i++ {
		rewritten := len(body) + occurrencesPerDoc*(len(newTitle)-len(oldTitle))
		outputTotal += rewritten
		retainedTotal += len(body) + rewritten
	}
	if outputTotal > MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: projected OUTPUT total %d must stay UNDER the cap %d, "+
			"otherwise an output-only guard would also refuse this and the test proves nothing",
			outputTotal, MaxRenameCascadeRetainedBytes)
	}
	if retainedTotal <= MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: RETAINED total %d must exceed the cap %d", retainedTotal, MaxRenameCascadeRetainedBytes)
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeShrinking")
	target := createTestDoc(t, s, ws.ID, oldTitle, "the document being renamed")
	for i := 0; i < linkers; i++ {
		createTestDoc(t, s, ws.ID, "Linker"+string(rune('a'+i)), body)
	}

	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("shrinking rename: got %v, want ErrRenameCascadeTooLarge — an output-only guard admits this", err)
	}
}

// TestRenameCascade_FindsLinkersWhoseTitleContainsABackslash pins codex round
// 1's P2. POSTGRES ONLY, and skipped loudly elsewhere rather than passing
// quietly — the defect is a DIALECT DIVERGENCE, and SQLite is the dialect that
// was accidentally right.
//
// The cascade finds linkers with `content LIKE ?`. Postgres LIKE treats
// backslash as the default escape character; SQLite LIKE has no default escape
// character at all. So an unescaped search term for a title containing `\` was
// searched for as the literal it is on SQLite, and as a DIFFERENT literal on
// Postgres — `[[Alpha\Beta]]` became `[[AlphaBeta]]`, the linking documents
// were not found, the cascade rewrote nothing, and the rename reported success
// leaving every link pointing at a title that no longer exists.
//
// A green run on SQLite is therefore a property of the DSN, not evidence about
// this fix, which is why this skips instead.
func TestRenameCascade_FindsLinkersWhoseTitleContainsABackslash(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("asserts a Postgres LIKE-escape property; SQLite LIKE has no default escape character, so the unescaped pattern is accidentally correct there")
	}

	ws := createTestWorkspace(t, s, "CascadeLikeBackslash")
	title := `Alpha\Beta`
	target := createTestDoc(t, s, ws.ID, title, "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "RealLinker", "see [["+title+"]] here")

	newTitle := "Renamed"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	got, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("read back linker: %v", err)
	}
	if want := "see [[Renamed]] here"; got.Content != want {
		t.Errorf("the linker was not rewritten — the cascade's LIKE pattern did not find it:\n got: %q\nwant: %q",
			got.Content, want)
	}
}

// TestRenameCascade_DoesNotSpendTheBudgetOnDocumentsThatDoNotLinkTheTitle is
// the rest of the class codex's backslash finding was an instance of
// (CONVE-18): `%` and `_` are LIKE wildcards in BOTH dialects, so an unescaped
// search term for a title containing them selects documents that do not link
// it.
//
// Over-matching cannot be caught by asserting content — the extra rows rewrite
// to themselves, because ReplaceTitle looks for the literal. It is observable
// through the guard, which is computed from this result set: unrelated
// documents inflate the retained total, and a rename that fits the cap is
// refused because of content it was never going to touch. That is the harm,
// and it is what this asserts.
//
// The first version of this test asserted the decoy's content was untouched
// and passed against the unescaped pattern — a vacuous green. Recorded here
// because the fix was to find the observable consequence, not to trust the
// mechanism.
func TestRenameCascade_DoesNotSpendTheBudgetOnDocumentsThatDoNotLinkTheTitle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		decoy string // matches the title read as a PATTERN, not as a literal
	}{
		{"percent", `Alpha%Beta`, `[[AlphaXYZBeta]]`},
		{"underscore", `Alpha_Beta`, `[[AlphaZBeta]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "CascadeLike"+tc.name)
			target := createTestDoc(t, s, ws.ID, tc.title, "the document being renamed")
			linker := createTestDoc(t, s, ws.ID, "RealLinker", "see [["+tc.title+"]] here")

			// Decoys big enough that INCLUDING them blows the cap, while the
			// real linker alone is negligible. With the pattern escaped they
			// are not selected and the rename is comfortably under budget.
			decoyBody := strings.Repeat("x", 1<<20) + " " + tc.decoy
			decoys := (MaxRenameCascadeRetainedBytes / len(decoyBody)) + 2
			for i := 0; i < decoys; i++ {
				createTestDoc(t, s, ws.ID, "Decoy"+string(rune('a'+i)), decoyBody)
			}

			newTitle := "Renamed"
			if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
				t.Fatalf("a rename well under the cap was refused because of documents that do not link it: %v", err)
			}

			got, err := s.GetDocument(linker.ID)
			if err != nil {
				t.Fatalf("read back linker: %v", err)
			}
			if want := "see [[Renamed]] here"; got.Content != want {
				t.Errorf("the real linker was not rewritten:\n got: %q\nwant: %q", got.Content, want)
			}
		})
	}
}

// TestRenameCascade_RetryRecheckesTheBudgetAgainstTheGrownBody pins codex round
// 1's other P1: the guard used to cover only the cascade's SCAN, while the
// compare-and-set's retry path re-read a linker's body and called ReplaceTitle
// on it with no bound at all.
//
// The re-read body is a NEW input supplied by whoever won the race, so a
// content edit landing inside the cascade's window could grow a linker from
// harmless to enormous and walk the rename straight back into the
// amplification it would have been refused for.
//
// POSTGRES ONLY, for the same structural reason as
// TestUpdateDocument_CascadeDoesNotOverwriteConcurrentEdit: SQLite's
// `_txlock=immediate` DSN takes the write lock at BEGIN and holds it across the
// cascade's whole read→write window, so a concurrent edit cannot commit inside
// it. A green run there would be a property of the DSN, not evidence about this
// guard.
func TestRenameCascade_RetryRecheckesTheBudgetAgainstTheGrownBody(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a concurrent edit to commit inside the cascade's read→write window; SQLite's BEGIN IMMEDIATE closes it structurally")
	}

	ws := createTestWorkspace(t, s, "CascadeRetryBudget")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")

	// Small at scan time — the cascade counts a few bytes and proceeds.
	linker := createTestDoc(t, s, ws.ID, "Linker", "before [[A]] after")

	// The winner's body is over the cap on its own, so the retry's re-read is
	// the first and only place this can be caught.
	newTitleLen := 255
	occurrences := (MaxRenameCascadeRetainedBytes / (5 + 5 + (newTitleLen - 1))) * 2
	grownBody, grownRetained := linkerBody(occurrences, newTitleLen)
	if grownRetained <= MaxRenameCascadeRetainedBytes {
		t.Fatalf("precondition: the grown body's retention %d must exceed the cap %d", grownRetained, MaxRenameCascadeRetainedBytes)
	}

	var once sync.Once
	var editErr error
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			// Content-only, so it takes no rename lock and can commit inside
			// the cascade's window (BUG-2785's seam, same mechanism).
			_, editErr = s.UpdateDocument(linker.ID, models.DocumentUpdate{Content: &grownBody})
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})

	if editErr != nil {
		t.Fatalf("the concurrent edit itself failed, so this run never exercised the retry path: %v", editErr)
	}
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge — the retry re-read an unbounded body", err)
	}
}

// TestRenameCascade_RetryBudgetExcludesThisDocumentsOwnStrings pins codex round
// 3's P1, and is deliberately separate from the test above: that one catches
// the retry check being ABSENT, this one catches it being too GENEROUS.
//
// The first version credited this document's own contribution back into its
// retry budget, on the reasoning that the retry replaces it. It does not — the
// original read and rewritten bodies stay reachable through `updates` while
// the write loop runs, so the re-read and its rewrite are allocated ON TOP of
// them. The bound could then be exceeded by up to one document's share while
// the arithmetic reported it satisfied.
//
// The grown body here is sized to fall BETWEEN the two budgets: under the
// credited-back budget (which would admit it) and over the true headroom
// (which refuses). A test using a body far over the cap cannot tell the two
// apart, because both refuse it.
//
// POSTGRES ONLY, same structural reason as the test above.
func TestRenameCascade_RetryBudgetExcludesThisDocumentsOwnStrings(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a concurrent edit to commit inside the cascade's read→write window; SQLite's BEGIN IMMEDIATE closes it structurally")
	}

	ws := createTestWorkspace(t, s, "CascadeRetryBudgetTight")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")

	const newTitleLen = 255
	perOccurrence := 5 + 5 + (newTitleLen - 1) // retained bytes per `[[A]]`

	// The single linker holds ~60% of the cap at scan time.
	scanOccurrences := (MaxRenameCascadeRetainedBytes * 6 / 10) / perOccurrence
	scanBody, scanRetained := linkerBody(scanOccurrences, newTitleLen)

	// The winner's body retains ~50% of the cap: comfortably under the cap on
	// its own, and under the OLD budget (which was the whole cap here, since
	// this is the only linker), but over the true headroom of cap - scanned.
	grownOccurrences := (MaxRenameCascadeRetainedBytes * 5 / 10) / perOccurrence
	grownBody, grownRetained := linkerBody(grownOccurrences, newTitleLen)

	// The credited-back budget was `cap - (retained - this document's share)`.
	// With a single linker those two terms are the same number, so it reduced
	// to the whole cap — which is exactly how a document could be handed a
	// budget that ignored what it was already holding.
	oldBudget := int64(MaxRenameCascadeRetainedBytes)
	newBudget := int64(MaxRenameCascadeRetainedBytes) - int64(scanRetained)
	if int64(grownRetained) > oldBudget {
		t.Fatalf("precondition: grown retention %d must fit the credited-back budget %d, or this test "+
			"cannot tell a too-generous budget from an absent one", grownRetained, oldBudget)
	}
	if int64(grownRetained) <= newBudget {
		t.Fatalf("precondition: grown retention %d must exceed the true headroom %d", grownRetained, newBudget)
	}

	linker := createTestDoc(t, s, ws.ID, "Linker", scanBody)

	var once sync.Once
	var editErr error
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			_, editErr = s.UpdateDocument(linker.ID, models.DocumentUpdate{Content: &grownBody})
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})

	if editErr != nil {
		t.Fatalf("the concurrent edit itself failed, so this run never exercised the retry path: %v", editErr)
	}
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge — the retry budget credited back "+
			"strings the cascade is still holding", err)
	}
}

// TestRenameCascade_ConcurrentEditThatRemovesTheLinkDoesNotRefuseTheRename pins
// codex round 3's P2.
//
// When a concurrent edit removes the link entirely, ReplaceTitle has nothing to
// replace — strings.Replace returns its input unchanged, allocating nothing.
// Charging that body twice (read + a rewritten copy that does not exist) could
// push a legitimate rename over the cap and refuse it for memory the cascade
// never allocates.
//
// The assertion is that the rename SUCCEEDS. POSTGRES ONLY, same reason as its
// neighbours.
func TestRenameCascade_ConcurrentEditThatRemovesTheLinkDoesNotRefuseTheRename(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a concurrent edit to commit inside the cascade's read→write window; SQLite's BEGIN IMMEDIATE closes it structurally")
	}

	ws := createTestWorkspace(t, s, "CascadeLinkRemoved")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")

	const newTitleLen = 255
	perOccurrence := 5 + 5 + (newTitleLen - 1)

	// Sized so that double-charging the link-free body would exceed the cap
	// while charging it once does not — otherwise the test passes either way.
	scanOccurrences := (MaxRenameCascadeRetainedBytes * 4 / 10) / perOccurrence
	scanBody, scanRetained := linkerBody(scanOccurrences, newTitleLen)

	// The winner's body has NO link left. Sized against the retry's real
	// HEADROOM (the cap less what the scan is still holding), not against the
	// cap: the first version of this test compared to the cap, and the body it
	// chose was legitimately over the headroom, so the refusal it caught was
	// correct behaviour rather than the double charge. Charged once it must
	// fit; charged twice it must not.
	headroom := int64(MaxRenameCascadeRetainedBytes) - int64(scanRetained)
	grownBody := strings.Repeat("y", int(headroom*7/10))
	if int64(len(grownBody)) > headroom {
		t.Fatalf("precondition: the link-free body %d must fit the retry headroom %d when charged once",
			len(grownBody), headroom)
	}
	if int64(2*len(grownBody)) <= headroom {
		t.Fatalf("precondition: the link-free body %d must EXCEED the headroom %d when charged twice, "+
			"or this test cannot detect the double charge", 2*len(grownBody), headroom)
	}

	linker := createTestDoc(t, s, ws.ID, "Linker", scanBody)

	var once sync.Once
	var editErr error
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			_, editErr = s.UpdateDocument(linker.ID, models.DocumentUpdate{Content: &grownBody})
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})

	if editErr != nil {
		t.Fatalf("the concurrent edit itself failed, so this run never exercised the retry path: %v", editErr)
	}
	if err != nil {
		t.Fatalf("rename refused after a concurrent edit REMOVED the link: %v — "+
			"the guard charged for a rewritten copy that ReplaceTitle never allocates", err)
	}
}
