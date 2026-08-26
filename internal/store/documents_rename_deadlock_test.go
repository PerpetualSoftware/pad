package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2778. A document rename writes rows in two stages inside one
// transaction: updateLinksInTx rewrites every OTHER document whose content
// links the old title, and the final UPDATE writes the renamed document
// itself. Two concurrent renames of documents that link to each other take
// those two row locks in opposite orders, and Postgres aborts one of them
// with SQLSTATE 40P01 — a 500 on an ordinary rename.
//
// This is a MEASURED failure, not a reasoned one. The measurement was a
// separate throwaway probe against the unfixed code, which deadlocked on 12
// of 12 rounds; THIS test is not that probe and does not reproduce the
// figure — it asserts the absence of the failure over 8 rounds against the
// fixed code, and dies to the mutations that remove or misplace the lock.
// The distinction matters because "12 of 12" is evidence from a run nobody
// can repeat by reading this file (codex round 6).
//
// The bug was originally filed (by me) as a lock-ORDERING problem in the
// cascade loop, and reproducing it is what showed that diagnosis to be wrong
// — each transaction's cascade set is a single row, so ordering that loop
// changes nothing. The fix serializes renames per workspace.
//
// POSTGRES ONLY, and skipped loudly elsewhere rather than passing quietly:
// SQLite's single writer cannot produce the cycle, so a green run there is
// not evidence about this fix. Running it on SQLite would add a passing test
// that could never fail, which reads as coverage without being it.
func TestUpdateDocument_ConcurrentMutualRenamesDoNotDeadlock(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("asserts a Postgres row-lock property; SQLite serializes writers and cannot exhibit the cycle")
	}
	ws := createTestWorkspace(t, s, "RenameDeadlock")

	// Several rounds because the interleaving is scheduler-dependent. The
	// start barrier below releases both goroutines together, which makes
	// overlap LIKELY, not certain — nothing here can guarantee two
	// transactions interleave at the statement that matters, which is why
	// this runs rounds rather than asserting on one (codex round 6). Each
	// round uses fresh titles: the (workspace_id, title) unique constraint
	// makes reuse a different error.
	const rounds = 8
	for i := 0; i < rounds; i++ {
		suffix := string(rune('A' + i))
		ta, tb := "Alpha"+suffix, "Beta"+suffix
		a := createTestDoc(t, s, ws.ID, ta, "links to [["+tb+"]]")
		b := createTestDoc(t, s, ws.ID, tb, "links to [["+ta+"]]")

		newA, newB := ta+"-renamed", tb+"-renamed"
		var wg sync.WaitGroup
		errs := make([]error, 2)
		// Start barrier: without it the two renames can run end to end and the
		// round proves nothing (codex round 3). Closing the channel releases
		// both goroutines at the same instant, which makes overlap likely —
		// it cannot make it certain, since nothing here controls when each
		// transaction reaches the statement that takes the second lock.
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errs[0] = s.UpdateDocument(a.ID, models.DocumentUpdate{Title: &newA})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errs[1] = s.UpdateDocument(b.ID, models.DocumentUpdate{Title: &newB})
		}()
		close(start)
		// Bounded wait so a failure reports as THIS test failing rather than
		// as a package-wide timeout (codex round 4). Postgres aborts a
		// detected deadlock within deadlock_timeout — a second by default —
		// so the unfixed code fails fast rather than hanging; the bound is
		// for the case where something holds a lock nothing releases, which
		// is the failure mode with no other signal.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: renames did not finish within 30s — a lock is being held that nothing releases", i)
		}

		for j, err := range errs {
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "deadlock") || strings.Contains(err.Error(), "40P01") {
				t.Fatalf("round %d writer %d deadlocked: %v", i, j, err)
			}
			t.Fatalf("round %d writer %d failed: %v", i, j, err)
		}

		// Both renames must have actually landed — otherwise a fix that
		// serialized by REFUSING one of them would pass the assertion above.
		for _, want := range []struct {
			id, title string
		}{{a.ID, newA}, {b.ID, newB}} {
			got, err := s.GetDocument(want.id)
			if err != nil {
				t.Fatalf("round %d: get document: %v", i, err)
			}
			if got.Title != want.title {
				t.Errorf("round %d: title = %q, want %q — serializing must not drop a rename", i, got.Title, want.title)
			}
		}
	}
}

