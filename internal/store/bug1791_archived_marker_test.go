package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListItems_IncludeArchived_PopulatesDeletedAt pins BUG-1791. An archived
// (soft-deleted) item must (a) stay out of the default active-only list and
// (b) appear in the include-archived list WITH DeletedAt populated — that
// marker is what lets a caller tell an archived row apart from a live one.
// Before the fix, ListItems' shared scanner dropped deleted_at, so an item
// surfaced by `all=true` looked identical to a live row, which read as
// corruption when it then 404'd on get/update.
func TestListItems_IncludeArchived_PopulatesDeletedAt(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, col.ID, "Doomed", "")
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	// Default (active-only) list must exclude the archived item.
	active, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: col.Slug})
	if err != nil {
		t.Fatalf("ListItems active: %v", err)
	}
	for _, it := range active {
		if it.ID == item.ID {
			t.Fatalf("archived item leaked into the default active-only list")
		}
	}

	// IncludeArchived must return it AND populate DeletedAt.
	all, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: col.Slug, IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListItems include-archived: %v", err)
	}
	var found *models.Item
	for i := range all {
		if all[i].ID == item.ID {
			found = &all[i]
		}
	}
	if found == nil {
		t.Fatalf("archived item missing from the include-archived list")
	}
	if found.DeletedAt == nil {
		t.Errorf("DeletedAt not populated for archived item in include-archived list (BUG-1791 marker missing)")
	}
}
