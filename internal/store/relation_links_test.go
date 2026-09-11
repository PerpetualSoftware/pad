package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The materialised reverse index for relation values (PLAN-2857 U5 /
// TASK-2997). Q3's proving test, leg for leg.
//
// EVERY LEG HERE IS A WIRING CLAIM. The index is only ever as good as the
// hooks that maintain it, so a test that exercised the helper directly would
// vouch for nothing — these go through the real write paths (CreateItem,
// UpdateItem, DeleteItem, RestoreItem, MoveItem, UpdateCollection) and read
// through the real query. The counterfactual that proves they are wired lives
// in TestRelationLinks_RemovedHookGoesStale at the bottom.

// unrestricted is the visibility vector for a viewer who sees everything. The
// viewer-scoped legs build their own.
var unrestricted = BacklinksVisibility{Unrestricted: true}

// relationIndexFixture returns a workspace with Cars (carrying a `color`
// relation) and Colors, plus one colour to point at.
func relationIndexFixture(t *testing.T, s *Store) (*models.Workspace, *models.Collection, *models.Collection, *models.Item) {
	t.Helper()
	return relationFixture(t, s)
}

// countFor is the reverse count a viewer who sees everything gets.
func countFor(t *testing.T, s *Store, target *models.Item, ws *models.Workspace) int {
	t.Helper()
	n, err := s.CountRelationBacklinks(target.ID, ws.ID, unrestricted)
	if err != nil {
		t.Fatalf("CountRelationBacklinks(%s): %v", target.Title, err)
	}
	return n
}

// carPointingAt creates an item in `cars` whose `color` relation names target.
func carPointingAt(t *testing.T, s *Store, ws *models.Workspace, cars *models.Collection, title, targetID string) *models.Item {
	t.Helper()
	blob, _ := json.Marshal(map[string]any{"status": "open", "color": targetID})
	it, err := s.CreateItem(ws.ID, cars.ID, models.ItemCreate{Title: title, Fields: string(blob)})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	return it
}

// TestRelationLinks_WriteThenReadCountsTheSource is the first half of Q3's
// property: a written relation value shows up on the target's reverse side.
func TestRelationLinks_WriteThenReadCountsTheSource(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)

	if n := countFor(t, s, red, ws); n != 0 {
		t.Fatalf("a fresh target is referenced by %d, want 0 — the fixture is not clean", n)
	}
	car := carPointingAt(t, s, ws, cars, "Red Car", red.ID)
	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("after a write the target is referenced by %d, want 1", n)
	}

	links, err := s.GetRelationBacklinks(red.ID, ws.ID, 50, 0, unrestricted)
	if err != nil {
		t.Fatalf("GetRelationBacklinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("want 1 backlink, got %d", len(links))
	}
	if links[0].SourceItemID != car.ID {
		t.Errorf("source = %s, want %s", links[0].SourceItemID, car.ID)
	}
	// The field key is the whole reason this index is its own table: the
	// reverse side must be able to say WHICH field points here.
	if links[0].FieldKey != "color" {
		t.Errorf("field key = %q, want %q", links[0].FieldKey, "color")
	}
	if links[0].SourceRef != car.Ref || links[0].SourceTitle != "Red Car" {
		t.Errorf("backlink lost the source's identity: %+v", links[0])
	}
}

// TestRelationLinks_ChangingTheValueMovesTheEdge is Q3's BOTH HALVES leg.
//
// A writer that only ever INSERTs passes "the new target counts it". Only the
// old target dropping to zero proves the edge MOVED rather than accumulated,
// and that is the half a naive hook fails.
func TestRelationLinks_ChangingTheValueMovesTheEdge(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, cars, red := relationIndexFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")

	car := carPointingAt(t, s, ws, cars, "Repainted Car", red.ID)
	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("setup: red referenced by %d, want 1", n)
	}

	blob, _ := json.Marshal(map[string]any{"status": "open", "color": blue.ID})
	fields := string(blob)
	if _, err := s.UpdateItem(car.ID, models.ItemUpdate{Fields: &fields}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	if n := countFor(t, s, red, ws); n != 0 {
		t.Errorf("the OLD target is still referenced by %d, want 0 — the hook inserts but does not remove, so edges accumulate", n)
	}
	if n := countFor(t, s, blue, ws); n != 1 {
		t.Errorf("the NEW target is referenced by %d, want 1", n)
	}
}