// TestUpdateDocument_CascadeUsesTheTitleUnderTheLock is the correctness half
// of BUG-2778's fix, and it discriminates a mutation the deadlock test cannot
// see: keeping the re-read but continuing to DECIDE from the pre-lock
// snapshot.
//
// A rename's cascade rewrites `[[oldTitle]]` in every other document. If the
// old title comes from a read taken before the lock, a rename that commits in
// that window makes it the wrong string: the cascade then rewrites a title
// nothing carries any more, and every backlink is left pointing at a title
// that no longer resolves.
func TestUpdateDocument_CascadeUsesTheTitleUnderTheLock(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeTitle")
	target := createTestDoc(t, s, ws.ID, "One", "the subject")
	linker := createTestDoc(t, s, ws.ID, "Linker", "points at [[One]]")

	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		s.afterDocumentPreLockRead = nil
		defer func() { s.afterDocumentPreLockRead = hook }()
		// A competing rename lands inside the window. It rewrites the linker
		// to [[Two]] as it goes, so the pre-read's "One" is now a string no
		// document contains.
		two := "Two"
		if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &two}); err != nil {
			t.Errorf("competing rename: %v", err)
		}
	}
	s.afterDocumentPreLockRead = hook

	three := "Three"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &three}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	s.afterDocumentPreLockRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the pre-lock window was not exercised", fired)
	}

	got, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("get linker: %v", err)
	}
	if !strings.Contains(got.Content, "[[Three]]") {
		t.Errorf("linker content = %q, want a [[Three]] link — the cascade rewrote the title from the pre-lock read instead of the row under the lock, so the backlink was left dangling", got.Content)
	}
}

// TestUpdateDocument_RenameBackToTheStaleTitleStillCascades covers the case a
// mutation exposed: gating the lock (and the re-read) on the PRE-LOCK title
// comparison rather than on a title being supplied at all.
//
// A competing rename takes the document One→Two, rewriting the linker to
// [[Two]] on its way. This request then asks for "One" — which equals the
// title its stale read saw, so a gate written against that read concludes
// "no rename" and skips both the lock and the cascade. The row's title column
// is still written, so the document ends up titled One with every backlink
// pointing at [[Two]]: a rename that silently breaks its own links.
func TestUpdateDocument_RenameBackToTheStaleTitleStillCascades(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RenameBack")
	target := createTestDoc(t, s, ws.ID, "One", "the subject")
	linker := createTestDoc(t, s, ws.ID, "Linker", "points at [[One]]")

	fired := 0
	var hook func(string)
	hook = func(string) {
		if fired > 0 {
			return
		}
		fired++
		s.afterDocumentPreLockRead = nil
		defer func() { s.afterDocumentPreLockRead = hook }()
		two := "Two"
		if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &two}); err != nil {
			t.Errorf("competing rename: %v", err)
		}
	}
	s.afterDocumentPreLockRead = hook

	// The title this request asks for is the one its stale read reported.
	one := "One"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &one}); err != nil {
		t.Fatalf("rename back: %v", err)
	}
	s.afterDocumentPreLockRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1", fired)
	}

	got, err := s.GetDocument(target.ID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got.Title != "One" {
		t.Fatalf("target title = %q, want One", got.Title)
	}
	link, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("get linker: %v", err)
	}
	if !strings.Contains(link.Content, "[[One]]") {
		t.Errorf("linker content = %q, want [[One]] — the document is titled One, so a backlink left at [[Two]] resolves to nothing", link.Content)
	}
}

// TestUpdateDocument_DeletedArchivedBeforeTheLockIsNotRenamed pins the
// EARLY-RETURN half: a delete that lands before the transaction's re-read
// makes that read return nil, and the rename must stop there.
//
// Precise about what it does NOT show (codex round 6): the cascade has not
// run at this point, so this is not evidence that a cascade gets rolled
// back. The rows-affected guard that provides that atomicity is exercised by
// TestUpdateDocument_DeleteLandingBeforeTheWriteIsNotOverwritten, on a path
// where it is the only mechanism.
func TestUpdateDocument_DeletedArchivedBeforeTheLockIsNotRenamed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "DeleteRace")
	target := createTestDoc(t, s, ws.ID, "Doomed", "the subject")
	linker := createTestDoc(t, s, ws.ID, "Linker", "points at [[Doomed]]")

	fired := 0
	s.afterDocumentPreLockRead = func(string) {
		if fired > 0 {
			return
		}
		fired++
		if err := s.DeleteDocument(target.ID); err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	}

	renamed := "Renamed"
	got, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &renamed})
	s.afterDocumentPreLockRead = nil
	if err != nil {
		t.Fatalf("rename over a deleted document: %v", err)
	}
	if got != nil {
		t.Errorf("rename of a soft-deleted document returned %+v, want nil (not found)", got)
	}
	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1", fired)
	}

	link, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("get linker: %v", err)
	}
	if !strings.Contains(link.Content, "[[Doomed]]") {
		t.Errorf("linker content = %q — the cascade committed even though the rename did not", link.Content)
	}
}

