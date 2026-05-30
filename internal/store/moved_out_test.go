package store

import (
	"strconv"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListMovedOutSince covers BUG-1675: an item that moves from a
// collection the caller can see into one they can't must surface as a
// moved-out tombstone so the caller's local cache evicts it.
func TestListMovedOutSince(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MovedOutTest")

	visible, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Visible",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create visible collection: %v", err)
	}
	hidden, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Hidden",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create hidden collection: %v", err)
	}

	item, err := s.CreateItem(ws.ID, visible.ID, models.ItemCreate{
		Title:  "Item that will move out",
		Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Move it into the hidden collection and log the move activity the
	// way the HTTP handlers do (the store move itself doesn't log).
	moved, err := s.MoveItem(item.ID, hidden.ID, `{"status":"open"}`)
	if err != nil {
		t.Fatalf("move item: %v", err)
	}
	if _, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  item.ID,
		Action:      "moved",
		Metadata: `{"from_collection":"` + visible.Slug + `","to_collection":"` + hidden.Slug +
			`","seq":"` + strconv.FormatInt(moved.Seq, 10) + `"}`,
	}); err != nil {
		t.Fatalf("log move activity: %v", err)
	}

	visibleSlugs := map[string]bool{visible.Slug: true}

	// Caller who can see the source collection but not the target:
	// the moved item must surface as a moved-out tombstone.
	rows, err := s.ListMovedOutSince(ws.ID, 0, 0, []string{visible.ID}, visibleSlugs, nil)
	if err != nil {
		t.Fatalf("ListMovedOutSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 moved-out row, got %d (%+v)", len(rows), rows)
	}
	if rows[0].ID != item.ID {
		t.Errorf("expected item %s, got %s", item.ID, rows[0].ID)
	}
	if rows[0].Seq != moved.Seq {
		t.Errorf("expected seq %d (post-move), got %d", moved.Seq, rows[0].Seq)
	}

	// since past the move's seq → no longer in the window.
	rows, _ = s.ListMovedOutSince(ws.ID, moved.Seq, 0, []string{visible.ID}, visibleSlugs, nil)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows when since >= move seq, got %d", len(rows))
	}

	// Re-fire regression (BUG-1675, Codex round 1): a later change while
	// the item sits in the hidden collection bumps its current seq, but
	// the tombstone is keyed on the MOVE's seq — so a caller who already
	// consumed the tombstone (since = move seq) must NOT get it again.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("post-move update: %v", err)
	}
	rows, _ = s.ListMovedOutSince(ws.ID, moved.Seq, 0, []string{visible.ID}, visibleSlugs, nil)
	if len(rows) != 0 {
		t.Errorf("tombstone must not re-fire on a later hidden-collection change; got %d rows", len(rows))
	}

	// Caller who can see the TARGET collection: the item is visible to
	// them via the main delta, so it must NOT be a moved-out tombstone.
	rows, _ = s.ListMovedOutSince(ws.ID, 0, 0, []string{hidden.ID}, map[string]bool{hidden.Slug: true}, nil)
	if len(rows) != 0 {
		t.Errorf("caller who sees target should get 0 moved-out rows, got %d", len(rows))
	}

	// Caller holding a direct grant on the item: excluded (grant
	// transcends collection; the main delta still delivers it).
	rows, _ = s.ListMovedOutSince(ws.ID, 0, 0, []string{visible.ID}, visibleSlugs, []string{item.ID})
	if len(rows) != 0 {
		t.Errorf("granted item must be excluded from moved-out, got %d rows", len(rows))
	}

	// Caller whose visible source set doesn't include the from-collection
	// (e.g. they see some other collection): no match.
	rows, _ = s.ListMovedOutSince(ws.ID, 0, 0, []string{hidden.ID}, map[string]bool{"something-else": true}, nil)
	if len(rows) != 0 {
		t.Errorf("from-collection not visible should yield 0 rows, got %d", len(rows))
	}

	// Empty visible scope → nil (unrestricted/no-source callers skip).
	rows, _ = s.ListMovedOutSince(ws.ID, 0, 0, nil, nil, nil)
	if rows != nil {
		t.Errorf("empty scope should return nil, got %+v", rows)
	}
}
