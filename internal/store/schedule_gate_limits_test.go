package store

import (
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// gateFanBundle is one item blocked by n others.
func gateFanBundle(n int) *models.WorkspaceExport {
	const ts = "2026-10-15T00:00:00Z"
	id := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1) }
	b := &models.WorkspaceExport{
		Version:   models.WorkspaceExportVersion,
		Workspace: models.WorkspaceExportMeta{Name: "Gate Fan", Slug: "gate-fan"},
		Collections: []models.CollectionExport{{
			ID: "old-coll", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`, CreatedAt: ts, UpdatedAt: ts,
		}},
	}
	for i := 0; i <= n; i++ {
		b.Items = append(b.Items, models.ItemExport{ID: id(i), CollectionID: "old-coll", Title: fmt.Sprintf("Item %d", i), Slug: fmt.Sprintf("item-%d", i),
			Fields: `{}`, Tags: `[]`, ItemNumber: i + 1, CreatedAt: ts, UpdatedAt: ts})
	}
	for i := 1; i <= n; i++ {
		b.ScheduleGates = append(b.ScheduleGates, models.ScheduleGateExport{ID: fmt.Sprintf("gate-%d", i), BlockedItemID: id(0), BlockerItemID: id(i), CreatedAt: ts})
	}
	return b
}

func TestTASK4423_R10ImportHoldsTheGateLimitOfAnItem(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if _, _, err := s.ImportWorkspaceWithReport(gateFanBundle(maxGatesPerItem), "Fan at limit", "", ""); err != nil {
		t.Fatalf("import at the limit: %v", err)
	}
	_, _, err := s.ImportWorkspaceWithReport(gateFanBundle(maxGatesPerItem+1), "Fan past limit", "", "")
	if _, ok := AsValidationError(err); !ok {
		t.Fatalf("import past the limit: err = %v, want a validation refusal", err)
	}
	ws, err := s.GetWorkspaceBySlug("fan-past-limit")
	if err != nil {
		t.Fatal(err)
	}
	if ws != nil {
		t.Fatalf("a refused import left workspace %q", ws.Slug)
	}
}

func TestTASK4423_R10ImportStoresNoTombstoneBody(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	bundle := bug3825Export("code")
	bundle.Version = models.WorkspaceExportVersion
	bundle.Items[0].DeletedAt = "2026-10-16T00:00:00Z"
	bundle.Items[0].Content = "a body in a deleted row, linking [[Second]]"
	bundle.Items[1].Content = "a live body"
	gateOnTombstone(bundle, bundle.Items[0], bundle.Items[1])
	ws, _, err := s.ImportWorkspaceWithReport(bundle, "Hollow tombstone", "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var deleted, live string
	if err := s.db.QueryRow(s.q(`SELECT content FROM items WHERE workspace_id=? AND deleted_at IS NOT NULL`), ws.ID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(s.q(`SELECT content FROM items WHERE workspace_id=? AND deleted_at IS NULL`), ws.ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if deleted != "" || live != "a live body" {
		t.Fatalf("deleted body = %q, live body = %q", deleted, live)
	}
	var links int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_wiki_links wl JOIN items i ON i.id=wl.source_item_id WHERE i.workspace_id=?`), ws.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 0 {
		t.Fatalf("%d wiki links indexed from a body that was not stored", links)
	}
}
