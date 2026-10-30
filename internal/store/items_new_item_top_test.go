package store

import (
	"context"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4005: every create path writes a PARENTLESS item at sort_order =
// MIN(sort_order) - 1 over the collection's parentless live items, so it lands
// on top of a lane; a child is created at 0 and appends under its parent.
// Dispositions: export.go import keeps the exported
// sort_order (a restore keeps the order); drag writes are explicit; the
// role_sort_order column is out of scope.

const manualSort = "sort_order:asc,created_at:asc,_id:asc"

// boardSort is the order the board and the share link render (itemSort.ts,
// handlers_share_links.go): same-second rows break by item_number.
const boardSort = "sort_order:asc,created_at:asc,_num:asc,_id:asc"

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
	return *it
}

// tiedChildren creates one child per title under a new parent and forces the
// same-second tie decompose produces: every child gets ONE created_at value
// (it has one-second resolution), and the ids are random v4 UUIDs, so it
// retries on fresh parents until the ids sort in the REVERSE of creation
// order. Only the item_number tie-break then returns creation order; an id
// or arbitrary order fails (BUG-4005). Returns the parent and the children
// in creation order.
func tiedChildren(t *testing.T, s *Store, wsID, colID string, names ...string) (models.Item, []models.Item) {
	t.Helper()
	for attempt := 0; attempt < 500; attempt++ {
		parent := createTestItem(t, s, wsID, colID, "parent", "")
		kids := make([]models.Item, len(names))
		reversed := true
		for i, n := range names {
			kids[i] = createChild(t, s, wsID, colID, parent.ID, n)
			if i > 0 && kids[i].ID >= kids[i-1].ID {
				reversed = false
			}
		}
		if !reversed {
			continue
		}
		for _, k := range kids[1:] {
			if _, err := s.db.Exec(s.q(`UPDATE items SET created_at = (SELECT created_at FROM items WHERE id = ?) WHERE id = ?`), kids[0].ID, k.ID); err != nil {
				t.Fatal(err)
			}
		}
		return *parent, kids
	}
	t.Fatal("no id set in reverse creation order after 500 attempts")
	return models.Item{}, nil
}

// A new child JOINS THE END of its siblings: ship and decompose rely on it,
// including for children created in the same second.
func TestNewItemTop_ChildJoinsTheEnd(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Top Children")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	parent, kids3 := tiedChildren(t, s, ws.ID, col.ID, "c1", "c2", "fresh")
	fresh := kids3[2]
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
	// Parentless siblings elsewhere in the collection at very negative orders.
	other := createTestItem(t, s, ws.ID, col.ID, "other", "")
	setSortOrder(t, s, other.ID, -50)
	// Same created_at, ids in reverse creation order: only item_number orders them.
	plan, _ := tiedChildren(t, s, ws.ID, col.ID, "T1", "T2", "T3")
	want := []string{"T1", "T2", "T3"}
	kids, err := s.getChildItems(s.db, plan.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(kids); !slices.Equal(got, want) {
		t.Errorf("getChildItems order = %v, want %v", got, want)
	}
	byParent, err := s.GetChildItemsForParents([]string{plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(byParent[plan.ID]); !slices.Equal(got, want) {
		t.Errorf("GetChildItemsForParents order = %v, want %v", got, want)
	}
	listed, err := s.ListChildItems(plan.ID, ChildListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(listed); !slices.Equal(got, want) {
		t.Errorf("ListChildItems order = %v, want %v", got, want)
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

// The fenced create path clamps at the INTEGER floor too.
func TestNewItemTop_FencedCreateClampsAtTheIntegerFloor(t *testing.T) {
	f := newFenceFixture(t)
	u := createTestUser(t, f.s, "fencefloor@example.com", "Fence Floor", "pw-123456789")
	old := createTestItem(t, f.s, f.ws.ID, f.companion.ID, "floor", "")
	setSortOrder(t, f.s, old.ID, math.MinInt32)
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
		t.Fatalf("fenced create at the floor: %v", err)
	}
	var n int
	if err := ftx.tx.QueryRow(f.s.q(`SELECT sort_order FROM items WHERE id = ?`), it.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != math.MinInt32 {
		t.Errorf("fenced create sort_order = %d, want the floor %d", n, math.MinInt32)
	}
}

// Template seeds append, so a fresh workspace lists them in the order the
// template defines: first seeded, first listed (BUG-4005 r7-H1). Seeds are
// created in sequence, so that order is item_number order.
func TestNewItemTop_TemplateSeedsKeepTheirOrder(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Seed Order")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	for _, coll := range []string{"conventions", "playbooks"} {
		items, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: coll, Sort: boardSort})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) < 2 {
			t.Fatalf("%s: want at least two seeds, got %d", coll, len(items))
		}
		for i := 1; i < len(items); i++ {
			if *items[i-1].ItemNumber > *items[i].ItemNumber {
				t.Fatalf("%s: seeds out of template order: %v", coll, titles(items))
			}
		}
	}
}

// An app's artifacts append, so they list in manifest order (BUG-4005 r7-H1).
func TestNewItemTop_ProvisionedArtifactsKeepManifestOrder(t *testing.T) {
	f := task3397Fixture(t)
	d := f.derived("n1")
	want := []string{"First", "Second", "Third"}
	d.Artifacts = nil
	for i, title := range want {
		d.Artifacts = append(d.Artifacts, ProvisionArtifact{Key: string(rune('a' + i)), CollectionID: f.dest.ID, Title: title,
			Fields: map[string]any{"owner": f.target.ID}, RawSHA256: "raw", NormalizedSHA256: "n1"})
	}
	if _, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return d, nil }); err != nil {
		t.Fatalf("provision: %v", err)
	}
	items, err := f.s.ListItems(f.ws.ID, models.ItemListParams{CollectionSlug: f.dest.Slug, Sort: boardSort})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(items); !slices.Equal(got, want) {
		t.Fatalf("artifacts out of manifest order: %v", got)
	}
}
