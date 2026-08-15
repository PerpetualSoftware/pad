package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2566: an explicit empty string for assigned_user_id / agent_role_id
// is what a JSON client sends when a user blanks the field. It must clear
// the assignment (write NULL) exactly like ClearAssignedUser /
// ClearAgentRole — before the fix it was bound verbatim and failed the FK
// with a driver-specific 500.

func TestUpdateItem_EmptyAssignedUserIDClearsAssignment(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Assign then blank", "")

	assignee := createTestUser(t, s, "blankme@example.com", "BlankMe", "pw")
	if err := s.AddWorkspaceMember(wsID, assignee.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{AssignedUserID: &assignee.ID}); err != nil {
		t.Fatalf("assign item: %v", err)
	}

	empty := ""
	cleared, err := s.UpdateItem(item.ID, models.ItemUpdate{AssignedUserID: &empty})
	if err != nil {
		t.Fatalf("update with empty assigned_user_id: %v", err)
	}
	if cleared.AssignedUserID != nil {
		t.Fatalf("expected assignment cleared, got %q", *cleared.AssignedUserID)
	}
	// The mutation signal must match the ClearAssignedUser shape — the
	// notification pipeline keys off it.
	if cleared.LastMutation == nil || !cleared.LastMutation.AssignmentChanged {
		t.Fatalf("expected AssignmentChanged=true, got %+v", cleared.LastMutation)
	}
	if cleared.LastMutation.FromAssignedUserID != assignee.ID || cleared.LastMutation.ToAssignedUserID != "" {
		t.Fatalf("expected %q -> '', got %q -> %q", assignee.ID,
			cleared.LastMutation.FromAssignedUserID, cleared.LastMutation.ToAssignedUserID)
	}
}

func TestUpdateItem_EmptyAgentRoleIDClearsRole(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Role then blank", "")

	role, err := s.CreateAgentRole(wsID, models.AgentRoleCreate{Name: "Blanker"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{AgentRoleID: &role.ID}); err != nil {
		t.Fatalf("set role: %v", err)
	}

	empty := ""
	cleared, err := s.UpdateItem(item.ID, models.ItemUpdate{AgentRoleID: &empty})
	if err != nil {
		t.Fatalf("update with empty agent_role_id: %v", err)
	}
	if cleared.AgentRoleID != nil {
		t.Fatalf("expected role cleared, got %q", *cleared.AgentRoleID)
	}
}

func TestCreateItem_EmptyAssignmentIDsAreNull(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)

	empty := ""
	item, err := s.CreateItem(wsID, colID, models.ItemCreate{
		Title:          "Born unassigned",
		AssignedUserID: &empty,
		AgentRoleID:    &empty,
	})
	if err != nil {
		t.Fatalf("create with empty assignment ids: %v", err)
	}
	if item.AssignedUserID != nil {
		t.Fatalf("expected nil assigned_user_id, got %q", *item.AssignedUserID)
	}
	if item.AgentRoleID != nil {
		t.Fatalf("expected nil agent_role_id, got %q", *item.AgentRoleID)
	}
}
