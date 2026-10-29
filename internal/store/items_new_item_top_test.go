package store

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4005: every create path writes a PARENTLESS item at sort_order =
// MIN(sort_order) - 1 over the collection's parentless live items, so it lands
// on top of a lane; a child is created at 0 and appends under its parent.
// Dispositions: export.go import keeps the exported
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

// createChild mirrors createItemChecked: the create is told it is a child
// (UnderParent), then the parent link is written as a second step.
func createChild(t *testing.T, s *Store, wsID, colID, parentID, title string) models.Item {
	t.Helper()
	it, err := s.CreateItem(wsID, colID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`, UnderParent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetParentLink(wsID, it.ID, parentID, "test"); err != nil {
		t.Fatal(err)
	}
	// created_at has one-second resolution, so children created in one test
	// second tie, and a tie is ordered arbitrarily on Postgres. Space them a
	// second apart, as sequential creates in real use are.
	childSeq++
	stamp := time.Date(2026, 1, 1, 0, 0, childSeq, 0, time.UTC).Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`UPDATE items SET created_at = ? WHERE id = ?`), stamp, it.ID); err != nil {
		t.Fatal(err)
	}
	return *it
}

var childSeq int

// A new child JOINS THE END of its siblings: ship and decompose rely on it.
func TestNewItemTop_ChildJoinsTheEnd(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Children")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	parent := createTestItem(t, s, ws.ID, col.ID, "parent", "")
	createChild(t, s, ws.ID, col.ID, parent.ID, "c1")
	createChild(t, s, ws.ID, col.ID, parent.ID, "c2")
	fresh := createChild(t, s, ws.ID, col.ID, parent.ID, "fresh")
	if got := sortOrderOf(t, s, fresh.ID); got != 0 {
		t.Errorf("child sort_order = %d, want 0", got)
	}
	kids, err := s.getChildItems(s.db, parent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 || kids[2].ID != fresh.ID {
		t.Errorf("getChildItems: new child not last: %v", titles(kids))
	}
	byParent, err := s.GetChildItemsForParents([]string{parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if k := byParent[parent.ID]; len(k) != 3 || k[2].ID != fresh.ID {
		t.Errorf("GetChildItemsForParents: new child not last: %v", titles(k))
	}
}

// The ship playbook expands a plan into its children "ordered by sort_order"
// and works them in that order; decompose creates them in sequence.
func TestNewItemTop_ShipOrderAfterDecompose(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ship Order")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	plan := createTestItem(t, s, ws.ID, col.ID, "plan", "")
	// Parentless siblings elsewhere in the collection at very negative orders.
	other := createTestItem(t, s, ws.ID, col.ID, "other", "")
	setSortOrder(t, s, other.ID, -50)
	for _, n := range []string{"T1", "T2", "T3"} {
		createChild(t, s, ws.ID, col.ID, plan.ID, n)
	}
	kids, err := s.getChildItems(s.db, plan.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(kids); len(got) != 3 || got[0] != "T1" || got[1] != "T2" || got[2] != "T3" {
		t.Errorf("children in sort_order, created_at order = %v, want [T1 T2 T3]", got)
	}
}

// The MIN covers PARENTLESS live items only: children with very negative
// sort_order do not push a parentless create further down.
func TestNewItemTop_ParentlessMinIgnoresChildren(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Mixed")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	parent := createTestItem(t, s, ws.ID, col.ID, "parent", "")
	setSortOrder(t, s, parent.ID, 4)
	kid := createChild(t, s, ws.ID, col.ID, parent.ID, "kid")
	setSortOrder(t, s, kid.ID, -100)
	fresh := createTestItem(t, s, ws.ID, col.ID, "fresh", "")
	if got := sortOrderOf(t, s, fresh.ID); got != 3 {
		t.Errorf("parentless sort_order = %d, want parentless min - 1 = 3", got)
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

// The reorder API accepts the INTEGER floor; MIN - 1 would overflow Postgres.
func TestNewItemTop_ClampsAtTheIntegerFloor(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Floor")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "floor", "")
	setSortOrder(t, s, it.ID, math.MinInt32)
	fresh, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "fresh", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("create at the floor: %v", err)
	}
	if got := sortOrderOf(t, s, fresh.ID); got != math.MinInt32 {
		t.Errorf("sort_order = %d, want the floor %d", got, math.MinInt32)
	}
}
