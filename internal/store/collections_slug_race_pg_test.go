package store

import (
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2885 (IDEA-2874 + BUG-2875). A collection's slug is an identifier and a
// mutable name at once, and two writers used to allocate it with no ordering
// between them:
//
//   - UpdateCollection scanned for a free slug on the POOL before its
//     transaction opened, so two renames deriving the same base both picked the
//     same slug and the loser's UPDATE failed on the UNIQUE index (IDEA-2874).
//   - CreateCollection took no lock at all, so a create could land between the
//     rename's FOR UPDATE scan of EXISTING sibling rows (which cannot see a row
//     that does not exist yet) and its commit (BUG-2875).
//
// Both are closed the same way: allocation moves under the transaction, and
// both paths take the workspace advisory lock FIRST — the order item-create and
// rename already share. These tests were written BEFORE the fix (team CONVE-29)
// and measured failing against it; see the trail for the run.
//
// "Blocked" is verified in the database via pg_stat_activity (waitForLockWait,
// attachments_live_item_test.go), never by elapsed time. The needle is the
// advisory-lock call itself: on the unfixed tree the create does NOT wait
// there — it has already chosen `gamma` and blocks on the UNIQUE index inside
// its INSERT — so a needle on `INSERT INTO collections` would pass on both
// sides and measure nothing.
//
// SQLite is excluded rather than skipped for convenience: its DSN sets
// _txlock=immediate, so the second writer cannot even open its transaction
// while the first is live — the interleaving these tests construct is
// unrepresentable there. The dialect-agnostic instrument for the same defects
// is TestConcurrentSlugWritersNeverCollide below.

func slugRacePGStore(t *testing.T) (*Store, string, *models.Collection) {
	t.Helper()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — the advisory lock only exists on Postgres")
	}
	s := testStorePostgres(t, pgURL)
	ws := createTestWorkspace(t, s, "SlugRace")
	alpha, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	return s, ws.ID, alpha
}

// holdUncommittedRename opens a transaction that looks exactly like a rename
// in flight: workspace lock held, the row's slug already rewritten, nothing
// committed. The caller commits (or rolls back) it.
func holdUncommittedRename(t *testing.T, s *Store, wsID, collID, newSlug string) *sql.Tx {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin rename tx: %v", err)
	}
	if err := s.acquireWorkspaceSeqLock(tx, wsID); err != nil {
		tx.Rollback()
		t.Fatalf("acquire workspace lock: %v", err)
	}
	if _, err := tx.Exec(s.q(`UPDATE collections SET slug = ?, updated_at = ? WHERE id = ?`), newSlug, nowNano(), collID); err != nil {
		tx.Rollback()
		t.Fatalf("rename in tx: %v", err)
	}
	return tx
}

// BUG-2875: a create must queue behind an in-flight rename, not slip between
// its scan and its commit.
func TestCreateCollectionWaitsForAnUncommittedRename(t *testing.T) {
	s, wsID, alpha := slugRacePGStore(t)

	tx := holdUncommittedRename(t, s, wsID, alpha.ID, "gamma")
	defer tx.Rollback() // no-op after Commit

	type result struct {
		coll *models.Collection
		err  error
	}
	done := make(chan error, 1)
	results := make(chan result, 1)
	go func() {
		// The relation names the slug the rename is moving AWAY from. On this
		// tree it lands dangling — refusing a relation target that names no
		// collection is TASK-2878's (U1), not this unit's. What this unit owes
		// is the ORDER: the create runs after the rename is committed, so that
		// refusal, once it exists, sees the committed state and not a
		// snapshot from before it.
		c, err := s.CreateCollection(wsID, models.CollectionCreate{
			Name: "Gamma", Schema: relationSchema(t, "alpha_ref", "alpha"),
		})
		results <- result{coll: c, err: err}
		done <- err
	}()

	waitForLockWait(t, s, "pg_advisory_xact_lock", done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rename: %v", err)
	}
	r := <-results
	if r.err != nil {
		t.Fatalf("create after the rename committed: %v", r.err)
	}
	// Allocated AFTER the rename's `gamma` became visible, so the create
	// stepped around it. Unfixed, the pool scan ran before the commit, chose
	// `gamma`, and the INSERT failed on the UNIQUE index once the rename landed.
	if r.coll.Slug != "gamma-2" {
		t.Fatalf("created slug = %q, want gamma-2 (allocated after the rename committed)", r.coll.Slug)
	}
	if got := relationTargetOf(t, s, r.coll.ID, "alpha_ref"); got != "alpha" {
		t.Fatalf("relation target = %q, want the literal alpha the caller supplied (repointing a create's own schema is not this unit's)", got)
	}
}

// IDEA-2874: a rename must allocate its new slug under the lock, so two
// renames deriving the same slug serialize into `gamma` and `gamma-2` rather
// than one of them failing on the UNIQUE index.
func TestRenameAllocatesItsSlugUnderTheWorkspaceLock(t *testing.T) {
	s, wsID, alpha := slugRacePGStore(t)
	beta, err := s.CreateCollection(wsID, models.CollectionCreate{Name: "Beta", Slug: "beta"})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	tx := holdUncommittedRename(t, s, wsID, alpha.ID, "gamma")
	defer tx.Rollback() // no-op after Commit

	type result struct {
		coll *models.Collection
		err  error
	}
	done := make(chan error, 1)
	results := make(chan result, 1)
	go func() {
		name := "Gamma"
		c, err := s.UpdateCollection(beta.ID, models.CollectionUpdate{Name: &name})
		results <- result{coll: c, err: err}
		done <- err
	}()

	// Both trees block here — the unfixed rename ALSO takes the workspace lock,
	// it just allocated before reaching it. The discriminating assertion is
	// the slug it ends up with.
	waitForLockWait(t, s, "pg_advisory_xact_lock", done)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rename: %v", err)
	}
	r := <-results
	if r.err != nil {
		t.Fatalf("second rename after the first committed: %v", r.err)
	}
	if r.coll.Slug != "gamma-2" {
		t.Fatalf("renamed slug = %q, want gamma-2 (allocated under the lock, after the first rename's gamma was visible)", r.coll.Slug)
	}
}

