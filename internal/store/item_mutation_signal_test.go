package store

import (
	"testing"

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
