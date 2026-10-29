package store

import (
	"context"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4005: every create path writes sort_order = MIN(sort_order) - 1 over the
// collection's live items, so a new item lands on top of a lane and of a
// parent's children. Dispositions: export.go import keeps the exported
// sort_order (a restore keeps the order); drag writes are explicit; the
// role_sort_order column is out of scope.

const manualSort = "sort_order:asc,created_at:asc,_id:asc"

func listManual(t *testing.T, s *Store, wsID, colID string) []models.Item {
	t.Helper()
	items, err := s.ListItems(wsID, models.ItemListParams{CollectionSlug: colID, Sort: manualSort})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	return items
}

func sortOrderOf(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT sort_order FROM items WHERE id = ?`), id).Scan(&n); err != nil {
		t.Fatalf("sort_order of %s: %v", id, err)
	}
	return n
}

func setSortOrder(t *testing.T, s *Store, id string, v int) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE items SET sort_order = ? WHERE id = ?`), v, id); err != nil {
		t.Fatal(err)
	}
}

func TestNewItemTop_NeverDraggedLane(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Lane")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	for _, n := range []string{"a", "b", "c"} {
		it := createTestItem(t, s, ws.ID, col.ID, n, "")
		setSortOrder(t, s, it.ID, 0) // legacy rows: all at 0
	}
	fresh := createTestItem(t, s, ws.ID, col.ID, "fresh", "")
	if got := sortOrderOf(t, s, fresh.ID); got != -1 {
		t.Errorf("sort_order = %d, want -1", got)
	}
	items := listManual(t, s, ws.ID, col.Slug)
	if len(items) != 4 || items[0].ID != fresh.ID {
		t.Fatalf("new item is not first: %v", titles(items))
	}
}

func titles(items []models.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

func TestNewItemTop_DraggedLane(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Dragged")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	var ups []ItemSortUpdate
	for i, n := range []string{"a", "b", "c"} {
		it := createTestItem(t, s, ws.ID, col.ID, n, "")
		ups = append(ups, ItemSortUpdate{ItemID: it.ID, SortOrder: i})
	}
	if _, err := s.UpdateItemSortOrders(ws.ID, ups, "batch"); err != nil {
		t.Fatal(err)
	}
	fresh := createTestItem(t, s, ws.ID, col.ID, "fresh", "")
	if got := sortOrderOf(t, s, fresh.ID); got >= 0 {
		t.Errorf("sort_order = %d, want < 0", got)
	}
	items := listManual(t, s, ws.ID, col.Slug)
	if len(items) != 4 || items[0].ID != fresh.ID {
		t.Fatalf("new item is not first: %v", titles(items))
	}
}

func TestNewItemTop_ChildLists(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Children")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	parent := createTestItem(t, s, ws.ID, col.ID, "parent", "")
	for _, n := range []string{"c1", "c2"} {
		it := createTestItem(t, s, ws.ID, col.ID, n, "")
		if _, err := s.SetParentLink(ws.ID, it.ID, parent.ID, "test"); err != nil {
			t.Fatal(err)
		}
		setSortOrder(t, s, it.ID, 0)
	}
	fresh := createTestItem(t, s, ws.ID, col.ID, "fresh", "")
	if _, err := s.SetParentLink(ws.ID, fresh.ID, parent.ID, "test"); err != nil {
		t.Fatal(err)
	}
	kids, err := s.getChildItems(s.db, parent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 || kids[0].ID != fresh.ID {
		t.Errorf("getChildItems: new child not first: %v", titles(kids))
	}
	byParent, err := s.GetChildItemsForParents([]string{parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if k := byParent[parent.ID]; len(k) != 3 || k[0].ID != fresh.ID {
		t.Errorf("GetChildItemsForParents: new child not first: %v", titles(k))
	}
}

func TestNewItemTop_CrossWorkspaceCopy(t *testing.T) {
	f := newCopyFixture(t)
	for _, n := range []string{"x", "y"} {
		it := createTestItem(t, f.s, f.wsB.ID, f.colB.ID, n, "")
		setSortOrder(t, f.s, it.ID, 3)
	}
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "src", "")
	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)
	if got := sortOrderOf(t, f.s, res.Item.ID); got != 2 {
		t.Errorf("copy sort_order = %d, want destination min - 1 = 2", got)
	}
	items := listManual(t, f.s, f.wsB.ID, f.colB.Slug)
	if len(items) != 3 || items[0].ID != res.Item.ID {
		t.Errorf("copy is not first: %v", titles(items))
	}
}

func TestNewItemTop_ConcurrentCreates(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Concurrent")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	seed := createTestItem(t, s, ws.ID, col.ID, "seed", "")
	setSortOrder(t, s, seed.ID, 0)

	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "c", Fields: `{"status":"open"}`})
			errs[i] = err
			if it != nil {
				ids[i] = it.ID
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	a, b := sortOrderOf(t, s, ids[0]), sortOrderOf(t, s, ids[1])
	if a == b {
		t.Errorf("both creates took sort_order %d; want distinct", a)
	}
	if a >= 0 || b >= 0 {
		t.Errorf("sort_orders %d, %d must both be below the old minimum 0", a, b)
	}
}

func TestNewItemTop_FencedCreate(t *testing.T) {
	f := newFenceFixture(t)
	u := createTestUser(t, f.s, "fence@example.com", "Fence", "pw-123456789")
	old := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "old", "")
	setSortOrder(t, f.s, old.ID, 0)
	ftx, err := f.s.BeginFenced(context.Background(), f.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer ftx.Rollback()
	it, err := ftx.CreateItem(FencedItemCreate{
		CollectionID: f.companion.ID, Title: "app", Fields: "{}",
		Actor: FencedActor{Kind: "user", UserID: u.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ftx.tx.QueryRow(f.s.q(`SELECT sort_order FROM items WHERE id = ?`), it.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != -1 {
		t.Errorf("fenced create sort_order = %d, want -1", n)
	}
}

func TestNewItemTop_MoveLandsOnTopOfTarget(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Move")
	src := createTestCollection(t, s, ws.ID, "Src")
	dst := createTestCollection(t, s, ws.ID, "Dst")
	for _, n := range []string{"x", "y"} {
		it := createTestItem(t, s, ws.ID, dst.ID, n, "")
		setSortOrder(t, s, it.ID, 5)
	}
	mv := createTestItem(t, s, ws.ID, src.ID, "mv", "")
	setSortOrder(t, s, mv.ID, 9)
	if _, err := s.MoveItem(mv.ID, dst.ID, `{"status":"open"}`); err != nil {
		t.Fatal(err)
	}
	if got := sortOrderOf(t, s, mv.ID); got != 4 {
		t.Errorf("moved sort_order = %d, want target min - 1 = 4", got)
	}
	items := listManual(t, s, ws.ID, dst.Slug)
	if len(items) != 3 || items[0].ID != mv.ID {
		t.Errorf("moved item is not first: %v", titles(items))
	}
}
