package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The reverse index and `multi_relation` (PLAN-2857 U4, population-table rows
// 20-22).
//
// These rows were OPEN on the table as questions, not as work: U5 built the
// extractor with a `case []any:` arm and an `ordinal` column "so U4 does not
// need an ALTER", and `relationKeysFromSchemaJSON` already matched
// `multi_relation`. Reading that is not the same as knowing it, so these legs
// pin it — CONVE-34's "a green instrument is evidence only once it has been
// shown able to go red".

func multiIndexFixture(t *testing.T, s *Store) (*models.Workspace, *models.Collection, *models.Item, *models.Item) {
	t.Helper()
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")
	return ws, colors, red, blue
}

func linkRowsFor(t *testing.T, s *Store, itemID string) []struct {
	Key     string
	Target  string
	Ordinal int
} {
	t.Helper()
	rows, err := s.db.Query(s.q(`
		SELECT source_field_key, target_item_id, ordinal
		FROM item_relation_links
		WHERE source_item_id = ?
		ORDER BY source_field_key, ordinal`), itemID)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Key     string
		Target  string
		Ordinal int
	}
	for rows.Next() {
		var r struct {
			Key     string
			Target  string
			Ordinal int
		}
		if err := rows.Scan(&r.Key, &r.Target, &r.Ordinal); err != nil {
			t.Fatalf("scan index row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// ROW 20: a multi_relation write indexes ONE ROW PER ELEMENT, with the array
// position in `ordinal`.
func TestRelationIndex_MultiRelationIndexesEveryElementWithItsOrdinal(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, red, blue := multiIndexFixture(t, s)

	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Fleet",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"colors","type":"multi_relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	car := createTestItem(t, s, ws.ID, cars.ID, "Car", "")
	if _, err := s.UpdateItem(car.ID, models.ItemUpdate{
		Fields: strPtr(`{"status":"open","colors":["` + blue.ID + `","` + red.ID + `"]}`),
	}); err != nil {
		t.Fatalf("set colors: %v", err)
	}

	got := linkRowsFor(t, s, car.ID)
	if len(got) != 2 {
		t.Fatalf("indexed %d rows, want one per element: %+v", len(got), got)
	}
	// Ordinal carries the ARRAY POSITION, so the index can answer "referenced
	// by X via `colors`, second" and not merely "referenced".
	if got[0].Target != blue.ID || got[0].Ordinal != 0 {
		t.Errorf("row 0 = %+v, want target %s at ordinal 0", got[0], blue.ID)
	}
	if got[1].Target != red.ID || got[1].Ordinal != 1 {
		t.Errorf("row 1 = %+v, want target %s at ordinal 1", got[1], red.ID)
	}

	// THE CONTROL for the ordinal assertion: reversing the stored order must
	// reverse the ordinals. Without this leg, an extractor that emitted a
	// constant 0 for every element would pass only if the fixture happened to
	// be sorted the same way.
	if _, err := s.UpdateItem(car.ID, models.ItemUpdate{
		Fields: strPtr(`{"status":"open","colors":["` + red.ID + `","` + blue.ID + `"]}`),
	}); err != nil {
		t.Fatalf("reverse colors: %v", err)
	}
	got = linkRowsFor(t, s, car.ID)
	if len(got) != 2 || got[0].Target != red.ID || got[1].Target != blue.ID {
		t.Errorf("after reversing the array the index reads %+v; ordinal must follow the stored order", got)
	}
}

// ROW 21: the BACKFILL handles the array form, not just the write hook.
//
// The two paths derive edges differently enough to be worth separating — the
// write hook runs per item write, the backfill walks every live item from a
// clean table — and the migration's own comment says a derived query "on a
// multi_relation array silently matches nothing", which is easy to misread as
// being about this path. It is not: the backfill reaches the same Go extractor.
func TestRelationIndex_BackfillCoversMultiRelationArrays(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, red, blue := multiIndexFixture(t, s)

	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Fleet",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"colors","type":"multi_relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("create fleet: %v", err)
	}
	car := createTestItem(t, s, ws.ID, cars.ID, "Car", "")
	if _, err := s.UpdateItem(car.ID, models.ItemUpdate{
		Fields: strPtr(`{"status":"open","colors":["` + red.ID + `","` + blue.ID + `"]}`),
	}); err != nil {
		t.Fatalf("set colors: %v", err)
	}

	// Empty the index by hand, so the backfill has something to rebuild. This
	// is the PRECONDITION the test would otherwise lack: without it a backfill
	// that did nothing at all would pass, because the write hook already
	// populated these rows.
	if _, err := s.db.Exec(s.q(`DELETE FROM item_relation_links WHERE source_item_id = ?`), car.ID); err != nil {
		t.Fatalf("clear index: %v", err)
	}
	if rows := linkRowsFor(t, s, car.ID); len(rows) != 0 {
		t.Fatalf("precondition failed: index still holds %d rows", len(rows))
	}
	// The backfill is once-only, guarded by a platform_settings marker; clear it
	// so this test can actually drive the pass.
	if _, err := s.db.Exec(s.q(`DELETE FROM platform_settings WHERE key = ?`), relationLinksBackfilledFlag); err != nil {
		t.Fatalf("clear backfill marker: %v", err)
	}

	res, err := s.BackfillRelationLinks()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Skipped {
		t.Fatal("backfill reported Skipped; the marker was cleared, so this run proves nothing")
	}

	got := linkRowsFor(t, s, car.ID)
	if len(got) != 2 {
		t.Fatalf("backfill rebuilt %d rows for a two-element array: %+v", len(got), got)
	}
	if got[0].Target != red.ID || got[0].Ordinal != 0 || got[1].Target != blue.ID || got[1].Ordinal != 1 {
		t.Errorf("backfill rebuilt %+v, want %s@0 then %s@1", got, red.ID, blue.ID)
	}
}

// ROW 22: a scalar->array RETYPE reindexes against what is STORED, not against
// what the new schema says the shape should be.
//
// The lead called a garbage retype a U4 defect. It is not one, and this records
// why rather than leaving the row open: the extractor is VALUE-shaped — it
// switches on the JSON type it finds — so after a retype the still-scalar blob
// indexes as one row at ordinal 0, which is the honest description of what the
// item holds. The alternative, indexing by the DECLARED cardinality, would
// invent rows the blob does not contain.
func TestRelationIndex_RetypeReindexesWhatIsStored(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, red, _ := multiIndexFixture(t, s)

	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Garage",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"color","type":"relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("create garage: %v", err)
	}
	car := createTestItem(t, s, ws.ID, cars.ID, "Car", "")
	if _, err := s.UpdateItem(car.ID, models.ItemUpdate{
		Fields: strPtr(`{"status":"open","color":"` + red.ID + `"}`),
	}); err != nil {
		t.Fatalf("set color: %v", err)
	}
	before := linkRowsFor(t, s, car.ID)
	if len(before) != 1 || before[0].Ordinal != 0 {
		t.Fatalf("precondition: a scalar relation should index one row at ordinal 0, got %+v", before)
	}

	// RETYPE the field to multi_relation, leaving the stored scalar alone. This
	// runs ReindexCollectionRelationLinks inside the schema-update transaction.
	newSchema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"color","type":"multi_relation","collection":"colors"}]}`
	if _, err := s.UpdateCollection(cars.ID, models.CollectionUpdate{Schema: &newSchema}); err != nil {
		t.Fatalf("retype field: %v", err)
	}

	after := linkRowsFor(t, s, car.ID)
	if len(after) != 1 {
		t.Fatalf("after the retype the index holds %d rows for a still-scalar blob: %+v", len(after), after)
	}
	if after[0].Target != red.ID || after[0].Ordinal != 0 {
		t.Errorf("retype reindexed to %+v, want the stored value %s at ordinal 0 — the index describes the BLOB, not the declared cardinality", after, red.ID)
	}
}