// TestRelationLinks_SoftDeletedSourceDropsOutAndRestoreBringsItBack.
//
// store/items.go documents that a restore resurrects an item's relationships;
// the reverse side has to match, or a restored item is silently missing from
// the pages that referenced it.
func TestRelationLinks_SoftDeletedSourceDropsOutAndRestoreBringsItBack(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)
	car := carPointingAt(t, s, ws, cars, "Disappearing Car", red.ID)

	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("setup: referenced by %d, want 1", n)
	}
	if err := s.DeleteItem(car.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Errorf("a soft-deleted source still counts (%d); the reverse side must not cite an item nobody can open", n)
	}
	if _, err := s.RestoreItem(car.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("after restore the source is referenced %d times, want 1 — restore resurrects relationships and the reverse side must agree", n)
	}
}

// TestRelationLinks_SoftDeletedTargetKeepsItsRows asserts on the STORE's
// answer rather than on a chip's wording.
//
// Q3's row phrases this leg as "the source's chip renders (deleted)". That
// chip is U2's, and BUG-3013 is that "deleted" overclaims when gone and hidden
// are indistinguishable — so pinning the word here would couple this unit to a
// wording that is already scheduled to change. What U5 owes is that the EDGE
// survives: the question "who pointed at this?" still has an answer after the
// target is deleted.
func TestRelationLinks_SoftDeletedTargetKeepsItsRows(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)
	carPointingAt(t, s, ws, cars, "Orphaned Car", red.ID)

	if err := s.DeleteItem(red.ID); err != nil {
		t.Fatalf("DeleteItem(target): %v", err)
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("deleting the TARGET dropped its reverse side to %d; the edge belongs to the source, which still points here", n)
	}
}

// TestRelationLinks_MoveReindexesAgainstTheDestinationSchema.
//
// A move is the one write where the blob and the SCHEMA change together. The
// same stored value indexes differently on the other side, because which keys
// are relations belongs to the COLLECTION.
func TestRelationLinks_MoveReindexesAgainstTheDestinationSchema(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)
	car := carPointingAt(t, s, ws, cars, "Moving Car", red.ID)
	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("setup: referenced by %d, want 1", n)
	}

	// A collection with NO relation field: the same blob carries no edge here.
	plain, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Plain",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// The blob travels unchanged; only the collection differs.
	moved, _ := json.Marshal(map[string]any{"status": "open", "color": red.ID})
	if _, err := s.MoveItem(car.ID, plain.ID, string(moved)); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Errorf("after moving into a collection with no relation field the target is still referenced %d times — the move indexed against the collection it LEFT", n)
	}
}

// TestRelationLinks_SchemaChangeReindexesTheCollection is the lead's D2 ruling
// as a test.
//
// Declaring a relation field over values that already exist must index them.
// Nothing writes those items, so "it fixes itself on the next write" would
// leave the index wrong indefinitely — the same argument that made this index
// materialised rather than derived.
func TestRelationLinks_SchemaChangeReindexesTheCollection(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationIndexFixture(t, s)

	// A collection where `color` is plain TEXT, holding what is already a
	// valid item id.
	garages, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Garages",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"text"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	blob, _ := json.Marshal(map[string]any{"status": "open", "color": red.ID})
	if _, err := s.CreateItem(ws.ID, garages.ID, models.ItemCreate{Title: "Garage", Fields: string(blob)}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Fatalf("a TEXT field must not be indexed as a relation (count %d)", n)
	}

	// Now declare it a relation. No item is written.
	reshaped := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"relation","collection":"colors"}]}`
	if _, err := s.UpdateCollection(garages.ID, models.CollectionUpdate{Schema: &reshaped}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("after the field became a relation the target is referenced %d times, want 1 — nothing writes these items, so only the schema-change reindex can find them", n)
	}

	// And the reverse direction: retyping it away must remove the edges.
	back := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"text"}]}`
	if _, err := s.UpdateCollection(garages.ID, models.CollectionUpdate{Schema: &back}); err != nil {
		t.Fatalf("UpdateCollection(back): %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Errorf("after retyping the field away from relation the target is still referenced %d times, want 0", n)
	}
}

