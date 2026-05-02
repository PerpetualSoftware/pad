package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestGetRoleBreakdown_UnassignedRowHasExplicitLabels is the
// regression test for BUG-987 bug 14. Previously the unassigned-items
// row was emitted with empty role_name + role_slug, so dashboard
// consumers saw a "phantom" entry: a row with item_count > 0 but no
// identifying label. Now the row is explicitly labelled "Unassigned"
// / "unassigned" while still keeping role_id null (the marker that
// distinguishes an unassigned bucket from a real role with that slug).
func TestGetRoleBreakdown_UnassignedRowHasExplicitLabels(t *testing.T) {
	s := testStore(t)
	ws := newTestWorkspace(t, s, "rb-bug987")

	coll := createTestCollection(t, s, ws.ID, "tasks")
	// One item with no agent role — should land in the unassigned row.
	createTestItem(t, s, ws.ID, coll.ID, "Unassigned task", "")

	got, err := s.GetRoleBreakdown(ws.ID)
	if err != nil {
		t.Fatalf("GetRoleBreakdown: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row (unassigned only); got %d", len(got))
	}
	row := got[0]
	if row.RoleID != nil {
		t.Errorf("RoleID = %v, want nil for unassigned row", row.RoleID)
	}
	if row.RoleName != "Unassigned" {
		t.Errorf("RoleName = %q, want \"Unassigned\"", row.RoleName)
	}
	if row.RoleSlug != "unassigned" {
		t.Errorf("RoleSlug = %q, want \"unassigned\"", row.RoleSlug)
	}
	if row.ItemCount != 1 {
		t.Errorf("ItemCount = %d, want 1", row.ItemCount)
	}
}

// newTestWorkspace creates a workspace bound to the test store with
// the given slug. Helper for store-level tests that don't already use
// the larger setup harness (e.g. permission tests). Kept minimal —
// just enough state for GetRoleBreakdown to succeed.
func newTestWorkspace(t *testing.T, s *Store, slug string) *models.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{
		Slug: slug,
		Name: slug,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}
