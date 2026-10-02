package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3357: the import remaps every collection and item by its SOURCE id, so
// two rows sharing an id silently collapse into one mapping and every
// reference to that id lands on whichever row was written last (a child under
// the wrong parent, a link to the wrong item, a version on the wrong body). A
// bundle with a duplicate source id is refused before anything is written.

func dupFixture(t *testing.T) *models.WorkspaceExport {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Dup Source")
	coll := createTestCollection(t, s, ws.ID, "Things")
	other := createTestCollection(t, s, ws.ID, "Others")
	createTestItem(t, s, ws.ID, coll.ID, "One", "body one")
	createTestItem(t, s, ws.ID, coll.ID, "Two", "body two")
	createTestItem(t, s, ws.ID, other.ID, "Three", "body three")
	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(data.Items) < 3 || len(data.Collections) < 2 {
		t.Fatalf("fixture: %d items, %d collections", len(data.Items), len(data.Collections))
	}
	return data
}

func TestBUG3357_DuplicateSourceIDsRefused(t *testing.T) {
	cases := map[string]func(d *models.WorkspaceExport){
		"item": func(d *models.WorkspaceExport) {
			d.Items[1].ID = d.Items[0].ID
		},
		"collection": func(d *models.WorkspaceExport) {
			d.Collections[1].ID = d.Collections[0].ID
		},
		// Two rows without an id collide on "" (codex r1); one is allowed.
		"item-empty": func(d *models.WorkspaceExport) {
			d.Items[0].ID, d.Items[1].ID = "", ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			data := dupFixture(t)
			mutate(data)
			dst := testStore(t)
			dstName := "bug3357-" + name
			ws, err := dst.ImportWorkspace(data, dstName, "", "api")
			v, ok := AsValidationError(err)
			if !ok {
				t.Fatalf("import with a duplicate %s id: err = %v (ws = %v), want a ValidationError", name, err, ws)
			}
			kind := strings.TrimSuffix(name, "-empty")
			if !strings.Contains(v.Reason, kind) || !(strings.Contains(v.Reason, "duplicate") || strings.Contains(v.Reason, "no id")) {
				t.Errorf("reason %q should name the %s collision", v.Reason, kind)
			}
			if ws != nil || workspaceExists(t, dst, dstName) {
				t.Error("the refused import left a workspace behind")
			}
		})
	}

	// Control: the same bundle, unmodified, imports.
	data := dupFixture(t)
	dst := testStore(t)
	if _, err := dst.ImportWorkspace(data, "bug3357-control", "", "api"); err != nil {
		t.Fatalf("control import: %v", err)
	}
}

// Codex r2: ImportReport.ItemIDs names only items that were actually
// inserted. An item the import skips (its collection is missing) has an id
// that exists nowhere, and the bundle door would attach to it.
func TestBUG3357_ItemIDsOmitSkippedItems(t *testing.T) {
	data := dupFixture(t)
	skipped := data.Items[0].ID
	data.Items[0].CollectionID = "no-such-collection"
	dst := testStore(t)
	ws, report, err := dst.ImportWorkspaceWithReport(data, "bug3357-skip", "", "api")
	if err != nil || ws == nil {
		t.Fatalf("import: %v", err)
	}
	if _, ok := report.ItemIDs[skipped]; ok {
		t.Fatalf("ItemIDs names the skipped item %s", skipped)
	}
	for _, it := range data.Items[1:] {
		newID, ok := report.ItemIDs[it.ID]
		if !ok {
			t.Fatalf("ItemIDs is missing inserted item %s", it.ID)
		}
		if got, err := dst.GetItem(newID); err != nil || got == nil {
			t.Fatalf("ItemIDs maps %s to %s, which does not exist (%v)", it.ID, newID, err)
		}
	}
}
