package store

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2785. A document rename cascades through every document whose content
// links the old title, as a read-modify-write across two statements: the
// cascade SELECTs each linker's content, rewrites the string in Go, then
// UPDATEs the row. A content edit to a linker committing between those two
// statements was silently overwritten — the cascade wrote the body it built
// from the version it read, and the user's edit was gone with no error and no
// version row.
//
// POSTGRES ONLY, and skipped loudly elsewhere rather than passing quietly.
// SQLite's DSN sets `_txlock=immediate`, so UpdateDocument's db.Begin() takes
// the write lock at BEGIN and holds it across the cascade's whole read→write
// window; a concurrent content edit cannot commit inside it and serializes on
// busy_timeout instead. The lost update is therefore unreachable on SQLite,
// and a green run there would be a property of the DSN rather than evidence
// about this fix. Worse than useless: driving the seam on SQLite would have
// the concurrent write block on the lock the rename already holds, so the test
// would stall for busy_timeout and then assert about a serialized order that
// has nothing to do with the defect.
//
// That dialect split is also why the mutation matrix for this fix has to be
// run under Postgres. Removing the compare-and-set leaves the SQLite suite
// entirely green.

// TestUpdateDocument_CascadeDoesNotOverwriteConcurrentEdit drives the exact
// interleaving the bug describes, through the afterLinkCascadeRead seam: the
// rename has read the linker's body and not yet written it back when an
// ordinary content edit commits.
//
// The assertions are deliberately three, because each catches a different
// wrong fix (CONVE-12 — assert what the WRONG behaviour would DO):
//
//   - the concurrent edit's text survives. An unconditional UPDATE erases it;
//     this is the defect itself.
//   - the link is rewritten to the new title. A "fix" that simply skipped a
//     contended linker would preserve the edit and silently abandon the
//     cascade, which is a different bug wearing this one's green test.
//   - the old title is gone. Without it, a rewrite that APPENDED rather than
//     replaced would pass the second assertion.
func TestUpdateDocument_CascadeDoesNotOverwriteConcurrentEdit(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("asserts a Postgres READ COMMITTED lost-update property; SQLite's BEGIN IMMEDIATE closes the window structurally")
	}
	ws := createTestWorkspace(t, s, "CascadeLostUpdate")

	target := createTestDoc(t, s, ws.ID, "Alpha", "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "Linker", "before [[Alpha]] after")

	// The concurrent edit keeps the link and adds text. Keeping the link
	// matters: it means the cascade still has work to do on the NEW body, so
	// the test can tell "re-applied the rewrite to the winner's text" from
	// "gave up on this linker".
	const editMarker = "TEXT FROM THE CONCURRENT WRITER"
	concurrentBody := "before [[Alpha]] after — " + editMarker

	var once sync.Once
	var editErr error
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			// A content-only update: it takes no rename lock (BUG-2778's lock
			// fires only when a title is supplied), which is precisely why it
			// can commit inside the cascade's window.
			_, editErr = s.UpdateDocument(linker.ID, models.DocumentUpdate{Content: &concurrentBody})
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := "Beta"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if editErr != nil {
		t.Fatalf("the concurrent content edit itself failed, so this run never "+
			"exercised the race: %v", editErr)
	}

	got, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("GetDocument(linker): %v", err)
	}
	if got == nil {
		t.Fatal("linker missing after the rename")
	}

	if !strings.Contains(got.Content, editMarker) {
		t.Errorf("the concurrent edit was overwritten by the cascade — this is BUG-2785.\n got: %q\nwant it to contain: %q", got.Content, editMarker)
	}
	if !strings.Contains(got.Content, "[[Beta]]") {
		t.Errorf("the cascade did not rewrite the link on the winning body; a fix that "+
			"skips a contended linker preserves the edit but abandons the rename's job.\n got: %q", got.Content)
	}
	if strings.Contains(got.Content, "[[Alpha]]") {
		t.Errorf("the old title survives the rewrite.\n got: %q", got.Content)
	}
}

