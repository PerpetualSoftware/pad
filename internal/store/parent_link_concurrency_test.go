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

// parentChainCycles walks the `parent` edges up from startID and reports
// whether the chain revisits a node (a cycle) within a bounded number of hops.
// The bound doubles as a safety net so a genuine cycle can't spin the walk
// forever. Reads committed state directly (post-concurrency assertion).
func parentChainCycles(t *testing.T, s *Store, startID string, maxHops int) bool {
	t.Helper()
	visited := map[string]bool{}
	current := startID
	for hops := 0; hops <= maxHops; hops++ {
		if visited[current] {
			return true
		}
		visited[current] = true
		var target string
		err := s.db.QueryRow(s.q(`
			SELECT target_id FROM item_links
			WHERE source_id = ? AND link_type = 'parent'
			LIMIT 1
		`), current).Scan(&target)
		if err != nil || target == "" {
			return false // no parent — chain terminates, no cycle
		}
		current = target
	}
	// Ran past maxHops without terminating: treat as a cycle.
	return true
}

// TestSetParentLink_ConcurrentNHopNoCycle exercises BUG-2074: an N-hop parent
// cycle closed via an edge on an item that NEITHER endpoint of a concurrent
// write locks. The BUG-2073 fix folds the child key into a per-endpoint sorted
// lock batch, but that only covers the two endpoints of the edge being written.
//
// Setup per quad: two committed 2-node chains A→B and C→D. Then two goroutines
// race to cross-link them:
//
//	SetParentLink(B, C)  — B's parent becomes C  (lock set {B, C})
//	SetParentLink(D, A)  — D's parent becomes A  (lock set {D, A})
//
// The two lock sets are DISJOINT, so under the per-endpoint scheme alone both
// cycle walks pass on stale snapshots and both inserts commit — forming
// A→B→C→D→A. The workspace-scoped parent-link cycle lock (BUG-2074) serializes
// the two adds so the loser's walk sees the committed edge and is rejected.
//
// Invariant asserted: after both goroutines finish, NO node's parent chain
// forms a cycle, and the two closing edges never BOTH exist.
func TestSetParentLink_ConcurrentNHopNoCycle(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	const quads = 64
	type quad struct{ a, b, c, d *models.Item }
	qs := make([]quad, quads)
	for i := range qs {
		q := quad{
			a: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("A%d", i), ""),
			b: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("B%d", i), ""),
			c: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("C%d", i), ""),
			d: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("D%d", i), ""),
		}
		// Commit the two base chains BEFORE the race: A→B and C→D.
		if _, err := s.SetParentLink(ws.ID, q.a.ID, q.b.ID, "user"); err != nil {
			t.Fatalf("quad %d seed A→B: %v", i, err)
		}
		if _, err := s.SetParentLink(ws.ID, q.c.ID, q.d.ID, "user"); err != nil {
			t.Fatalf("quad %d seed C→D: %v", i, err)
		}
		qs[i] = q
	}

	// One shared barrier so every closing-edge pair is released together,
	// maximizing the odds the disjoint-lock adds actually interleave.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range qs {
		q := qs[i]
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// B's parent → C. A cycle rejection here is the EXPECTED loser outcome.
			_, _ = s.SetParentLink(ws.ID, q.b.ID, q.c.ID, "user")
		}()
		go func() {
			defer wg.Done()
			<-start
			// D's parent → A. Likewise, a rejection is acceptable.
			_, _ = s.SetParentLink(ws.ID, q.d.ID, q.a.ID, "user")
		}()
	}
	close(start)
	wg.Wait()

	var cycles int64
	for i, q := range qs {
		// Walk from every node in the quad — a cycle is reachable from any of them.
		for _, item := range []*models.Item{q.a, q.b, q.c, q.d} {
			if parentChainCycles(t, s, item.ID, 8) {
				atomic.AddInt64(&cycles, 1)
				t.Errorf("quad %d formed an N-hop parent cycle (reachable from %s)", i, item.ID)
				break
			}
		}
		// The two closing edges must never both exist (that IS the cycle).
		bParent, err := s.GetParentForItem(q.b.ID)
		if err != nil {
			t.Fatalf("quad %d GetParentForItem(B): %v", i, err)
		}
		dParent, err := s.GetParentForItem(q.d.ID)
		if err != nil {
			t.Fatalf("quad %d GetParentForItem(D): %v", i, err)
		}
		bToC := bParent != nil && bParent.TargetID == q.c.ID
		dToA := dParent != nil && dParent.TargetID == q.a.ID
		if bToC && dToA {
			t.Errorf("quad %d: both closing edges committed (B→C and D→A) — cycle", i)
		}
	}
	if n := atomic.LoadInt64(&cycles); n > 0 {
		t.Fatalf("%d/%d quads formed an N-hop parent cycle under concurrency (BUG-2074 not closed)", n, quads)
	}
}

