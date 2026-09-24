package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3014. A stored relation value that is not UUID-shaped was never an id,
// so hydration marks it `stored_as_text` instead of presenting it as an id-only
// entry (which reads as "gone or hidden"). Lead ruling, day 79: the marker is
// decided from the stored bytes alone, never from a lookup; a UUID-shaped value
// is byte-identical to v0.49; and a UUID that fails to resolve is NOT marked.

func TestHydrateRelationTargets_MarksStoredTextOnlyForNonUUIDValues(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, red := relationFixture(t, s)

	mk := func(label, value string) models.Item {
		it := createTestItem(t, s, ws.ID, cars.ID, "Car "+label, "")
		blob, _ := json.Marshal(map[string]any{"status": "open", "color": value})
		if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: strPtr(string(blob))}); err != nil {
			t.Fatalf("seed fields: %v", err)
		}
		got, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		return *got
	}

	resolvable := mk("resolvable", red.ID)
	dangling := mk("dangling", "00000000-0000-4000-8000-00000000dead")
	// The title of a LIVE item in the declared collection. It is still text:
	// resolving it on read would make the answer depend on the reader.
	title := mk("title", red.Title)
	legacyRef := mk("ref", "COLO-999")

	schemas := map[string]models.CollectionSchema{cars.ID: u1RelationSchema("colors")}
	out, err := s.HydrateRelationTargetsQ(s.DB(), ws.ID, []models.Item{resolvable, dangling, title, legacyRef}, schemas)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	// A resolved UUID marshals exactly as v0.49 did: no new key. Checked
	// against a literal, not against another call into the same code.
	gotJSON, err := json.Marshal(out[resolvable.ID]["color"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantJSON := `{"id":"` + red.ID + `","ref":"` + red.Ref + `","title":"` + red.Title + `"}`
	if string(gotJSON) != wantJSON {
		t.Errorf("resolved entry = %s, want the v0.49 bytes %s", gotJSON, wantJSON)
	}

	// A UUID that resolves to nothing stays plain id-only: the gone-or-hidden
	// case, never marked (the marker would otherwise be an existence oracle).
	gotJSON, _ = json.Marshal(out[dangling.ID]["color"])
	if want := `{"id":"00000000-0000-4000-8000-00000000dead"}`; string(gotJSON) != want {
		t.Errorf("dangling UUID entry = %s, want %s", gotJSON, want)
	}

	for _, tc := range []struct {
		name  string
		item  models.Item
		value string
	}{
		{"title of a live item", title, red.Title},
		{"legacy ref", legacyRef, "COLO-999"},
	} {
		got := scalarHydrated(t, out[tc.item.ID]["color"])
		if !got.StoredAsText {
			t.Errorf("%s: %+v not marked stored_as_text", tc.name, got)
		}
		if got.ID != tc.value || got.Ref != "" || got.Title != "" {
			t.Errorf("%s: hydrated as %+v, want only the stored text %q", tc.name, got, tc.value)
		}
	}
}

// TestImportWorkspace_TitleValuedRelationHydratesAsStoredText is the case the
// bug was filed on: a bundle whose relation holds a TITLE imports verbatim (the
// door's carry contract, pinned by TestImportWorkspace_CarriesUnresolvable-
// RelationValues) and must then read back as text, not as a dangling id. The
// resolvable leg is the control: it proves the import ran its remap and that
// hydration resolves what it should.
func TestImportWorkspace_TitleValuedRelationHydratesAsStoredText(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Name: "Owner", Email: "stored-as-text-import@example.com", Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const (
		stamp       = "2026-09-24T00:00:00Z"
		carsSchema  = `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"color","type":"relation","collection":"colors"}]}`
		colorSchema = `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true}]}`
	)
	export := &models.WorkspaceExport{
		Version: 1, ExportedAt: stamp,
		Workspace: models.WorkspaceExportMeta{Name: "Title Archive", Slug: "title-archive"},
		Collections: []models.CollectionExport{
			{ID: "old-coll-colors", Name: "Colors", Slug: "colors", Prefix: "COLO", Schema: colorSchema, CreatedAt: stamp, UpdatedAt: stamp},
			{ID: "old-coll-cars", Name: "Cars", Slug: "cars", Prefix: "CAR", Schema: carsSchema, CreatedAt: stamp, UpdatedAt: stamp},
		},
		Items: []models.ItemExport{
			{ID: "old-color-alpha", CollectionID: "old-coll-colors", Title: "Red", Slug: "red", Fields: `{}`, Tags: `[]`, CreatedAt: stamp, UpdatedAt: stamp},
			{ID: "old-car-resolvable", CollectionID: "old-coll-cars", Title: "Resolvable", Slug: "resolvable", Fields: `{"color":"old-color-alpha"}`, Tags: `[]`, CreatedAt: stamp, UpdatedAt: stamp},
			{ID: "old-car-titled", CollectionID: "old-coll-cars", Title: "Titled", Slug: "titled", Fields: `{"color":"Red"}`, Tags: `[]`, CreatedAt: stamp, UpdatedAt: stamp},
		},
	}
	ws, err := s.ImportWorkspace(export, "Title Archive Target", owner.ID, "")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	byTitle := map[string]models.Item{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	cars := byTitle["Titled"].CollectionID
	coll, err := s.GetCollection(cars)
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	out, err := s.HydrateRelationTargetsQ(s.DB(), ws.ID,
		[]models.Item{byTitle["Resolvable"], byTitle["Titled"]}, map[string]models.CollectionSchema{cars: schema})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	control := scalarHydrated(t, out[byTitle["Resolvable"].ID]["color"])
	if control.ID != byTitle["Red"].ID || control.Title != "Red" || control.StoredAsText {
		t.Fatalf("CONTROL: the remapped relation hydrated as %+v, want the imported Red resolved and unmarked", control)
	}
	got := scalarHydrated(t, out[byTitle["Titled"].ID]["color"])
	if !got.StoredAsText || got.ID != "Red" || got.Ref != "" {
		t.Errorf("an imported title-valued relation hydrated as %+v, want {id:\"Red\", stored_as_text:true}", got)
	}
}

// TestRelationValueStoredAsText_SharedVectors pins the Go predicate to the
// table the web's copy is pinned to (lead review, day 79). KEEP IN SYNC with
// web/src/lib/items/relationFieldTypes.test.ts, which reads the same file:
// the two implementations of one contract rule can only drift by turning one
// of the two suites red.
func TestRelationValueStoredAsText_SharedVectors(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "relation_stored_as_text_cases.json"))
	if err != nil {
		t.Fatalf("read shared vectors: %v", err)
	}
	var fixture struct {
		Cases []struct {
			Name         string `json:"name"`
			Value        string `json:"value"`
			StoredAsText bool   `json:"stored_as_text"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode shared vectors: %v", err)
	}
	// An empty or truncated fixture would make the loop vacuous and green.
	if len(fixture.Cases) < 20 {
		t.Fatalf("shared vectors hold %d cases, want at least 20", len(fixture.Cases))
	}
	for _, tc := range fixture.Cases {
		if got := relationValueStoredAsText(tc.Value); got != tc.StoredAsText {
			t.Errorf("%s: relationValueStoredAsText(%q) = %v, want %v", tc.Name, tc.Value, got, tc.StoredAsText)
		}
	}
}