// TestConcurrentSlugWritersNeverCollide is the dialect-agnostic instrument for
// the same two defects. Each round releases THREE writers at a barrier — two
// renames and one create — all deriving the same base slug, and asserts every
// writer succeeded and the three slugs are distinct. Unfixed, the scans run
// outside any lock on both dialects (SQLite's BEGIN IMMEDIATE serializes the
// WRITE, not a pool read issued before it), so two writers pick the same slug
// and one fails on the UNIQUE index.
//
// Repeated and barrier-released for the reason the deadlock test states: a
// single unsynchronised trio almost never interleaves at the point that
// matters. Measured failing on the unfixed tree before the fix landed — the
// count is on the trail, not recalled here.
func TestConcurrentSlugWritersNeverCollide(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "SlugCollide")

	a, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	b, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Beta", Slug: "beta"})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	const rounds = 40
	for round := 0; round < rounds; round++ {
		name := fmt.Sprintf("Same R%d", round)
		start := make(chan struct{})
		slugs := make(chan string, 3)
		errs := make(chan error, 3)
		var wg sync.WaitGroup

		rename := func(id string) {
			defer wg.Done()
			<-start
			n := name
			c, err := s.UpdateCollection(id, models.CollectionUpdate{Name: &n})
			if err != nil {
				errs <- err
				return
			}
			slugs <- c.Slug
		}
		wg.Add(3)
		go rename(a.ID)
		go rename(b.ID)
		go func() {
			defer wg.Done()
			<-start
			c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: name})
			if err != nil {
				errs <- err
				return
			}
			slugs <- c.Slug
		}()
		close(start)

		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Fatalf("round %d: writers did not complete within 20s", round)
		}
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: a slug writer failed: %v", round, err)
		}
		close(slugs)
		seen := map[string]bool{}
		for sl := range slugs {
			if seen[sl] {
				t.Fatalf("round %d: two writers were handed the same slug %q", round, sl)
			}
			seen[sl] = true
		}
		if len(seen) != 3 {
			t.Fatalf("round %d: %d distinct slugs, want 3", round, len(seen))
		}
	}
}

// TestCollectionSlugWritersDoNoPoolIOUnderTheLock is the guard for the
// invariant the fix introduces: everything CreateCollection and a renaming
// UpdateCollection read while holding the workspace lock goes through the
// transaction, never the pool. A pool read from inside a lock-holding
// transaction needs a SECOND connection, and when the pool is saturated by
// callers waiting on that very lock the second connection never arrives — a
// deadlock in the application that no SQLSTATE names (BUG-2409's class). With
// MaxOpenConns(1) the transaction owns the only connection, so ANY pool read
// inside the critical section deadlocks deterministically here instead of
// only under load. Works identically on both dialects.
//
// This one passes on the unfixed tree too (the pre-fix scans ran BEFORE the
// transaction opened, which is the ordering defect, not this one). Its
// negative control is the mutation matrix on the trail: handing `s.db` to
// either allocation, or issuing the INSERT on the pool, hangs it.
//
// Every optional leg of the rename is armed: a relation sibling so
// retargetRelationFieldsTx runs, and a field migration so
// applyFieldMigrationsTx runs — an unarmed branch is invisible to the sweep.
func TestCollectionSlugWritersDoNoPoolIOUnderTheLock(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NoPoolIO")

	alpha, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Alpha", Slug: "alpha",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`,
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Beta", Slug: "beta", Schema: relationSchema(t, "alpha_ref", "alpha"),
	}); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, alpha.ID, models.ItemCreate{Title: "Migrate me", Fields: `{"status":"open"}`}); err != nil {
		t.Fatalf("seed field value: %v", err)
	}

	// From here on the pool has exactly one connection: the transaction's.
	s.db.SetMaxOpenConns(1)

	run := func(label string, op func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- op() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not complete under MaxOpenConns(1): a read inside the "+
				"lock-holding transaction went to the pool and is waiting for a "+
				"connection the transaction itself holds", label)
		}
	}

	run("CreateCollection", func() error {
		_, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Gamma"})
		return err
	})
	run("UpdateCollection(rename + migration + relation sibling)", func() error {
		name := "Alpha Prime"
		schema := `{"fields":[{"key":"status","type":"select","options":["todo","done"]}]}`
		_, err := s.UpdateCollection(alpha.ID, models.CollectionUpdate{
			Name:   &name,
			Schema: &schema,
			Migrations: []models.FieldMigration{
				{Field: "status", RenameOptions: map[string]string{"open": "todo"}},
			},
		})
		return err
	})

	// The rename's sibling repoint ran under the same single connection.
	if got := relationTargetOf(t, s, mustCollectionBySlug(t, s, ws.ID, "beta").ID, "alpha_ref"); got != "alpha-prime" {
		t.Fatalf("relation target = %q, want alpha-prime", got)
	}
}

func mustCollectionBySlug(t *testing.T, s *Store, wsID, slug string) *models.Collection {
	t.Helper()
	c, err := s.GetCollectionBySlug(wsID, slug)
	if err != nil {
		t.Fatalf("GetCollectionBySlug(%s): %v", slug, err)
	}
	if c == nil {
		t.Fatalf("collection %q missing", slug)
	}
	return c
}
