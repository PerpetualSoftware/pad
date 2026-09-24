package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG-2777. CreateActivityDebounced coalesces a writer's "updated" rows by
// reading a recent candidate and merging into it; when the read finds NONE it
// inserts. Two calls of one writer that both miss both inserted, so the run the
// debounce exists to coalesce started as two rows. BUG-2770's compare-and-set
// covers two writers and ONE row; this is two writers and no row yet.
//
// The fix re-checks the miss and inserts as one serialized decision per
// document (insertDebouncedOnMiss). Three tests, each catching a different
// wrong implementation:
//
//   - ConcurrentMissInsertsOneRow: a competitor inserts inside the unlocked
//     miss-to-insert window. Catches a fix without the RE-READ.
//   - LockedMissSerializes (Postgres only): a competitor is released while the
//     first call holds the lock, and must be SEEN waiting on it. Catches a fix
//     without the LOCK, which the first test cannot: its competitor finishes
//     before the first call's re-read, so a re-read alone passes it.
//   - ConcurrentBurstInsertsOneRow: real goroutines, no seam, both backends.
//     Measured against the unfixed code at 29/30 bursts producing >1 row on
//     each backend (BUG-2777 checkpoint 1); the lock makes it 0 by
//     construction, so a single duplicate is a failure. On SQLite this is the
//     lock's test, because BEGIN IMMEDIATE is the lock there and no
//     pg_stat_activity exists to observe a waiter.

func TestCreateActivityDebounced_ConcurrentMissInsertsOneRow(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MissRace")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	fired := 0
	s.afterDebounceMiss = func() {
		if fired > 0 {
			return
		}
		fired++
		if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, "", "status: open → done")); err != nil {
			t.Errorf("competing write: %v", err)
		}
	}
	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, "", "title: a → b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.afterDebounceMiss = nil
	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the test did not exercise the miss window", fired)
	}

	rows := updatedRowsFor(t, s, doc.ID)
	if len(rows) != 1 {
		t.Fatalf("want the run coalesced into 1 row, got %d — both calls that missed inserted (BUG-2777)", len(rows))
	}
	// One row is also what a call that DROPPED its change would leave, so the
	// row must carry both.
	changes := changesOf(t, rows[0])
	for _, want := range []string{"title", "status"} {
		if !strings.Contains(changes, want) {
			t.Errorf("surviving row lost the %q change: changes = %q", want, changes)
		}
	}
}

func TestCreateActivityDebounced_LockedMissSerializes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("observes an advisory-lock waiter in pg_stat_activity; SQLite's lock is BEGIN IMMEDIATE, covered by ConcurrentBurstInsertsOneRow")
	}
	ws := createTestWorkspace(t, s, "LockedMiss")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// Two OBSERVED outcomes, never a timeout (the BUG-2797 test's reasoning):
	// the competitor COMMITTED while the first call sat inside its lock (the
	// defect), or its backend is seen WAITING on an advisory lock (the fix).
	const deadline = 30 * time.Second
	var (
		once    sync.Once
		done    = make(chan error, 1)
		outcome string
	)
	s.afterDebounceLockedMiss = func() {
		once.Do(func() {
			go func() {
				_, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, "", "status: open → done"))
				done <- err
			}()
			stop := time.After(deadline)
			for outcome == "" {
				select {
				case err := <-done:
					outcome = "committed"
					done <- err
				case <-stop:
					t.Errorf("within %s the competitor neither committed nor was seen waiting on a lock: inconclusive", deadline)
					return
				case <-time.After(20 * time.Millisecond):
					var waiting int
					if err := s.db.QueryRow(`SELECT count(*) FROM pg_stat_activity
						WHERE datname = current_database()
						  AND wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&waiting); err != nil {
						t.Errorf("read pg_stat_activity: %v", err)
						return
					}
					if waiting > 0 {
						outcome = "blocked"
					}
				}
			}
		})
	}
	defer func() { s.afterDebounceLockedMiss = nil }()

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, "", "title: a → b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if outcome == "" {
		t.Fatal("the seam never reached an outcome (it did not fire, or the run was inconclusive), so this run exercised nothing")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the competing write failed, so this run never exercised the race: %v", err)
		}
	case <-time.After(deadline):
		t.Fatal("the competing write never finished after the first call committed")
	}

	if outcome != "blocked" {
		t.Errorf("the competing call COMMITTED while the first held its miss decision: nothing serialised the two")
	}
	if rows := updatedRowsFor(t, s, doc.ID); len(rows) != 1 {
		t.Errorf("want the run coalesced into 1 row, got %d", len(rows))
	}
}

func TestCreateActivityDebounced_ConcurrentBurstInsertsOneRow(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MissBurst")
	const trials, writers = 30, 4
	dup := 0
	for i := 0; i < trials; i++ {
		doc := createTestDoc(t, s, ws.ID, fmt.Sprintf("Doc %d", i), "content")
		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, "", "status: open → done")); err != nil {
					t.Errorf("write: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := len(updatedRowsFor(t, s, doc.ID)); n > 1 {
			dup++
		}
	}
	if dup > 0 {
		t.Errorf("%s: %d/%d bursts of %d concurrent calls by one writer produced more than one row", s.dialect.Driver(), dup, trials, writers)
	}
}
