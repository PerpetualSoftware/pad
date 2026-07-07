package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// linkChild wires child -> parent via a `parent` link (the shape
// GetChildItems / GetChildItemsForParents walk).
func linkChild(t *testing.T, s *Store, workspaceID, childID, parentID string) {
	t.Helper()
	if _, err := s.CreateItemLink(workspaceID, models.ItemLinkCreate{
		TargetID: parentID,
		LinkType: "parent",
	}, childID); err != nil {
		t.Fatalf("link child %s -> parent %s: %v", childID, parentID, err)
	}
}

// linkBlocks wires blocker -> blocked via a `blocks` link.
func linkBlocks(t *testing.T, s *Store, workspaceID, blockerID, blockedID string) {
	t.Helper()
	if _, err := s.CreateItemLink(workspaceID, models.ItemLinkCreate{
		TargetID: blockedID,
		LinkType: "blocks",
	}, blockerID); err != nil {
		t.Fatalf("link blocker %s -> blocked %s: %v", blockerID, blockedID, err)
	}
}

func TestGetChildItemsForParents(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	planA := createTestItem(t, s, ws.ID, col.ID, "Plan A", "")
	planB := createTestItem(t, s, ws.ID, col.ID, "Plan B", "")
	planC := createTestItem(t, s, ws.ID, col.ID, "Plan C (no children)", "")

	a1 := createTestItem(t, s, ws.ID, col.ID, "A child 1", "body-a1")
	a2 := createTestItem(t, s, ws.ID, col.ID, "A child 2", "body-a2")
	b1 := createTestItem(t, s, ws.ID, col.ID, "B child 1", "body-b1")

	linkChild(t, s, ws.ID, a1.ID, planA.ID)
	linkChild(t, s, ws.ID, a2.ID, planA.ID)
	linkChild(t, s, ws.ID, b1.ID, planB.ID)

	byParent, err := s.GetChildItemsForParents([]string{planA.ID, planB.ID, planC.ID})
	if err != nil {
		t.Fatalf("GetChildItemsForParents: %v", err)
	}

	if got := len(byParent[planA.ID]); got != 2 {
		t.Errorf("plan A: expected 2 children, got %d", got)
	}
	if got := len(byParent[planB.ID]); got != 1 {
		t.Errorf("plan B: expected 1 child, got %d", got)
	}
	if _, ok := byParent[planC.ID]; ok {
		t.Errorf("plan C: expected no map entry for childless parent")
	}

	// Grouping must match the per-parent GetChildItems (same rows).
	single, err := s.GetChildItems(planA.ID)
	if err != nil {
		t.Fatalf("GetChildItems: %v", err)
	}
	if len(single) != len(byParent[planA.ID]) {
		t.Errorf("batch child count %d != single %d for plan A", len(byParent[planA.ID]), len(single))
	}

	// The projection omits content (heavy body column) but keeps identity +
	// computed ref/fields.
	for _, child := range byParent[planA.ID] {
		if child.Content != "" {
			t.Errorf("expected empty content in batch projection, got %q for %s", child.Content, child.Title)
		}
		if child.Ref == "" {
			t.Errorf("expected computed ref on child %s", child.Title)
		}
	}
}

func TestGetChildItemsForParentsExcludesDeleted(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	plan := createTestItem(t, s, ws.ID, col.ID, "Plan", "")
	live := createTestItem(t, s, ws.ID, col.ID, "Live child", "")
	gone := createTestItem(t, s, ws.ID, col.ID, "Deleted child", "")
	linkChild(t, s, ws.ID, live.ID, plan.ID)
	linkChild(t, s, ws.ID, gone.ID, plan.ID)

	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	byParent, err := s.GetChildItemsForParents([]string{plan.ID})
	if err != nil {
		t.Fatalf("GetChildItemsForParents: %v", err)
	}
	if got := len(byParent[plan.ID]); got != 1 {
		t.Fatalf("expected 1 live child, got %d", got)
	}
	if byParent[plan.ID][0].ID != live.ID {
		t.Errorf("expected live child, got %s", byParent[plan.ID][0].Title)
	}
}

func TestGetChildItemsForParentsEmpty(t *testing.T) {
	s := testStore(t)
	byParent, err := s.GetChildItemsForParents(nil)
	if err != nil {
		t.Fatalf("GetChildItemsForParents(nil): %v", err)
	}
	if len(byParent) != 0 {
		t.Errorf("expected empty map, got %d entries", len(byParent))
	}
}