// TestCreateItemLink_ConcurrentNHopNoCycle covers the second parent-edge adder
// (BUG-2074 / Codex round-1): CreateItemLink with link_type="parent" writes the
// same graph edge SetParentLink does and is the only other path that can close
// a cycle, yet it neither ran a cycle check nor joined the workspace cycle
// serialization. Here the B→C closing edge is written via CreateItemLink(parent)
// while D→A goes through SetParentLink — proving the two paths serialize on the
// shared workspace cycle lock and CreateItemLink's own cycle check rejects the
// loser, so A→B→C→D→A can't form across the two APIs.
func TestCreateItemLink_ConcurrentNHopNoCycle(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	const quads = 64
	type quad struct{ a, b, c, d *models.Item }
	qs := make([]quad, quads)
	for i := range qs {
		q := quad{
			a: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("A%d", i), ""),
			b: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("B%d", i), ""),
			c: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("C%d", i), ""),
			d: createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("D%d", i), ""),
		}
		if _, err := s.SetParentLink(ws.ID, q.a.ID, q.b.ID, "user"); err != nil {
			t.Fatalf("quad %d seed A→B: %v", i, err)
		}
		if _, err := s.SetParentLink(ws.ID, q.c.ID, q.d.ID, "user"); err != nil {
			t.Fatalf("quad %d seed C→D: %v", i, err)
		}
		qs[i] = q
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range qs {
		q := qs[i]
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// B's parent → C, via the item-link API (link_type=parent).
			_, _ = s.CreateItemLink(ws.ID, models.ItemLinkCreate{
				TargetID: q.c.ID,
				LinkType: models.ItemLinkTypeParent,
			}, q.b.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			// D's parent → A, via SetParentLink.
			_, _ = s.SetParentLink(ws.ID, q.d.ID, q.a.ID, "user")
		}()
	}
	close(start)
	wg.Wait()

	var cycles int64
	for i, q := range qs {
		for _, item := range []*models.Item{q.a, q.b, q.c, q.d} {
			if parentChainCycles(t, s, item.ID, 8) {
				atomic.AddInt64(&cycles, 1)
				t.Errorf("quad %d formed an N-hop parent cycle (reachable from %s)", i, item.ID)
				break
			}
		}
	}
	if n := atomic.LoadInt64(&cycles); n > 0 {
		t.Fatalf("%d/%d quads formed an N-hop cycle across CreateItemLink+SetParentLink (BUG-2074 not closed)", n, quads)
	}
}

// TestCreateItemLink_ParentSingleParentAndCycle asserts CreateItemLink with
// link_type="parent" now behaves like SetParentLink (BUG-2074 / Codex round-2):
// it enforces the single-parent invariant (DELETE-then-INSERT, not append) and
// runs cycle detection. Both were absent before — CreateItemLink appended a raw
// parent row with no cycle check, so a child could accumulate multiple parents
// and the cycle walk (which follows one arbitrary parent per source) could miss
// a cycle. Dialect-agnostic: the invariant holds on SQLite and Postgres alike.
func TestCreateItemLink_ParentSingleParentAndCycle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")
	p1 := createTestItem(t, s, ws.ID, col.ID, "P1", "")
	p2 := createTestItem(t, s, ws.ID, col.ID, "P2", "")

	// First parent via CreateItemLink(parent).
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: p1.ID, LinkType: models.ItemLinkTypeParent}, child.ID); err != nil {
		t.Fatalf("CreateItemLink child→p1: %v", err)
	}
	// Second parent via CreateItemLink(parent) must REPLACE, not append.
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: p2.ID, LinkType: models.ItemLinkTypeParent}, child.ID); err != nil {
		t.Fatalf("CreateItemLink child→p2: %v", err)
	}
	if n := countParentLinks(t, s, child.ID); n != 1 {
		t.Fatalf("child has %d parent links via CreateItemLink, want exactly 1 (single-parent invariant)", n)
	}
	link, err := s.GetParentForItem(child.ID)
	if err != nil {
		t.Fatalf("GetParentForItem(child): %v", err)
	}
	if link == nil || link.TargetID != p2.ID {
		t.Fatalf("child's parent = %v, want p2 (latest wins)", link)
	}

	// Cycle rejection: child→p2 exists, so p2→child would close a 1-hop cycle.
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: child.ID, LinkType: models.ItemLinkTypeParent}, p2.ID); err == nil {
		t.Fatal("CreateItemLink(p2→child) should have been rejected as a cycle, got nil error")
	}
}