// TestRelationLinks_BackfillDerivesRowsWrittenBeforeTheIndexExisted is Q3's
// backfill leg. The table is emptied to simulate a pre-migration database.
func TestRelationLinks_BackfillDerivesRowsWrittenBeforeTheIndexExisted(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)
	carPointingAt(t, s, ws, cars, "Pre-index Car", red.ID)

	// Simulate the pre-migration state: the rows exist, the index does not,
	// and no completed pass has been recorded.
	if _, err := s.db.Exec(s.q(`DELETE FROM item_relation_links`)); err != nil {
		t.Fatalf("clear index: %v", err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM platform_settings WHERE key = ?`), relationLinksBackfilledFlag); err != nil {
		t.Fatalf("clear backfill marker: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Fatalf("the index was not actually cleared (count %d), so the leg below would pass without a backfill", n)
	}

	res, err := s.BackfillRelationLinks()
	if err != nil {
		t.Fatalf("BackfillRelationLinks: %v", err)
	}
	if res.Skipped {
		t.Fatal("the backfill skipped: it must not short-circuit when the table is empty and a collection declares a relation field")
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("after the backfill the target is referenced %d times, want 1", n)
	}

	// Second run short-circuits, which is what keeps steady-state boots cheap.
	again, err := s.BackfillRelationLinks()
	if err != nil {
		t.Fatalf("BackfillRelationLinks (second): %v", err)
	}
	if !again.Skipped {
		t.Errorf("the second run re-derived instead of short-circuiting (%+v); every boot would pay a full scan", again)
	}
}

// TestRelationLinks_ImportIndexesTheRemappedIDs covers the import hook, which
// none of the legs above reach.
//
// Two things have to be true and only one is obvious. The edge must be indexed
// at all — and it must be indexed with the DESTINATION's id, not the exported
// one. The import rewrites relation values to the new ids in a SECOND pass, so
// a hook at the first-pass INSERT would index ids belonging to the workspace
// that produced the bundle: rows that match nothing, on an index whose whole
// job is matching.
func TestRelationLinks_ImportIndexesTheRemappedIDs(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Name: "Owner", Email: "relation-index-import@example.com", Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const oldColorID = "old-color-indexed"
	export := &models.WorkspaceExport{
		Version:    1,
		ExportedAt: "2026-09-11T00:00:00Z",
		Workspace:  models.WorkspaceExportMeta{Name: "Index Archive", Slug: "index-archive"},
		Collections: []models.CollectionExport{
			{
				ID: "old-coll-colors", Name: "Colors", Slug: "colors", Prefix: "COLO",
				Schema:    `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z",
			},
			{
				ID: "old-coll-cars", Name: "Cars", Slug: "cars", Prefix: "CAR",
				Schema:    `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"relation","collection":"colors"}]}`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z",
			},
		},
		Items: []models.ItemExport{
			{
				ID: oldColorID, CollectionID: "old-coll-colors", Title: "Imported Red", Slug: "imported-red",
				Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z",
			},
			{
				ID: "old-car-indexed", CollectionID: "old-coll-cars", Title: "Imported Car", Slug: "imported-car",
				Fields: `{"color":"` + oldColorID + `"}`, Tags: `[]`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z",
			},
		},
	}

	ws, err := s.ImportWorkspace(export, "Index Archive Target", owner.ID, "")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var red, car models.Item
	for _, it := range items {
		switch it.Title {
		case "Imported Red":
			red = it
		case "Imported Car":
			car = it
		}
	}
	if red.ID == "" || car.ID == "" {
		t.Fatalf("the bundle did not import both items; the assertion below would be vacuous (got %d items)", len(items))
	}

	n, err := s.CountRelationBacklinks(red.ID, ws.ID, unrestricted)
	if err != nil {
		t.Fatalf("CountRelationBacklinks: %v", err)
	}
	if n != 1 {
		t.Fatalf("the imported target is referenced %d times, want 1 — the import hook did not run, or ran before the id remap", n)
	}

	links, err := s.GetRelationBacklinks(red.ID, ws.ID, 50, 0, unrestricted)
	if err != nil {
		t.Fatalf("GetRelationBacklinks: %v", err)
	}
	if len(links) != 1 || links[0].SourceItemID != car.ID {
		t.Errorf("backlink = %+v, want the DESTINATION's car id %s — an index built at the first-pass insert would hold the exported id instead", links, car.ID)
	}
}

