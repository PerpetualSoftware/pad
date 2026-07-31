package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Postgres-only tests for CopyItemAcrossWorkspaces — PLAN-2357 / TASK-2363.
//
// These are the tests DR-9 says the lock ordering needs in order to be a
// decision rather than a comment. On SQLite they cannot fail: BEGIN IMMEDIATE
// serializes every writer, so two opposing copies never interleave and there
// is no advisory lock to order. `make test-pg` is where they run.

// TestCopyItemAcrossWorkspaces_OpposingDirectionsDoNotDeadlock drives A->B and
// B->A copies simultaneously, repeatedly.
//
// This is the test that FAILS without the sorted lock acquisition. With an
// unordered (or ID-sorted, which does not order the hashes) acquisition, one
// goroutine holds hashtext(A) and waits on hashtext(B) while the other holds
// hashtext(B) and waits on hashtext(A); Postgres detects the cycle and aborts
// one transaction with SQLSTATE 40P01. With the keys sorted, both transactions
// request the same key first, so the cycle cannot form.
func TestCopyItemAcrossWorkspaces_OpposingDirectionsDoNotDeadlock(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Deadlock A")
	wsB := createTestWorkspace(t, s, "Deadlock B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks A")
	colB := createTestCollection(t, s, wsB.ID, "Tasks B")

	const rounds = 40
	sourcesA := make([]*models.Item, rounds)
	sourcesB := make([]*models.Item, rounds)
	for i := 0; i < rounds; i++ {
		sourcesA[i] = createTestItem(t, s, wsA.ID, colA.ID, fmt.Sprintf("A-%d", i), "body")
		sourcesB[i] = createTestItem(t, s, wsB.ID, colB.ID, fmt.Sprintf("B-%d", i), "body")
	}

	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	start := make(chan struct{})

	run := func(src *models.Item, targetWS, targetCol string) {
		defer wg.Done()
		<-start
		_, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{
			SourceItemID:       src.ID,
			TargetWorkspaceID:  targetWS,
			TargetCollectionID: targetCol,
			Actor:              "deadlock-actor",
		})
		if err != nil {
			errs <- err
		}
	}

	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go run(sourcesA[i], wsB.ID, colB.ID) // A -> B
		go run(sourcesB[i], wsA.ID, colA.ID) // B -> A
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if isDeadlockError(err) {
			t.Fatalf("opposing cross-workspace copies deadlocked: %v", err)
		}
		t.Fatalf("opposing cross-workspace copy failed: %v", err)
	}

	// Every copy landed. A deadlock aborts a transaction, so a silent loss
	// would show up here even if the error channel were somehow drained.
	for _, ws := range []struct {
		id   string
		want int
	}{{wsA.ID, rounds * 2}, {wsB.ID, rounds * 2}} {
		if got := countItemsIn(t, s, ws.id); got != ws.want {
			t.Errorf("workspace %s has %d items, want %d", ws.id, got, ws.want)
		}
	}
}

