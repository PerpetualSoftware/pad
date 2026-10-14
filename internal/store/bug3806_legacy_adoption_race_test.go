package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3806: AdoptLegacyBuiltins matched on the title, fields and content it
// buffered in its scan, then inserted origins in a later transaction without
// re-reading the items. An edit landing between the two was invisible to the
// match: a convention renamed away from its library title, or deleted, was
// still adopted, and then offered a built-in's text it no longer names. The
// conflicting writes run from the after-scan hook, which is package-level, so
// this test must not be parallel.

func legacyAdoptionWorkspace(t *testing.T, s *Store) string {
	t.Helper()
	u := createTestUser(t, s, "legacy-3806@test.com", "Legacy", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Legacy 3806", Slug: "legacy-3806", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	// Make every seeded item a pre-origin legacy item: no origin row, and
	// item and workspace both created before the origin migration.
	for _, q := range []string{
		`DELETE FROM item_builtin_origin WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`,
		`UPDATE items SET created_at = '2020-01-01T00:00:00Z' WHERE workspace_id = ?`,
		`UPDATE workspaces SET created_at = '2020-01-01T00:00:00Z' WHERE id = ?`,
	} {
		if _, err := s.db.Exec(s.q(q), ws.ID); err != nil {
			t.Fatalf("make legacy: %v", err)
		}
	}
	return ws.ID
}

func legacyItemByTitle(t *testing.T, s *Store, wsID, title string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(s.q(`SELECT id FROM items WHERE workspace_id = ? AND title = ? AND deleted_at IS NULL`), wsID, title).Scan(&id); err != nil {
		t.Fatalf("find %q: %v", title, err)
	}
	return id
}

func TestAdoptLegacyBuiltins_EditAfterScanIsSeen(t *testing.T) {
	s := testStore(t)
	wsID := legacyAdoptionWorkspace(t, s)

	renamed := legacyItemByTitle(t, s, wsID, "Conventional commit format")
	deleted := legacyItemByTitle(t, s, wsID, "Never push directly to main")
	untouched := legacyItemByTitle(t, s, wsID, "Decompose a plan into tasks")

	fired := false
	adoptLegacyAfterScanHook = func() {
		adoptLegacyAfterScanHook = nil
		fired = true
		title := "Our commit style"
		if _, err := s.UpdateItem(renamed, models.ItemUpdate{Title: &title}); err != nil {
			t.Errorf("rename: %v", err)
		}
		if err := s.DeleteItem(deleted); err != nil {
			t.Errorf("delete: %v", err)
		}
	}
	t.Cleanup(func() { adoptLegacyAfterScanHook = nil })

	if _, err := s.AdoptLegacyBuiltins(); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !fired {
		t.Fatal("after-scan hook never fired; the test did not exercise the window")
	}

	for _, c := range []struct {
		name, id string
		want     bool
	}{
		{"renamed away from its library title", renamed, false},
		{"deleted", deleted, false},
		{"untouched", untouched, true},
	} {
		o, err := s.GetItemBuiltinOrigin(c.id)
		if err != nil {
			t.Fatalf("%s: read origin: %v", c.name, err)
		}
		if got := o != nil; got != c.want {
			t.Errorf("%s: adopted = %v, want %v", c.name, got, c.want)
		}
	}
}
