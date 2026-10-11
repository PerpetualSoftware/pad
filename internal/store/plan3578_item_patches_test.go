package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3578 step 3: a stored patch is read only within its workspace, and
// goes with its item.
func TestPLAN3578_ItemPatchScopedAndCascaded(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Patches")
	other := createTestWorkspace(t, s, "Other")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Patched", "alpha")
	body := "ALPHA"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body, ForceVersion: true,
		PatchRecord: &models.ItemPatchRecord{ID: "p-1", Edits: `[{"old":"alpha","new":"ALPHA","ctx_before":"","ctx_after":""}]`}}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetItemPatch(ws.ID, "p-1"); err != nil || r == nil || r.ItemID != item.ID {
		t.Fatalf("own workspace: %+v %v", r, err)
	}
	if r, err := s.GetItemPatch(other.ID, "p-1"); err != nil || r != nil {
		t.Fatalf("another workspace must not see it: %+v %v", r, err)
	}
	// Items are hard-deleted only by the workspace purge; its list carries
	// item_patches.
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeWorkspaceData(ws.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_patches WHERE id = ?`), "p-1").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the patch record outlived its workspace purge: %d %v", n, err)
	}
}