// TestUpdateDocument_CascadeTreatsSoftDeletedLinkerAsDone pins the OTHER arm of
// the zero-row result, and is the test that makes the disambiguating probe
// necessary rather than decorative.
//
// Once the UPDATE carries `content = ?` alongside `deleted_at IS NULL`, a
// zero-row result means one of two opposite things: the linker was archived
// (stop — there is no link left to keep consistent) or a concurrent edit landed
// (retry against the new body). Without the probe that tells them apart, an
// archived linker sends the loop to exhaustion and fails an otherwise valid
// rename.
//
// So the assertion is that the rename SUCCEEDS, and the counterfactual is
// specific: with the probe removed, this returns the exhaustion error.
func TestUpdateDocument_CascadeTreatsSoftDeletedLinkerAsDone(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a write to commit inside the cascade's window, which SQLite's BEGIN IMMEDIATE prevents")
	}
	ws := createTestWorkspace(t, s, "CascadeDeletedLinker")

	target := createTestDoc(t, s, ws.ID, "Gamma", "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "GammaLinker", "before [[Gamma]] after")

	var once sync.Once
	var delErr error
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			delErr = s.DeleteDocument(linker.ID)
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := "Delta"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename failed because a linker was archived mid-cascade; an archived "+
			"linker is a documented normal outcome, not an error: %v", err)
	}
	if delErr != nil {
		t.Fatalf("the concurrent soft-delete itself failed, so this run never "+
			"exercised the arm it exists for: %v", delErr)
	}

	// The rename itself must still have landed on the target.
	got, err := s.GetDocument(target.ID)
	if err != nil {
		t.Fatalf("GetDocument(target): %v", err)
	}
	if got == nil || got.Title != newTitle {
		t.Errorf("target title = %v, want %q", got, newTitle)
	}
}

// TestUpdateDocument_CascadeExhaustionRollsBackTheWholeRename pins the
// disposition chosen when the retry budget runs out: the rename FAILS rather
// than leaving one linker pointing at a title that no longer exists.
//
// The alternative considered was log-and-continue, which trades a loud
// retryable failure for a silent inconsistency. Without this test that choice
// is only a comment — and the comment is exactly the kind that stays true-
// looking after someone changes the code under it.
//
// Exhaustion is forced by lowering the retry bound to 1 rather than by
// arranging three consecutive commits, which would need a per-attempt hook in
// production code. Same disposition, less test-only surface.
func TestUpdateDocument_CascadeExhaustionRollsBackTheWholeRename(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("needs a write to commit inside the cascade's window, which SQLite's BEGIN IMMEDIATE prevents")
	}
	ws := createTestWorkspace(t, s, "CascadeExhaustion")

	const originalTitle = "Epsilon"
	target := createTestDoc(t, s, ws.ID, originalTitle, "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "EpsilonLinker", "before [[Epsilon]] after")

	restore := cascadeRewriteAttempts
	cascadeRewriteAttempts = 1
	defer func() { cascadeRewriteAttempts = restore }()

	var once sync.Once
	edited := "before [[Epsilon]] after — edit that beats the single attempt"
	s.afterLinkCascadeRead = func(string) {
		once.Do(func() {
			if _, err := s.UpdateDocument(linker.ID, models.DocumentUpdate{Content: &edited}); err != nil {
				t.Errorf("concurrent edit: %v", err)
			}
		})
	}
	defer func() { s.afterLinkCascadeRead = nil }()

	newTitle := "Zeta"
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})
	if err == nil {
		t.Fatal("rename succeeded after the cascade exhausted its retries; it must fail rather " +
			"than leave a linker pointing at a title that no longer exists")
	}
	// The error has to be CLASSIFIABLE as contention, not just non-nil: the
	// HTTP layer answers 503 + Retry-After on this sentinel and 500 otherwise,
	// so losing the wrap turns a retryable outcome into "this will never work".
	if !errors.Is(err, ErrLinkCascadeContention) {
		t.Errorf("error does not wrap ErrLinkCascadeContention, so the HTTP layer will report an "+
			"opaque 500 instead of a retryable 503: %v", err)
	}

	// Rollback: the target keeps its original title.
	got, err := s.GetDocument(target.ID)
	if err != nil {
		t.Fatalf("GetDocument(target): %v", err)
	}
	if got.Title != originalTitle {
		t.Errorf("target title = %q, want %q — the failed rename did not roll back", got.Title, originalTitle)
	}

	// And the concurrent editor's text is intact, which is the property this
	// whole unit exists to protect. A rollback that also reverted the OTHER
	// transaction's committed write would be a worse bug than the one fixed.
	link, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("GetDocument(linker): %v", err)
	}
	if link.Content != edited {
		t.Errorf("the concurrent edit did not survive the rolled-back rename.\n got: %q\nwant: %q", link.Content, edited)
	}
}
