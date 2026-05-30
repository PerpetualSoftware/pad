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

// TestListMovedOutSince_PaginatesByMoveSeq is the BUG-1675 round-2
// regression: pagination must order/limit by the MOVE seq, not the
// item's current seq. An item that moved out early but then churned in
// the hidden collection (high current seq) must not be dropped past the
// limit while the cursor advances beyond its move seq.
func TestListMovedOutSince_PaginatesByMoveSeq(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MovedOutPaging")
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	visible, _ := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Visible", Schema: schema})
	hidden, _ := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Hidden", Schema: schema})

	logMove := func(id string, seq int64) {
		if _, err := s.CreateActivity(models.Activity{
			WorkspaceID: ws.ID, DocumentID: id, Action: "moved",
			Metadata: `{"from_collection":"` + visible.Slug + `","to_collection":"` + hidden.Slug +
				`","seq":"` + strconv.FormatInt(seq, 10) + `"}`,
		}); err != nil {
			t.Fatalf("log move: %v", err)
		}
	}

	// Three items move out in order A, B, C (ascending move seq).
	a, _ := s.CreateItem(ws.ID, visible.ID, models.ItemCreate{Title: "A", Fields: `{"status":"open"}`})
	b, _ := s.CreateItem(ws.ID, visible.ID, models.ItemCreate{Title: "B", Fields: `{"status":"open"}`})
	c, _ := s.CreateItem(ws.ID, visible.ID, models.ItemCreate{Title: "C", Fields: `{"status":"open"}`})
	movedA, _ := s.MoveItem(a.ID, hidden.ID, `{"status":"open"}`)
	logMove(a.ID, movedA.Seq)
	movedB, _ := s.MoveItem(b.ID, hidden.ID, `{"status":"open"}`)
	logMove(b.ID, movedB.Seq)
	movedC, _ := s.MoveItem(c.ID, hidden.ID, `{"status":"open"}`)
	logMove(c.ID, movedC.Seq)

	// A then churns in the hidden collection — its CURRENT seq leaps past
	// B and C, but its MOVE seq is still the smallest.
	if _, err := s.UpdateItem(a.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("churn A: %v", err)
	}

	vis := []string{visible.ID}
	slugs := map[string]bool{visible.Slug: true}

	// limit=2: must return the two SMALLEST move seqs (A, B) — A must
	// not be starved by its high current seq.
	page1, _ := s.ListMovedOutSince(ws.ID, 0, 2, vis, slugs, nil)
	if len(page1) != 2 {
		t.Fatalf("page1: expected 2 rows, got %d (%+v)", len(page1), page1)
	}
	if page1[0].ID != a.ID || page1[0].Seq != movedA.Seq {
		t.Errorf("page1[0] should be A@%d, got %s@%d", movedA.Seq, page1[0].ID, page1[0].Seq)
	}
	if page1[1].ID != b.ID || page1[1].Seq != movedB.Seq {
		t.Errorf("page1[1] should be B@%d, got %s@%d", movedB.Seq, page1[1].ID, page1[1].Seq)
	}

	// Next page from the last cursor → C, with no gap.
	page2, _ := s.ListMovedOutSince(ws.ID, page1[1].Seq, 2, vis, slugs, nil)
	if len(page2) != 1 || page2[0].ID != c.ID {
		t.Fatalf("page2: expected [C], got %+v", page2)
	}
}
