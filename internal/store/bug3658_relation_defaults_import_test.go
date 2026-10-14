package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3658: an imported collection's relation defaults named items by their
// SOURCE ids, which name nothing in the new workspace, so the default was
// silently dropped on the next create (or refused it, when required). After
// import a default names the imported item; an id the import has no item for
// is dropped and reported; a value that is not an id is kept.
func TestBUG3658_ImportRemapsRelationDefaults(t *testing.T) {
	s := testStore(t)
	const (
		oldRed    = "11111111-1111-4111-8111-111111111111"
		oldBlue   = "22222222-2222-4222-8222-222222222222"
		notInDump = "33333333-3333-4333-8333-333333333333"
	)
	carsSchema := `{"fields":[` +
		`{"key":"status","type":"select","options":["open","done"],"default":"open"},` +
		`{"key":"color","type":"relation","collection":"colors","default":"` + oldRed + `"},` +
		`{"key":"palette","type":"multi_relation","collection":"colors","default":["` + oldRed + `","` + oldBlue + `","` + notInDump + `"]},` +
		`{"key":"ghost","type":"relation","collection":"colors","default":"` + notInDump + `"},` +
		`{"key":"byref","type":"relation","collection":"colors","default":"COLO-1"},` +
		`{"key":"note","type":"text","default":"` + oldRed + `","x-future":{"kept":true}}]}`
	export := &models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Defaults Source", Slug: "defaults-source"},
		Collections: []models.CollectionExport{
			{ID: "old-coll-colors", Name: "Colors", Slug: "colors", Prefix: "COLO",
				Schema:    `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z"},
			{ID: "old-coll-cars", Name: "Cars", Slug: "cars", Prefix: "CAR", Schema: carsSchema,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z"},
		},
		Items: []models.ItemExport{
			{ID: oldRed, CollectionID: "old-coll-colors", Title: "Red", Slug: "red", Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z"},
			{ID: oldBlue, CollectionID: "old-coll-colors", Title: "Blue", Slug: "blue", Fields: `{}`, Tags: `[]`,
				CreatedAt: "2026-09-11T00:00:00Z", UpdatedAt: "2026-09-11T00:00:00Z"},
		},
	}
	ws, report, err := s.ImportWorkspaceWithReport(export, "Defaults Target", "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	newRed, newBlue := report.ItemIDs[oldRed], report.ItemIDs[oldBlue]
	if newRed == "" || newBlue == "" || newRed == oldRed {
		t.Fatalf("instrument: item ids were not remapped: %v", report.ItemIDs)
	}
	cars, err := s.GetCollectionBySlug(ws.ID, "cars")
	if err != nil || cars == nil {
		t.Fatalf("cars: %v", err)
	}
	var schema struct {
		Fields []map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(cars.Schema), &schema); err != nil {
		t.Fatalf("schema: %v (%s)", err, cars.Schema)
	}
	got := map[string]map[string]any{}
	for _, f := range schema.Fields {
		got[f["key"].(string)] = f
	}
	if d := got["color"]["default"]; d != newRed {
		t.Errorf("color default = %v, want the imported Red %s", d, newRed)
	}
	if d, _ := json.Marshal(got["palette"]["default"]); string(d) != `["`+newRed+`","`+newBlue+`"]` {
		t.Errorf("palette default = %s, want the imported Red and Blue, the id not in the import dropped", d)
	}
	if d, has := got["ghost"]["default"]; has {
		t.Errorf("ghost default = %v, want it dropped: it names no item in the import", d)
	}
	if d := got["byref"]["default"]; d != "COLO-1" {
		t.Errorf("byref default = %v, want the ref kept", d)
	}
	// Not a relation: left alone, and so is a key this build does not model.
	if d := got["note"]["default"]; d != oldRed {
		t.Errorf("a text default was rewritten: %v", d)
	}
	if _, ok := got["note"]["x-future"]; !ok {
		t.Errorf("an unmodelled schema key was lost: %s", cars.Schema)
	}
	want := map[string]bool{"cars.palette": true, "cars.ghost": true}
	if len(report.DroppedRelationDefaults) != len(want) {
		t.Errorf("DroppedRelationDefaults = %v, want %v", report.DroppedRelationDefaults, want)
	}
	for _, d := range report.DroppedRelationDefaults {
		if !want[d] {
			t.Errorf("unexpected dropped default %q", d)
		}
	}
}
