package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3535: a bundle written before tracks_work existed lands the way an
// upgraded database does. Its system collections become reference (what the
// migration did to them); every other collection keeps the absent key, which
// means work; a value the bundle carries is kept.
func TestPLAN3535_ImportOfAnOldBundleMarksSystemCollectionsReference(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "imp3535@example.com", "Owner", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Src", Slug: "src3535", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	// Strip the key everywhere, as an older Pad wrote it, except playbooks,
	// whose bundle says work explicitly.
	for i := range data.Collections {
		var m map[string]any
		if err := json.Unmarshal([]byte(data.Collections[i].Settings), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "tracks_work")
		if data.Collections[i].Slug == "playbooks" {
			m["tracks_work"] = true
		}
		b, _ := json.Marshal(m)
		data.Collections[i].Settings = string(b)
	}
	imported, err := s.ImportWorkspace(data, "Imported", owner.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	colls, err := s.ListCollections(imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"conventions": false, "playbooks": true, "tasks": true, "docs": true}
	for _, c := range colls {
		w, ok := want[c.Slug]
		if !ok {
			continue
		}
		if got := models.CollectionTracksWorkJSON(c.Settings); got != w {
			t.Errorf("%s: tracks_work %v, want %v (settings %s)", c.Slug, got, w, c.Settings)
		}
		delete(want, c.Slug)
	}
	if len(want) != 0 {
		t.Fatalf("collections missing from the import: %v", want)
	}
}
