package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Postgres-only tests for CopyItemAcrossWorkspaces — PLAN-2357 / TASK-2363,
// hardened by TASK-2372.
//
// These are the tests DR-9 says the lock ordering needs in order to be a
// decision rather than a comment. On SQLite they cannot fail: BEGIN IMMEDIATE
// serializes every writer, so two opposing copies never interleave and there
// is no advisory lock to order. `make test-pg` is where they run.
//
// --- Why these tests contend BY CONSTRUCTION (TASK-2372) --------------------
//
// The original shape was `close(start)` and hope: nothing established that the
// two transactions were ever inside the contested region at the same time, so
// a run in which they happened to execute serially passed just as green. That
// is a test that catches the regression by TIMING, which is the property that
// rots silently.
//
// The obvious fix — a rendezvous barrier immediately AFTER the first lock —
// cannot work here and would hang a CORRECT implementation. The sorted
// acquisition means both transactions request the SAME key first: transaction
// 1 takes it and waits at the barrier, transaction 2 blocks on the lock and
// never arrives (PLAN-2373, Phase D round 1).
//
// So the rendezvous is external and BELOW the code under test:
// advisoryLockGate holds the very workspace advisory keys the copy protocol
// acquires. Both copies therefore block on their FIRST lock request, whatever
// and wherever it is, and the gate does not release until pg_locks shows both
// of them queued as ungranted waiters. That is stronger than "both goroutines
// started" — it is "both transactions are open and parked in the contested
// region", verified in the database rather than assumed.
//
// Determinism follows from Postgres's FIFO lock queues:
//
//   - CORRECT (sorted) acquisition: both copies queue on the same min key.
//     The gate releases, one is granted, takes the second key uncontended,
//     commits; the other then proceeds. No cycle, no hang. This is the case
//     the after-the-first-lock design would have deadlocked.
//   - BROKEN acquisition (removed or mis-sorted): the two copies queue on
//     DIFFERENT keys. Both are already queued when the gate releases, so both
//     are granted their first key, and each then requests the key the other
//     holds — a cycle, every time, aborted with SQLSTATE 40P01. Not a
//     probability: the queue order is fixed before either can proceed.
//
// Because contention is constructed rather than sampled, a handful of rounds
// carries the weight forty sampled ones used to. Five executions are still
// evidence rather than proof — what changed is that a passing run no longer
// depends on the scheduler having happened to interleave them.
//
// --- MUTATION HARNESS (re-runnable; TASK-2372 round-5 amendment) ------------
//
// Each mutation below was applied to the production code, observed to fail,
// and reverted (2026-07-31, TASK-2372, against this commit's production
// code). The "Observed" lines are RECORDED RESULTS: nothing in the tree can
// substantiate them, which is the point of writing down the exact edit. Re-run
// with:
//
//	docker compose -f docker-compose.test.yml up -d --wait
//	PORT=$(docker compose -f docker-compose.test.yml port postgres 5432 | sed 's/.*://')
//	PAD_TEST_POSTGRES_URL="postgres://pad:pad@127.0.0.1:$PORT/pad?sslmode=disable" \
//	  go test ./internal/store/ -run 'CrossWorkspaces_Opposing|JointlyExceedQuota' -count=1
//
// The port is READ BACK, not hardcoded (TASK-2708): the compose file lets
// Docker assign it, so several worktrees can run the Postgres leg at once.
// This recipe said 5445 until that change; pasting it now would connect to
// whatever else happens to be on that port, or to nothing.
//
// MUTATION A — remove the outer acquisition entirely.
//
//	items_cross_workspace_copy.go, copyItemAcrossWorkspacesTx, step 1: delete
//	the `if _, err := s.acquireWorkspaceLocksOrdered(tx, sourceWorkspaceID,
//	req.TargetWorkspaceID); err != nil { return nil, err }` block.
//
//	Observed: TestCopyItemAcrossWorkspaces_OpposingMovesDoNotDeadlock fails
//	on round 0 with "opposing cross-workspace MOVES deadlocked: acquire
//	workspace seq lock: ERROR: deadlock detected (SQLSTATE 40P01)".
//	TestCopyItemAcrossWorkspaces_OpposingDirectionsDoNotDeadlock fails too,
//	but on the gate's readiness check rather than a deadlock ("only 1 of 2
//	transactions reached the contested region within 30s"): with no outer
//	acquisition a plain copy takes exactly one workspace key, and the two
//	directions serialize on their OVERLAPPING collection FOR UPDATE rows
//	instead, so the second copy never becomes an advisory waiter. Either way
//	the mutation cannot pass, and the message says which shape broke.
//
// MUTATION B — drop the ordering: acquire in argument order.
//
//	items_cross_workspace_copy.go, in acquireWorkspaceLocksOrdered, replace
//	`ordered := sortedDedupedLockKeys(keys)` with `ordered := keys`. That is
//	argument order — source, then target — so an A->B copy and a B->A copy
//	take the two keys in opposite orders.
//
//	NOT what this mutation models, and worth stating because it is the
//	intuitive reading: sorting by workspace ID rather than lock key. That is
//	still a total order, and both directions of a pair sort the same way, so
//	it does not deadlock — verified by mutating acquireWorkspaceLocksOrdered
//	to sort the IDs and lock in that order, which leaves both tests green.
//	The lock-KEY requirement is therefore not what these two tests defend;
//	it is defended by TestAcquireWorkspaceLocksOrdered_CollidingKeysTakeOneLock
//	(hashtext collisions collapse to one key) and by the re-entrancy with the
//	hashtext-keyed acquisitions elsewhere in the pipeline. What these tests
//	defend is that SOME consistent order is taken, and that the acquisition
//	happens at all.
//
//	Observed: BOTH deadlock tests fail, on round 0 and on every subsequent
//	round, with "... deadlocked: acquire workspace lock N: ERROR: deadlock
//	detected (SQLSTATE 40P01)" — the copy test because argument order is not
//	key order, the move test because it takes the same two keys the same
//	wrong way. (This mutation also drops the dedup, so
//	TestAcquireWorkspaceLocksOrdered_CollidingKeysTakeOneLock fails
//	alongside them; that is the mutation being crude, not a third signal.)
//
// MUTATION D — absorb the deadlock instead of surfacing it.
//
//	Not a lock-ordering mutation: the vacuity a retry wrapper would introduce
//	(Codex round 6). Apply mutation B, and additionally, in
//	CopyItemAcrossWorkspaces, wrap the call:
//
//		result, err := s.copyItemAcrossWorkspacesTx(req, sourceWorkspaceID)
//		for i := 0; err != nil && isDeadlockError(err) && i < 5; i++ { // MUTATION D
//			result, err = s.copyItemAcrossWorkspacesTx(req, sourceWorkspaceID)
//		}
//
//	Observed: with only the returned-error assertions this passes — the retry
//	completes serially and every count is right. With the deadlocksSettled
//	check BOTH tests fail, on "Postgres recorded 5 deadlock(s) during opposing
//	copies/moves, want 0". That check is why the tests assert the cycle did not
//	FORM, not merely that no caller saw it.
//
// The two Postgres deadlock tests prove different halves — the move test that
// the outer acquisition must EXIST, the copy test that it must be ORDERED —
// so a mutation failing only one of them is expected, and both mutations
// together must fail at least one each.

