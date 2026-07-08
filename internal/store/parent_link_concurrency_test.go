package store

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// requirePostgresForConcurrency skips a test unless a real PostgreSQL backend
// is configured. The parent-link TOCTOU races (BUG-2073) are Postgres-only:
// SQLite serializes every writer via BEGIN IMMEDIATE (_txlock=immediate), so
// the advisory-lock protocol these tests exercise is a no-op there and two
// opposing writers can never actually interleave. Proving the fix therefore
// requires Postgres (make test-pg).
func requirePostgresForConcurrency(t *testing.T) {
	t.Helper()
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: SQLite serializes writers via BEGIN IMMEDIATE, so this race can't manifest")
	}
}

// countParentLinks returns how many `parent` link rows an item is the source
// of. The invariant is that a well-behaved item has at most one — the
// DELETE-then-INSERT in setParentLinkTx must never leave duplicates behind,
// even under concurrent reparenting.
func countParentLinks(t *testing.T, s *Store, itemID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_links
		WHERE source_id = ? AND link_type = 'parent'
	`), itemID).Scan(&n); err != nil {
		t.Fatalf("count parent links for %s: %v", itemID, err)
	}
	return n
}

// TestSetParentLink_ConcurrentOpposingNoCycle exercises BUG-2073 race 1: two
// goroutines run opposing SetParentLink(A,B) and SetParentLink(B,A) on the
// same pair. Before the fix, SetParentLink locked only the old+new parent keys
// and NOT the child's own key, so the two calls locked disjoint keys ({B} vs
// {A}), both cycle walks passed on stale snapshots, and both inserts committed
// — forming an A<->B cycle. With the child (itemID) key folded into the sorted
// lock batch the two calls contend on {A,B}, serialize, and the loser's cycle
// walk (run under the lock) observes the committed edge and is rejected.
//
// The invariant asserted: for every pair, A->B and B->A must NEVER both exist.
func TestSetParentLink_ConcurrentOpposingNoCycle(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	const pairs = 64
	type pair struct{ a, b *models.Item }
	ps := make([]pair, pairs)
	for i := range ps {
		ps[i] = pair{
			a: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("A%d", i), ""),
			b: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("B%d", i), ""),
		}
	}

	// One shared barrier so all 2*pairs goroutines are released together —
	// maximizes the odds that each opposing pair actually interleaves.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var bothSucceeded int64
	for i := range ps {
		p := ps[i]
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.SetParentLink(ws.ID, p.a.ID, p.b.ID, "user")
			_ = err // an error here (cycle rejection) is the EXPECTED loser outcome
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := s.SetParentLink(ws.ID, p.b.ID, p.a.ID, "user")
			_ = err
		}()
	}
	close(start)
	wg.Wait()

	for i, p := range ps {
		aParent, err := s.GetParentForItem(p.a.ID)
		if err != nil {
			t.Fatalf("pair %d GetParentForItem(A): %v", i, err)
		}
		bParent, err := s.GetParentForItem(p.b.ID)
		if err != nil {
			t.Fatalf("pair %d GetParentForItem(B): %v", i, err)
		}
		aToB := aParent != nil && aParent.TargetID == p.b.ID
		bToA := bParent != nil && bParent.TargetID == p.a.ID
		if aToB && bToA {
			atomic.AddInt64(&bothSucceeded, 1)
			t.Errorf("pair %d formed an A<->B cycle: both SetParentLink calls committed (A->B and B->A)", i)
		}
		// Neither item may end up with more than one parent link row.
		if n := countParentLinks(t, s, p.a.ID); n > 1 {
			t.Errorf("pair %d item A has %d parent links, want <=1", i, n)
		}
		if n := countParentLinks(t, s, p.b.ID); n > 1 {
			t.Errorf("pair %d item B has %d parent links, want <=1", i, n)
		}
	}
	if n := atomic.LoadInt64(&bothSucceeded); n > 0 {
		t.Fatalf("%d/%d pairs formed a parent cycle under concurrency (BUG-2073 race 1 not closed)", n, pairs)
	}
}

// TestSetParentLink_ConcurrentReparentSingleParent exercises BUG-2073 race 2:
// many goroutines reparent the SAME child to DIFFERENT parents at once. The
// child's own (itemID) advisory lock, now folded into every SetParentLink
// batch, serializes the DELETE-then-INSERT so the child always ends with
// EXACTLY ONE parent link — no duplicate rows from interleaved inserts, no
// lost link from a delete that raced a concurrent insert. Each reparent
// re-reads the old parent under the child lock before detaching, so it always
// detaches from (and holds the lock for) the child's real current parent.
func TestSetParentLink_ConcurrentReparentSingleParent(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")

	const parentCount = 24
	parents := make([]*models.Item, parentCount)
	validTarget := make(map[string]bool, parentCount)
	for i := range parents {
		parents[i] = createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Parent%d", i), "")
		validTarget[parents[i].ID] = true
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range parents {
		p := parents[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Errors are acceptable under contention (e.g. a transient
			// serialization/deadlock abort); what must hold is the
			// post-state invariant checked below.
			_, _ = s.SetParentLink(ws.ID, child.ID, p.ID, "user")
		}()
	}
	close(start)
	wg.Wait()

	// Exactly one parent link row must remain, pointing at a real parent.
	if n := countParentLinks(t, s, child.ID); n != 1 {
		t.Fatalf("child has %d parent links after concurrent reparenting, want exactly 1", n)
	}
	link, err := s.GetParentForItem(child.ID)
	if err != nil {
		t.Fatalf("GetParentForItem(child): %v", err)
	}
	if link == nil {
		t.Fatal("child has no parent link after concurrent reparenting, want exactly 1")
	}
	if !validTarget[link.TargetID] {
		t.Errorf("child parent link points at %q, which is not one of the reparent targets", link.TargetID)
	}
}