// TestDocumentRenameLock_DoesNotContendWithTheSeqLock pins the reason the
// rename key is its own key. A half-fix that reused acquireWorkspaceSeqLock's
// bare workspace key serializes renames just as well and passes every other
// test here (codex round 2) — while also making every rename wait behind every
// item-number and seq write in the workspace, and vice versa. The difference
// is invisible in behaviour and visible only in contention, so this asserts
// contention directly.
//
// Both legs matter: the seq key must be FREE while a rename holds its lock
// (that is the property), and the rename key must be TAKEN (otherwise the
// first leg passes against an implementation that acquires nothing at all).
func TestDocumentRenameLock_DoesNotContendWithTheSeqLock(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("advisory locks are a Postgres construct; the helper is a no-op elsewhere")
	}
	ws := createTestWorkspace(t, s, "LockKeys")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if err := s.acquireWorkspaceDocumentRenameLock(tx, ws.ID); err != nil {
		t.Fatalf("acquire rename lock: %v", err)
	}

	other, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	defer other.Rollback()

	var seqFree bool
	if err := other.QueryRow("SELECT pg_try_advisory_xact_lock(hashtext($1))", ws.ID).Scan(&seqFree); err != nil {
		t.Fatalf("probe seq key: %v", err)
	}
	if !seqFree {
		t.Error("holding the document-rename lock blocked the workspace seq key — renames would serialize against every item write in the workspace")
	}

	var renameFree bool
	if err := other.QueryRow("SELECT pg_try_advisory_xact_lock(hashtext('pad:document-rename:' || $1))", ws.ID).Scan(&renameFree); err != nil {
		t.Fatalf("probe rename key: %v", err)
	}
	if renameFree {
		t.Error("control: the rename key was acquirable while a transaction claims to hold it — the lock is not being taken at all")
	}
}

// TestUpdateDocument_DeleteLandingBeforeTheWriteIsNotOverwritten covers the
// window the previous test cannot reach: a soft-delete that commits AFTER
// this transaction's own read, with the write already prepared. The path
// chosen is a content-only PATCH, which takes no rename lock at all — so the
// rows-affected guard on the final UPDATE is the only thing preventing a
// write to an archived row, and it is tested where it is the sole mechanism
// rather than where another one would mask it.
func TestUpdateDocument_DeleteLandingBeforeTheWriteIsNotOverwritten(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		// SQLite cannot reach this interleaving at all: the open transaction
		// holds the database's single write lock, so the delete inside the
		// seam blocks until busy_timeout and fails. The guard is unreachable
		// there — which is a fact about the engine, not coverage, so the skip
		// says it rather than letting a green SQLite run imply otherwise.
		t.Skip("the interleaving requires MVCC; SQLite's write lock blocks the concurrent delete outright")
	}
	ws := createTestWorkspace(t, s, "DeleteBeforeWrite")
	doc := createTestDoc(t, s, ws.ID, "Subject", "original body")

	fired := 0
	s.afterDocumentPreWrite = func(string) {
		if fired > 0 {
			return
		}
		fired++
		if err := s.DeleteDocument(doc.ID); err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	}

	body := "rewritten body"
	got, err := s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &body})
	s.afterDocumentPreWrite = nil
	if err != nil {
		t.Fatalf("update over a deleted document: %v", err)
	}
	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1", fired)
	}
	if got != nil {
		t.Errorf("update of a soft-deleted document returned %+v, want nil (not found)", got)
	}

	// Read the archived row directly: GetDocument filters it out, so it
	// cannot show whether the write landed on it.
	var content string
	var deletedAt *string
	if err := s.db.QueryRow(s.q(`SELECT content, deleted_at FROM documents WHERE id = ?`), doc.ID).Scan(&content, &deletedAt); err != nil {
		t.Fatalf("read archived row: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("document is not archived; the test did not exercise the race")
	}
	if content != "original body" {
		t.Errorf("archived row content = %q, want it untouched — the update wrote over a document that had already been deleted", content)
	}
}

// TestUpdateDocument_RenameWaitIsBounded pins the lock timeout. Without it a
// rename waiting on the workspace lock holds its pool connection for as long
// as the holder runs, which is how lock contention becomes pool exhaustion —
// the failure mode is a stalled server, not an error, so nothing surfaces.
//
// A held lock is manufactured directly (a transaction that takes the key and
// sits on it), because that is the only way to make the wait deterministic;
// what is asserted is that the waiting rename FAILS, in bounded time, with
// the SQLSTATE the handler maps to a retryable 503.
func TestUpdateDocument_RenameWaitIsBounded(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("advisory locks and lock_timeout are Postgres constructs; the helper is a no-op elsewhere")
	}
	ws := createTestWorkspace(t, s, "BoundedWait")
	doc := createTestDoc(t, s, ws.ID, "Held", "body")

	holder, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback()
	if err := s.acquireWorkspaceDocumentRenameLock(holder, ws.ID); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		title := "Renamed while blocked"
		_, rerr := s.UpdateDocument(doc.ID, models.DocumentUpdate{Title: &title})
		done <- result{err: rerr, elapsed: time.Since(started)}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("the blocked rename succeeded while another transaction held the lock")
		}
		if !strings.Contains(r.err.Error(), "55P03") {
			t.Errorf("blocked rename failed with %v, want SQLSTATE 55P03 (lock_not_available) — that is what the handler maps to a retryable 503", r.err)
		}
		// The bound is 5s; allow slack for scheduling, but fail if the wait
		// was effectively unbounded.
		if r.elapsed > 20*time.Second {
			t.Errorf("blocked rename took %s — the lock_timeout did not bound the wait", r.elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("blocked rename never returned — the wait is unbounded and its pool connection is pinned for the holder's lifetime")
	}
}