// TestRelationLinks_RestoreAfterASchemaChangeReindexes is codex round 1's
// second P1, and it is a hole my own site enumeration could not have found.
//
// I enumerated the sites that WRITE items.fields. Restore writes no fields —
// it clears deleted_at — so it never appeared. But the index depends on
// (blob, schema) AND on the item being live, and the collection reindex
// deliberately skips soft-deleted items. So: delete an item, change the
// collection's schema while it is gone, restore it, and its rows are whatever
// they were before the deletion — stale against a schema that has moved.
//
// Both directions matter. A field that BECAME a relation leaves the restored
// item missing edges; a field that stopped being one leaves it with edges it
// should not have.
func TestRelationLinks_RestoreAfterASchemaChangeReindexes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationIndexFixture(t, s)

	car := carPointingAt(t, s, ws, cars, "Time Traveller", red.ID)
	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("setup: referenced by %d, want 1", n)
	}

	// Gone while the schema moves under it.
	if err := s.DeleteItem(car.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	plain := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"text"}]}`
	if _, err := s.UpdateCollection(cars.ID, models.CollectionUpdate{Schema: &plain}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}

	if _, err := s.RestoreItem(car.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	if n := countFor(t, s, red, ws); n != 0 {
		t.Errorf("the restored item is referenced %d times, want 0 — `color` is a TEXT field now, and the reindex that would have noticed skipped this item because it was deleted at the time", n)
	}

	// The other direction: make it a relation again while the item is gone,
	// and the restore must FIND the edge.
	if err := s.DeleteItem(car.ID); err != nil {
		t.Fatalf("DeleteItem (second): %v", err)
	}
	back := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"},{"key":"color","type":"relation","collection":"colors"}]}`
	if _, err := s.UpdateCollection(cars.ID, models.CollectionUpdate{Schema: &back}); err != nil {
		t.Fatalf("UpdateCollection (back): %v", err)
	}
	if _, err := s.RestoreItem(car.ID); err != nil {
		t.Fatalf("RestoreItem (second): %v", err)
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("the restored item is referenced %d times, want 1 — `color` is a relation again and the restore must derive the edge the reindex could not", n)
	}
}

// TestRelationLinks_AnInterruptedBackfillIsRetriedNotSkipped is codex round
// 1's first P1.
//
// The guard used to be "does item_relation_links have any row". Rows commit
// per item, so a crash partway through leaves the table NON-EMPTY AND
// INCOMPLETE — and that guard then skips forever, with the missing edges never
// derived. The rebuild story ("delete the table") only ever covered deliberate
// deletion.
//
// Completion is now recorded explicitly and only after a full pass, so a table
// that is partially populated with no marker must be RE-DERIVED.
func TestRelationLinks_AnInterruptedBackfillIsRetriedNotSkipped(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, cars, red := relationIndexFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")

	carPointingAt(t, s, ws, cars, "Indexed Car", red.ID)
	carPointingAt(t, s, ws, cars, "Missed Car", blue.ID)

	// The shape an interrupted run leaves behind: one item's rows present,
	// another's missing, and NO completion marker.
	if _, err := s.db.Exec(s.q(`DELETE FROM item_relation_links WHERE target_item_id = ?`), blue.ID); err != nil {
		t.Fatalf("simulate partial index: %v", err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM platform_settings WHERE key = ?`), relationLinksBackfilledFlag); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if n := countFor(t, s, blue, ws); n != 0 {
		t.Fatalf("the partial state was not established (blue count %d); the assertion below would pass for the wrong reason", n)
	}
	if n := countFor(t, s, red, ws); n != 1 {
		t.Fatalf("the partial state removed too much (red count %d)", n)
	}

	res, err := s.BackfillRelationLinks()
	if err != nil {
		t.Fatalf("BackfillRelationLinks: %v", err)
	}
	if res.Skipped {
		t.Fatal("the backfill SKIPPED a partially populated table; under the old any-row guard the missing edges would never be derived")
	}
	if n := countFor(t, s, blue, ws); n != 1 {
		t.Errorf("the missed edge is still missing (count %d) after a retry", n)
	}
	// And the already-indexed one is not duplicated — the pass is idempotent
	// because replaceRelationLinks deletes before it inserts.
	if n := countFor(t, s, red, ws); n != 1 {
		t.Errorf("the already-indexed edge is now counted %d times; a repeat pass must replace rather than append", n)
	}
}
