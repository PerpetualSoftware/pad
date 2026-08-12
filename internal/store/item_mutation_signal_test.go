package store

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestLastMutation_StatusChange asserts UpdateItem attaches a
// race-free ItemMutationSignal (TASK-2533) when the done-field changes,
// carrying the same from/to values the status_transitions row records.
func TestLastMutation_StatusChange(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Do a thing", "")

	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)})
	if err != nil {
		t.Fatalf("update item: %v", err)
	}
	if updated.LastMutation == nil {
		t.Fatalf("expected LastMutation to be set")
	}
	if !updated.LastMutation.StatusChanged {
		t.Fatalf("expected StatusChanged=true")
	}
	if updated.LastMutation.FromStatus != "open" || updated.LastMutation.ToStatus != "done" {
		t.Fatalf("expected open -> done, got %q -> %q", updated.LastMutation.FromStatus, updated.LastMutation.ToStatus)
	}
	if updated.LastMutation.StatusFieldKey != "status" {
		t.Fatalf("expected field key 'status', got %q", updated.LastMutation.StatusFieldKey)
	}
	if updated.LastMutation.AssignmentChanged {
		t.Fatalf("expected AssignmentChanged=false")
	}
}

// TestLastMutation_NoOpWhenNothingChanges asserts that an update touching
// neither the done-field nor assignment leaves LastMutation nil — the
// notification pipeline must not fire on unrelated field edits (e.g. a
// title rename).
func TestLastMutation_NoOpWhenNothingChanges(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Do a thing", "")

	newTitle := "Do a different thing"
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("update item: %v", err)
	}
	if updated.LastMutation != nil {
		t.Fatalf("expected LastMutation to stay nil, got %+v", updated.LastMutation)
	}
}

// TestLastMutation_AssignmentChange asserts assignment (set, then clear)
// is captured, and that it is independent of status.
func TestLastMutation_AssignmentChange(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Assign me", "")

	assignee := createTestUser(t, s, "assignee@example.com", "Assignee", "pw")
	if err := s.AddWorkspaceMember(wsID, assignee.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{AssignedUserID: &assignee.ID})
	if err != nil {
		t.Fatalf("assign item: %v", err)
	}
	if updated.LastMutation == nil || !updated.LastMutation.AssignmentChanged {
		t.Fatalf("expected AssignmentChanged=true, got %+v", updated.LastMutation)
	}
	if updated.LastMutation.FromAssignedUserID != "" || updated.LastMutation.ToAssignedUserID != assignee.ID {
		t.Fatalf("expected '' -> %q, got %q -> %q", assignee.ID,
			updated.LastMutation.FromAssignedUserID, updated.LastMutation.ToAssignedUserID)
	}
	if updated.LastMutation.StatusChanged {
		t.Fatalf("expected StatusChanged=false for an assignment-only update")
	}

	cleared, err := s.UpdateItem(item.ID, models.ItemUpdate{ClearAssignedUser: true})
	if err != nil {
		t.Fatalf("clear assignment: %v", err)
	}
	if cleared.LastMutation == nil || !cleared.LastMutation.AssignmentChanged {
		t.Fatalf("expected AssignmentChanged=true on clear, got %+v", cleared.LastMutation)
	}
	if cleared.LastMutation.FromAssignedUserID != assignee.ID || cleared.LastMutation.ToAssignedUserID != "" {
		t.Fatalf("expected %q -> '', got %q -> %q", assignee.ID,
			cleared.LastMutation.FromAssignedUserID, cleared.LastMutation.ToAssignedUserID)
	}
}

// TestLastMutation_OnMove mirrors TestStatusTransition_CapturedOnMoveWithStatusOverride
// but asserts the LastMutation signal MoveItemWithPreCheck attaches.
func TestLastMutation_OnMove(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	targetCol := createTestCollection(t, s, wsID, "Done Tasks")
	item := createTestItem(t, s, wsID, colID, "Move me", "")

	moved, err := s.MoveItem(item.ID, targetCol.ID, `{"status":"done"}`)
	if err != nil {
		t.Fatalf("move item: %v", err)
	}
	if moved.LastMutation == nil || !moved.LastMutation.StatusChanged {
		t.Fatalf("expected StatusChanged=true on move, got %+v", moved.LastMutation)
	}
	if moved.LastMutation.FromStatus != "open" || moved.LastMutation.ToStatus != "done" {
		t.Fatalf("expected open -> done, got %q -> %q", moved.LastMutation.FromStatus, moved.LastMutation.ToStatus)
	}
}

// TestLastMutation_NoOpOnMoveWithoutStatusChange mirrors the sibling
// status_transitions test: a move that doesn't touch the done-field must
// not attach a LastMutation signal.
func TestLastMutation_NoOpOnMoveWithoutStatusChange(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	targetCol := createTestCollection(t, s, wsID, "Other Tasks")
	item := createTestItem(t, s, wsID, colID, "Move me quietly", "")

	moved, err := s.MoveItem(item.ID, targetCol.ID, item.Fields)
	if err != nil {
		t.Fatalf("move item: %v", err)
	}
	if moved.LastMutation != nil {
		t.Fatalf("expected LastMutation to stay nil, got %+v", moved.LastMutation)
	}
}