// advisoryLockGate parks concurrent copy transactions on the workspace
// advisory locks they are about to take, and holds them there until every
// contender is verifiably queued.
//
// It is deliberately NOT a seam in the production code. Nothing about the copy
// protocol is instrumented or made test-aware: the gate holds the same keys by
// the same rule (hashtext(workspace_id)::bigint) from an ordinary transaction
// on another connection, so the code under test cannot tell it apart from a
// concurrent writer — which is precisely what it is standing in for.
type advisoryLockGate struct {
	t    *testing.T
	s    *Store
	tx   *sql.Tx
	keys []int64
}

// newAdvisoryLockGate opens a transaction and takes the advisory lock for
// every supplied workspace, in sorted key order (the gate obeys the same
// protocol it is testing, so it can never be half of a cycle itself).
func newAdvisoryLockGate(t *testing.T, s *Store, workspaceIDs ...string) *advisoryLockGate {
	t.Helper()
	if s.dialect.Driver() != DriverPostgres {
		t.Fatal("advisoryLockGate requires Postgres")
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("advisory gate: begin: %v", err)
	}
	keys := make([]int64, 0, len(workspaceIDs))
	for _, id := range workspaceIDs {
		var key int64
		if err := tx.QueryRow("SELECT hashtext($1)::bigint", id).Scan(&key); err != nil {
			tx.Rollback() //nolint:errcheck // best effort on a failed setup
			t.Fatalf("advisory gate: lock key for %q: %v", id, err)
		}
		keys = append(keys, key)
	}
	g := &advisoryLockGate{t: t, s: s, tx: tx, keys: sortedDedupedLockKeys(keys)}
	// Backstop: a gate that never reaches release() — a panic, or a Fatalf on
	// some other assertion mid-round — would otherwise hold its keys and its
	// pooled connection for the rest of the test. Rollback after a successful
	// Commit is ErrTxDone, which is exactly the no-op wanted here.
	t.Cleanup(func() { tx.Rollback() }) //nolint:errcheck // no-op once released
	for _, key := range g.keys {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", key); err != nil {
			tx.Rollback() //nolint:errcheck // best effort on a failed setup
			t.Fatalf("advisory gate: acquire %d: %v", key, err)
		}
	}
	return g
}