func TestGetBlocksEdges(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	blocker := createTestItem(t, s, ws.ID, col.ID, "Blocker", "")
	blocked := createTestItem(t, s, ws.ID, col.ID, "Blocked", "")
	// A non-blocks link must NOT appear in the result.
	parent := createTestItem(t, s, ws.ID, col.ID, "Parent", "")
	child := createTestItem(t, s, ws.ID, col.ID, "Child", "")

	linkBlocks(t, s, ws.ID, blocker.ID, blocked.ID)
	linkChild(t, s, ws.ID, child.ID, parent.ID)

	edges, err := s.GetBlocksEdges(ws.ID)
	if err != nil {
		t.Fatalf("GetBlocksEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 blocks edge, got %d: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.TargetID != blocked.ID {
		t.Errorf("expected target %s (blocked), got %s", blocked.ID, e.TargetID)
	}
	if e.SourceID != blocker.ID {
		t.Errorf("expected source %s (blocker), got %s", blocker.ID, e.SourceID)
	}
	if e.SourceTitle != "Blocker" {
		t.Errorf("expected source title 'Blocker', got %q", e.SourceTitle)
	}
	if e.SourceCollectionID != col.ID {
		t.Errorf("expected source collection %s, got %s", col.ID, e.SourceCollectionID)
	}
}

func TestGetBlocksEdgesExcludesDeleted(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	blocker := createTestItem(t, s, ws.ID, col.ID, "Blocker", "")
	blocked := createTestItem(t, s, ws.ID, col.ID, "Blocked", "")
	linkBlocks(t, s, ws.ID, blocker.ID, blocked.ID)

	// Soft-deleting either endpoint drops the edge (join requires both live).
	if err := s.DeleteItem(blocker.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	edges, err := s.GetBlocksEdges(ws.ID)
	if err != nil {
		t.Fatalf("GetBlocksEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges after deleting blocker, got %d", len(edges))
	}
}

func TestGetItemsByIDsIncludeDeleted(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	live := createTestItem(t, s, ws.ID, col.ID, "Live", "body")
	gone := createTestItem(t, s, ws.ID, col.ID, "Archived", "body")
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	got, err := s.GetItemsByIDsIncludeDeleted([]string{live.ID, gone.ID, "missing-id"})
	if err != nil {
		t.Fatalf("GetItemsByIDsIncludeDeleted: %v", err)
	}
	// Both live and soft-deleted must be present; missing ID absent.
	if got[live.ID] == nil {
		t.Errorf("expected live item present")
	}
	if got[gone.ID] == nil {
		t.Errorf("expected soft-deleted item present (include-deleted)")
	}
	if _, ok := got["missing-id"]; ok {
		t.Errorf("expected missing id absent")
	}
	if got[live.ID] != nil {
		if got[live.ID].Title != "Live" {
			t.Errorf("expected title 'Live', got %q", got[live.ID].Title)
		}
		if got[live.ID].Ref == "" {
			t.Errorf("expected computed ref on hydrated item")
		}
		if got[live.ID].Content != "" {
			t.Errorf("expected empty content in projection, got %q", got[live.ID].Content)
		}
	}
}

func TestGetItemsByIDsIncludeDeletedEmpty(t *testing.T) {
	s := testStore(t)
	got, err := s.GetItemsByIDsIncludeDeleted(nil)
	if err != nil {
		t.Fatalf("GetItemsByIDsIncludeDeleted(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d", len(got))
	}
}

func TestListItemsNoContentProjection(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "Has body", "the full markdown body")

	// Default: content loaded.
	full, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(full) != 1 || full[0].Content != "the full markdown body" {
		t.Fatalf("expected content loaded by default, got %+v", full)
	}

	// NoContent: body omitted, everything else intact.
	skinny, err := s.ListItems(ws.ID, models.ItemListParams{NoContent: true})
	if err != nil {
		t.Fatalf("ListItems NoContent: %v", err)
	}
	if len(skinny) != 1 {
		t.Fatalf("expected 1 item, got %d", len(skinny))
	}
	if skinny[0].Content != "" {
		t.Errorf("expected empty content with NoContent, got %q", skinny[0].Content)
	}
	if skinny[0].Title != "Has body" {
		t.Errorf("expected title preserved, got %q", skinny[0].Title)
	}
}