// TestCopyItemAcrossWorkspaces_OpposingMovesDoNotDeadlock is the sibling that
// fails when the outer workspace acquisition is REMOVED, rather than merely
// mis-ordered.
//
// Two things have to line up for that to be a real test, and getting either
// wrong makes it vacuous:
//
//   - The copies must be MOVES. A plain copy writes in one workspace only, so
//     its transaction naturally takes exactly one workspace advisory lock (the
//     destination's, inside createItemTxWithID) and no AB/BA cycle can form
//     without the outer acquisition. A move writes in both: the create takes
//     B's key, the archive takes A's.
//   - The two directions must use DISJOINT collection pairs. lockCollectionRows
//     is a second, independent ordering guard — it takes both collection rows
//     FOR UPDATE in sorted ID order, so two copies whose collection sets
//     OVERLAP are already serialized by that alone and cannot deadlock however
//     the workspace locks are taken. Verified empirically: with overlapping
//     collections, deleting the outer acquisition does NOT deadlock. Give each
//     direction its own source and destination collection and the collection
//     locks stop overlapping, leaving the workspace keys as the only ordering.
//
// The two Postgres deadlock tests therefore prove different halves: this one
// proves the outer acquisition must EXIST, and the plain-copy one above proves
// it must be SORTED BY LOCK KEY (an unsorted acquisition deadlocks there even
// though every transaction takes both keys).
func TestCopyItemAcrossWorkspaces_OpposingMovesDoNotDeadlock(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Move Deadlock A")
	wsB := createTestWorkspace(t, s, "Move Deadlock B")
	// One collection pair per direction, so the FOR UPDATE collection locks of
	// the two directions do not overlap.
	colAOut := createTestCollection(t, s, wsA.ID, "A Outbound")
	colBIn := createTestCollection(t, s, wsB.ID, "B Inbound")
	colBOut := createTestCollection(t, s, wsB.ID, "B Outbound")
	colAIn := createTestCollection(t, s, wsA.ID, "A Inbound")

	const rounds = 40
	sourcesA := make([]*models.Item, rounds)
	sourcesB := make([]*models.Item, rounds)
	for i := 0; i < rounds; i++ {
		sourcesA[i] = createTestItem(t, s, wsA.ID, colAOut.ID, fmt.Sprintf("mA-%d", i), "body")
		sourcesB[i] = createTestItem(t, s, wsB.ID, colBOut.ID, fmt.Sprintf("mB-%d", i), "body")
	}

	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	start := make(chan struct{})

	run := func(src *models.Item, targetWS, targetCol string) {
		defer wg.Done()
		<-start
		_, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{
			SourceItemID:       src.ID,
			TargetWorkspaceID:  targetWS,
			TargetCollectionID: targetCol,
			Actor:              "move-actor",
			ArchiveSource:      true,
		})
		if err != nil {
			errs <- err
		}
	}

	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go run(sourcesA[i], wsB.ID, colBIn.ID) // A -> B, archiving in A
		go run(sourcesB[i], wsA.ID, colAIn.ID) // B -> A, archiving in B
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if isDeadlockError(err) {
			t.Fatalf("opposing cross-workspace MOVES deadlocked: %v", err)
		}
		t.Fatalf("opposing cross-workspace move failed: %v", err)
	}

	// Every move landed: each workspace archived its own `rounds` sources and
	// received `rounds` arrivals, so the live count is unchanged.
	for _, id := range []string{wsA.ID, wsB.ID} {
		if got := countItemsIn(t, s, id); got != rounds {
			t.Errorf("workspace %s has %d live items, want %d", id, got, rounds)
		}
	}
}