// waitForContenders blocks until `n` distinct backends are queued as ungranted
// waiters on the gate's keys — i.e. until n copy transactions are open and
// parked at their first lock request.
//
// Returns an error rather than calling Fatalf so the caller can release the
// gate first; leaving it held would strand the contenders forever.
func (g *advisoryLockGate) waitForContenders(n int) error {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		got, err := g.waiters()
		if err != nil {
			return err
		}
		if got >= n {
			return nil
		}
		last = got
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("only %d of %d transactions reached the contested region within 30s: "+
		"they are not queued on the workspace advisory locks. The lock protocol changed shape — "+
		"the acquisition was removed, something else now blocks first (the collection FOR UPDATE "+
		"rows are the near neighbour), or a lock this gate does not hold serializes them earlier", last, n)
}

// waiters counts distinct backends waiting on the gate's advisory keys in THIS
// database. Advisory keys land in pg_locks as classid = key>>32, objid = key
// (both 32-bit), so the pair is derived from the key rather than matched
// loosely — an unrelated advisory waiter must not count.
//
// The split is unsigned, and hashtext returns int4, so NEGATIVE keys are the
// interesting half. Verified against the server rather than assumed: key
// -469100607 appears as (classid 4294967295, objid 3825866689), which is what
// the uint32 conversions below produce. A signed shift would have looked
// right for positive keys and silently matched nothing for negative ones —
// i.e. it would have degraded into the 30s readiness timeout for roughly half
// of all workspace pairs.
//
// objsubid distinguishes the one-bigint form (1) from the two-int form (2),
// which can present the same classid/objid pair. Pad only ever takes the
// bigint form, so the filter is a guard against a future two-int caller being
// miscounted as a contender here — a false-ready, which is the direction that
// would make a test pass vacuously.
func (g *advisoryLockGate) waiters() (int, error) {
	query := `
		SELECT COUNT(DISTINCT pid) FROM pg_locks
		WHERE locktype = 'advisory'
		  AND NOT granted
		  AND objsubid = 1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND (`
	args := make([]any, 0, len(g.keys)*2)
	for i, key := range g.keys {
		if i > 0 {
			query += " OR "
		}
		query += fmt.Sprintf("(classid::bigint = $%d AND objid::bigint = $%d)", len(args)+1, len(args)+2)
		args = append(args, int64(uint32(uint64(key)>>32)), int64(uint32(uint64(key))))
	}
	query += ")"

	var n int
	if err := g.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("advisory gate: count waiters: %w", err)
	}
	return n, nil
}

// release commits the holding transaction, freeing every queued contender at
// once. Idempotent-ish: a second call is a no-op error that the test ignores.
func (g *advisoryLockGate) release() {
	g.t.Helper()
	if err := g.tx.Commit(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		g.t.Fatalf("advisory gate: release: %v", err)
	}
}