// TestLastMutation_AssignmentDelta_NotMisattributedUnderConcurrentWrite
// covers TASK-2533 codex round 2 finding 4: the assignment-delta capture
// used to compare against `existing`, which stayed the STALE pre-tx
// snapshot for any update that triggered none of {precheck,
// ExpectedUpdatedAt, FieldsPatch} — including a plain title-only update.
// A concurrent OTHER transaction's assignment change landing between
// this transaction's pre-tx read and its lock acquisition would get
// misattributed to THIS transaction: a title-only update would report a
// spurious AssignmentChanged for a change it never made, duplicating the
// real one the OTHER transaction already reported correctly.
//
// Reproduces the exact interleaving using UpdateItemWithPreCheck's
// precheck hook as a synchronization point: TX2 (the real assignment
// change) blocks INSIDE its precheck — after acquiring the write lock,
// before its own UPDATE statement runs — while TX1 (a concurrent
// title-only update, no precheck) runs its pre-tx GetItem (WAL mode: not
// blocked by TX2's held write lock) and then blocks on its own
// tx.Begin() (BEGIN IMMEDIATE) until TX2 commits and releases the lock.
func TestLastMutation_AssignmentDelta_NotMisattributedUnderConcurrentWrite(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Contested", "")
	assignee := createTestUser(t, s, "assignee2@example.com", "Assignee Two", "pw")
	if err := s.AddWorkspaceMember(wsID, assignee.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	tx2InPrecheck := make(chan struct{})
	releaseTx2 := make(chan struct{})
	tx1Started := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	var tx2Result *models.Item
	var tx2Err error
	go func() {
		defer wg.Done()
		precheck := func(tx *sql.Tx, existing *models.Item) error {
			close(tx2InPrecheck)
			<-releaseTx2
			return nil
		}
		tx2Result, tx2Err = s.UpdateItemWithPreCheck(item.ID,
			models.ItemUpdate{AssignedUserID: &assignee.ID}, precheck)
	}()

	var tx1Result *models.Item
	var tx1Err error
	go func() {
		defer wg.Done()
		<-tx2InPrecheck // wait until TX2 holds the write lock but hasn't committed
		close(tx1Started)
		newTitle := "Contested (renamed)"
		// TX1's own pre-tx GetItem (inside UpdateItem) runs here, racing
		// ahead of TX2's held write lock (WAL mode: a plain SELECT isn't
		// blocked by it) — it must see the PRE-TX2 assignee (nil), which
		// is the whole point: this is the stale snapshot that must NOT
		// leak into TX1's delta once TX2 has committed by the time TX1
		// actually acquires the lock and writes. TX1's own tx.Begin()
		// (BEGIN IMMEDIATE) blocks right after, until TX2 releases the
		// lock below.
		tx1Result, tx1Err = s.UpdateItem(item.ID, models.ItemUpdate{Title: &newTitle})
	}()

	<-tx1Started
	// Give TX1's pre-tx GetItem (fast, non-blocking SELECT) time to
	// actually complete and TX1's tx.Begin() time to actually block on
	// TX2's held lock, before letting TX2 proceed to commit.
	time.Sleep(50 * time.Millisecond)
	close(releaseTx2)

	// Bounded wait, not an unconditional wg.Wait(): TX2 is released
	// unconditionally above regardless of scheduler contention, so both
	// goroutines are guaranteed to make progress — but a bare wg.Wait()
	// still has no ceiling on how long that progress can legitimately take
	// under real DB lock contention. 10s is generous for a two-row update
	// even on a slow, shared runner; if it's ever exceeded, fail fast with
	// a clear diagnostic instead of silently eating test-binary budget.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for TX1/TX2 to complete — synchronization assumption broke down under contention")
	}

	if tx2Err != nil {
		t.Fatalf("TX2 (assignment): %v", tx2Err)
	}
	if tx1Err != nil {
		t.Fatalf("TX1 (title-only): %v", tx1Err)
	}

	if tx2Result.LastMutation == nil || !tx2Result.LastMutation.AssignmentChanged {
		t.Fatalf("expected TX2 to correctly report AssignmentChanged, got %+v", tx2Result.LastMutation)
	}
	if tx2Result.LastMutation.ToAssignedUserID != assignee.ID {
		t.Fatalf("expected TX2's ToAssignedUserID to be %q, got %q", assignee.ID, tx2Result.LastMutation.ToAssignedUserID)
	}

	// The bug: TX1 never touched assignment, but with a stale `existing`
	// it would see (pre-TX2 nil) vs (post-TX2 assignee.ID) and wrongly
	// report AssignmentChanged for a transition it didn't make.
	if tx1Result.LastMutation != nil && tx1Result.LastMutation.AssignmentChanged {
		t.Fatalf("TX1 (title-only update) must NOT report AssignmentChanged — that transition belongs to TX2 alone, got %+v", tx1Result.LastMutation)
	}
}