// TestCopyItemAcrossWorkspaces_ConcurrentCopiesCannotJointlyExceedQuota is the
// DR-16 in-tx claim: two copies each individually under the cap but jointly
// over it must not both commit.
//
// What actually makes it hold is the DESTINATION WORKSPACE LOCK, taken before
// the check: the second copy's transaction blocks on it until the first
// commits, so its COUNT observes the committed row. Stated plainly because it
// bounds what this test proves — a pool-based CheckLimit would pass it too,
// under that lock. CheckLimitTx is the belt-and-braces half (it additionally
// sees the transaction's OWN uncommitted inserts, which matters the moment
// this operation ever creates more than one item), and
// TestCheckLimitTx_SeesUncommittedRowsInTheTransaction is what proves that
// property directly.
//
// The failure this test genuinely catches is the check moving OUTSIDE the
// transaction, or before the destination lock is acquired — either of which
// lets both copies read the same pre-copy count and both commit.
func TestCopyItemAcrossWorkspaces_ConcurrentCopiesCannotJointlyExceedQuota(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	owner := createTestUser(t, s, "joint-quota@example.com", "Owner", "s3cret")
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := s.SetUserPlanOverrides(owner.ID, `{"items_per_workspace": 1}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	wsA, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Joint Source", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(A): %v", err)
	}
	wsB, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Joint Dest", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(B): %v", err)
	}
	colA := createTestCollection(t, s, wsA.ID, "Tasks A")
	colB := createTestCollection(t, s, wsB.ID, "Tasks B")

	src1 := createTestItem(t, s, wsA.ID, colA.ID, "One", "body")
	src2 := createTestItem(t, s, wsA.ID, colA.ID, "Two", "body")

	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, src := range []*models.Item{src1, src2} {
		wg.Add(1)
		go func(src *models.Item) {
			defer wg.Done()
			<-start
			_, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{
				SourceItemID:       src.ID,
				TargetWorkspaceID:  wsB.ID,
				TargetCollectionID: colB.ID,
				Actor:              owner.ID,
				EnforceItemLimit:   true,
			})
			results <- err
		}(src)
	}
	close(start)
	wg.Wait()
	close(results)

	var ok, rejected int
	for err := range results {
		switch {
		case err == nil:
			ok++
		default:
			var limitErr *ItemLimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("unexpected error: %v", err)
			}
			rejected++
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("got %d successes and %d quota rejections, want exactly 1 of each", ok, rejected)
	}
	if got := countItemsIn(t, s, wsB.ID); got != 1 {
		t.Errorf("destination has %d items, want 1 (the cap)", got)
	}
}

// TestAcquireWorkspaceLocksOrdered_CollidingKeysTakeOneLock is the dedup half
// of the DR-9 contract, end to end.
//
// hashtext is a 32-bit hash, so two distinct workspace IDs CAN collide onto
// one lock key. The test finds a real colliding pair by brute force in the
// database (a birthday search over ~300k candidates; hashtext's own output is
// the only source of truth for what collides), creates two workspaces with
// those IDs, and asserts that acquiring both workspaces' locks yields ONE key.
func TestAcquireWorkspaceLocksOrdered_CollidingKeysTakeOneLock(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	idA, idB := findHashtextCollision(t, s)

	// Two real workspaces carrying the colliding IDs. CreateWorkspace mints
	// its own id, so these are inserted directly.
	for i, id := range []string{idA, idB} {
		if _, err := s.db.Exec(s.q(`
			INSERT INTO workspaces (id, name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`), id, fmt.Sprintf("Collide %d", i), fmt.Sprintf("collide-%d-%s", i, newID()[:8]), now(), now()); err != nil {
			t.Fatalf("insert colliding workspace: %v", err)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // test cleanup

	keys, err := s.acquireWorkspaceLocksOrdered(tx, idA, idB)
	if err != nil {
		t.Fatalf("acquireWorkspaceLocksOrdered: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("colliding workspaces produced %d lock keys (%v), want 1", len(keys), keys)
	}

	// Sanity: a NON-colliding pair still yields two.
	other := createTestWorkspace(t, s, "Distinct")
	keys2, err := s.acquireWorkspaceLocksOrdered(tx, idA, other.ID)
	if err != nil {
		t.Fatalf("acquireWorkspaceLocksOrdered (distinct): %v", err)
	}
	if len(keys2) != 2 {
		t.Fatalf("distinct workspaces produced %d lock keys, want 2", len(keys2))
	}
	if keys2[0] >= keys2[1] {
		t.Errorf("lock keys %v are not in ascending order", keys2)
	}
}

// findHashtextCollision brute-forces two distinct UUID-shaped strings that
// hashtext maps to the same 32-bit value. Expected collisions over 300k
// candidates is ~10, so a miss is vanishingly unlikely; the test skips rather
// than fails if the search comes up empty, since an empty search proves
// nothing about the code under test.
func findHashtextCollision(t *testing.T, s *Store) (string, string) {
	t.Helper()
	var a, b string
	err := s.db.QueryRow(`
		WITH candidates AS (
			SELECT gen_random_uuid()::text AS id FROM generate_series(1, 300000)
		)
		SELECT MIN(id), MAX(id)
		FROM candidates
		GROUP BY hashtext(id)
		HAVING COUNT(*) > 1 AND MIN(id) <> MAX(id)
		LIMIT 1
	`).Scan(&a, &b)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			t.Skip("no hashtext collision found in 300k candidates (expected ~10; retry)")
		}
		t.Fatalf("hashtext collision search: %v", err)
	}
	if a == b {
		t.Skip("degenerate collision pair")
	}
	return a, b
}