// deadlocksDetected reads this database's cumulative deadlock counter.
//
// The error channel alone cannot prove no deadlock HAPPENED — only that none
// SURFACED. A future retry-on-40P01 wrapper around CopyItemAcrossWorkspaces
// would absorb the abort, complete serially, and leave both deadlock tests
// green while the cycle they exist to forbid formed on every round (Codex
// round 6). Postgres counts the aborts itself, below the layer any retry could
// hide, so the tests assert on the counter as well as on the returned errors.
//
// Read before and after; assert the delta is zero. The counter is cumulative
// per database and each test has its own database (testStorePostgres), so no
// other test can contribute to the delta.
//
// Stats flushing can lag a transaction's end — a backend flushes its pending
// counts at most about once a second — so a bare read taken immediately after
// wg.Wait() could still see zero for a deadlock that did happen. Callers use
// deadlocksSettled, which waits that window out.
func deadlocksDetected(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(
		`SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`,
	).Scan(&n); err != nil {
		t.Fatalf("read deadlock counter: %v", err)
	}
	return n
}

// deadlocksSettled returns how many deadlocks were recorded since `before`,
// having given Postgres's statistics flush time to catch up.
//
// It returns the moment the counter moves — a failing run reports immediately
// — and otherwise waits out the flush window before agreeing that zero is
// zero. Without the wait the assertion would be a coin flip on a fast machine:
// the copies' backends stay pooled and idle after wg.Wait(), so their pending
// counts can sit unflushed for up to about a second (Codex round 7).
func deadlocksSettled(t *testing.T, s *Store, before int64) int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := deadlocksDetected(t, s) - before; got != 0 {
			return got
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCopyItemAcrossWorkspaces_OpposingDirectionsDoNotDeadlock drives A->B and
// B->A copies simultaneously, repeatedly.
//
// This is the test that FAILS without the sorted lock acquisition. With an
// UNORDERED acquisition — the keys taken in argument order, source then
// target — one
// goroutine holds hashtext(A) and waits on hashtext(B) while the other holds
// hashtext(B) and waits on hashtext(A); Postgres detects the cycle and aborts
// one transaction with SQLSTATE 40P01. With the keys sorted, both transactions
// request the same key first, so the cycle cannot form.
//
// (An ID-SORTED acquisition would also be a consistent order for a pair and
// does NOT deadlock — see mutation B in the file header. This test proves the
// acquisition is ordered, not that it is ordered by lock key specifically.)
//
// The overlap is constructed, not sampled: advisoryLockGate holds both
// workspace keys until pg_locks shows both copies queued on them. See the
// file header for why the barrier sits BEFORE the first acquisition and for
// mutation B, which this test is the one to catch.
func TestCopyItemAcrossWorkspaces_OpposingDirectionsDoNotDeadlock(t *testing.T) {
	requirePostgresForConcurrency(t)

	s := testStore(t)
	wsA := createTestWorkspace(t, s, "Deadlock A")
	wsB := createTestWorkspace(t, s, "Deadlock B")
	colA := createTestCollection(t, s, wsA.ID, "Tasks A")
	colB := createTestCollection(t, s, wsB.ID, "Tasks B")

	// A handful of rounds, not forty: each round is a constructed collision,
	// so the rounds buy durability against a future change of shape, not
	// probability of hitting the race.
	const rounds = 5
	sourcesA := make([]*models.Item, rounds)
	sourcesB := make([]*models.Item, rounds)
	for i := 0; i < rounds; i++ {
		sourcesA[i] = createTestItem(t, s, wsA.ID, colA.ID, fmt.Sprintf("A-%d", i), "body")
		sourcesB[i] = createTestItem(t, s, wsB.ID, colB.ID, fmt.Sprintf("B-%d", i), "body")
	}

	errs := make(chan error, rounds*2)
	run := func(wg *sync.WaitGroup, src *models.Item, targetWS, targetCol string) {
		defer wg.Done()
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

	deadlocksBefore := deadlocksDetected(t, s)

	// A readiness failure does not Fatal inside the loop: the accumulated
	// copy errors are the more informative signal and would otherwise be
	// thrown away unread (Codex round 6). Break, report those first, then the
	// readiness failure.
	var readyFailure error
	for i := 0; i < rounds; i++ {
		gate := newAdvisoryLockGate(t, s, wsA.ID, wsB.ID)
		var wg sync.WaitGroup
		wg.Add(2)
		go run(&wg, sourcesA[i], wsB.ID, colB.ID) // A -> B
		go run(&wg, sourcesB[i], wsA.ID, colA.ID) // B -> A
		readyErr := gate.waitForContenders(2)
		gate.release()
		wg.Wait()
		if readyErr != nil {
			readyFailure = fmt.Errorf("round %d: %w", i, readyErr)
			break
		}
	}
	close(errs)

	for err := range errs {
		if isDeadlockError(err) {
			t.Fatalf("opposing cross-workspace copies deadlocked: %v", err)
		}
		t.Fatalf("opposing cross-workspace copy failed: %v", err)
	}
	if readyFailure != nil {
		t.Fatal(readyFailure)
	}

	// No deadlock was ABSORBED either — see deadlocksDetected.
	if got := deadlocksSettled(t, s, deadlocksBefore); got != 0 {
		t.Fatalf("Postgres recorded %d deadlock(s) during opposing copies, want 0", got)
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
// it must be ORDERED (an unsorted acquisition deadlocks there even though every
// transaction takes both keys). Ordering by lock KEY rather than by workspace
// ID is a separate requirement, defended by the collision test rather than by
// either of these — see mutation B in the file header.
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

	const rounds = 5
	sourcesA := make([]*models.Item, rounds)
	sourcesB := make([]*models.Item, rounds)
	for i := 0; i < rounds; i++ {
		sourcesA[i] = createTestItem(t, s, wsA.ID, colAOut.ID, fmt.Sprintf("mA-%d", i), "body")
		sourcesB[i] = createTestItem(t, s, wsB.ID, colBOut.ID, fmt.Sprintf("mB-%d", i), "body")
	}

	errs := make(chan error, rounds*2)
	run := func(wg *sync.WaitGroup, src *models.Item, targetWS, targetCol string) {
		defer wg.Done()
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

	// Same constructed collision as the copy test. It works here even though
	// the mutation this test catches (a REMOVED acquisition) takes its first
	// workspace key much later in the transaction — at the create in B and the
	// archive in A — because the gate holds both keys and waits for both
	// transactions to queue, wherever in the pipeline they reach them.
	deadlocksBefore := deadlocksDetected(t, s)

	var readyFailure error
	for i := 0; i < rounds; i++ {
		gate := newAdvisoryLockGate(t, s, wsA.ID, wsB.ID)
		var wg sync.WaitGroup
		wg.Add(2)
		go run(&wg, sourcesA[i], wsB.ID, colBIn.ID) // A -> B, archiving in A
		go run(&wg, sourcesB[i], wsA.ID, colAIn.ID) // B -> A, archiving in B
		readyErr := gate.waitForContenders(2)
		gate.release()
		wg.Wait()
		if readyErr != nil {
			readyFailure = fmt.Errorf("round %d: %w", i, readyErr)
			break
		}
	}
	close(errs)

	for err := range errs {
		if isDeadlockError(err) {
			t.Fatalf("opposing cross-workspace MOVES deadlocked: %v", err)
		}
		t.Fatalf("opposing cross-workspace move failed: %v", err)
	}
	if readyFailure != nil {
		t.Fatal(readyFailure)
	}

	if got := deadlocksSettled(t, s, deadlocksBefore); got != 0 {
		t.Fatalf("Postgres recorded %d deadlock(s) during opposing moves, want 0", got)
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

	// Constructed overlap, as in the deadlock tests: the gate holds the
	// destination key so BOTH copies are open transactions queued on it before
	// either can read the quota. Without that, the two calls could run
	// end-to-end serially and the assertion below would hold vacuously — the
	// second copy would see the committed row because it started afterwards,
	// not because the lock made it.
	gate := newAdvisoryLockGate(t, s, wsA.ID, wsB.ID)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, src := range []*models.Item{src1, src2} {
		wg.Add(1)
		go func(src *models.Item) {
			defer wg.Done()
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
	readyErr := gate.waitForContenders(2)
	gate.release()
	wg.Wait()
	close(results)
	if readyErr != nil {
		t.Fatal(readyErr)
	}

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
