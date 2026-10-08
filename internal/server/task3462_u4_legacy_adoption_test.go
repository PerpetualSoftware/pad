package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462 U4: items made before built-in origins were recorded get one by
// exact title (or a playbook's invocation_slug), with no seed version, so they
// read current (text unchanged) or unknown_origin (text differs) and the 2-way
// preview applies. Dave's ruling: no historical hashes. A one-time pass, so a
// later copy (which deliberately records no origin) is never adopted, and an
// item created after origins began being recorded is left alone.

// makeLegacy turns a workspace's seeded built-ins back into what an install
// from before TASK-3462 holds: no origin rows, items older than the origin
// migration.
func makeLegacy(t *testing.T, srv *Server, wsID string) {
	t.Helper()
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`DELETE FROM item_builtin_origin WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`), wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = '2020-01-01T00:00:00Z' WHERE workspace_id = ?`), wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE workspaces SET created_at = '2020-01-01T00:00:00Z' WHERE id = ?`), wsID); err != nil {
		t.Fatal(err)
	}
}

func TestTASK3462U4_LegacyAdoption(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Legacy 3462", "startup")
		conv := task3462ItemByTitle(t, srv, slug, "conventions", "Conventional commit format")
		plan := task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative")
		ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
		dec := task3462ItemByTitle(t, srv, slug, "playbooks", "Decompose a plan into tasks")
		makeLegacy(t, srv, wsID)

		// Legacy edits: plan's body; ship's title (its invocation_slug still
		// names it); decompose is soft-deleted.
		edited := "my own plan steps"
		if _, err := srv.store.UpdateItem(plan.ID, models.ItemUpdate{Content: &edited}); err != nil {
			t.Fatal(err)
		}
		renamed := "Our release ritual"
		if _, err := srv.store.UpdateItem(ship.ID, models.ItemUpdate{Title: &renamed}); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.DeleteItem(dec.ID); err != nil {
			t.Fatal(err)
		}
		// Back-date again: the edits above stamped updated_at only, but be
		// explicit that these are pre-origin items.
		makeLegacyDatesOnly(t, srv, wsID)

		// A NEW item with a library title, made after origins began being
		// recorded and with none (what a copy looks like): never adopted.
		fresh, err := srv.store.CreateItem(wsID, conv.CollectionID, models.ItemCreate{Title: "Never push directly to main", Fields: `{}`})
		if err != nil {
			t.Fatal(err)
		}
		// Pinned AFTER the migration: in a test the migration and this create
		// can share a second, and the cutoff includes that second.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = '2999-01-01T00:00:00Z' WHERE id = ?`), fresh.ID); err != nil {
			t.Fatal(err)
		}

		res, err := srv.store.AdoptLegacyBuiltins()
		if err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if res.Skipped {
			t.Fatal("first run skipped")
		}

		origin := func(id string) *models.BuiltinOrigin {
			o, err := srv.store.GetItemBuiltinOrigin(id)
			if err != nil {
				t.Fatal(err)
			}
			return o
		}
		if o := origin(conv.ID); o == nil || o.Key != "convention/conventional-commit-format" || o.SeedHash != "" {
			t.Fatalf("unedited convention not adopted by title: %+v", o)
		}
		if st := task3462GetState(t, srv, slug, conv.Slug); st.State != collections.BuiltinCurrent {
			t.Fatalf("adopted unedited convention reads %s, want current", st.State)
		}
		if st := task3462GetState(t, srv, slug, plan.Slug); st.State != collections.BuiltinUnknownOrigin {
			t.Fatalf("adopted edited playbook reads %s, want unknown_origin", st.State)
		}
		if o := origin(ship.ID); o == nil || o.Key != "playbook/ship" {
			t.Fatalf("renamed playbook not adopted by invocation_slug: %+v", o)
		}
		if o := origin(dec.ID); o != nil {
			t.Fatalf("soft-deleted item adopted: %+v", o)
		}
		if o := origin(fresh.ID); o != nil {
			t.Fatalf("an item created after the cutoff was adopted: %+v", o)
		}

		// Once only: a second run does nothing, even for an eligible item.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`DELETE FROM item_builtin_origin WHERE item_id = ?`), conv.ID); err != nil {
			t.Fatal(err)
		}
		res2, err := srv.store.AdoptLegacyBuiltins()
		if err != nil {
			t.Fatal(err)
		}
		if !res2.Skipped || origin(conv.ID) != nil {
			t.Fatalf("second run adopted again: %+v", res2)
		}
	})
}

func makeLegacyDatesOnly(t *testing.T, srv *Server, wsID string) {
	t.Helper()
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = '2020-01-01T00:00:00Z' WHERE workspace_id = ?`), wsID); err != nil {
		t.Fatal(err)
	}
}

// The matcher: exact title, or a playbook's invocation_slug; an ambiguous
// title resolves only by exact body.
func TestTASK3462U4_MatchLegacyBuiltin(t *testing.T) {
	entries := []collections.BuiltinEntry{
		{Key: "a/one", Kind: "convention", Title: "Same title", Content: "body one"},
		{Key: "b/one", Kind: "convention", Title: "Same title", Content: "body two"},
		{Key: "p/x", Kind: "playbook", Title: "Exes", Content: "x", Fields: `{"invocation_slug":"exes"}`},
		{Key: "p/y", Kind: "playbook", Title: "Wyes", Content: "y", Fields: `{"invocation_slug":"wyes"}`},
	}
	cases := []struct {
		name, kind, title, slug, content, want string
	}{
		{"ambiguous title, body decides", "convention", "Same title", "", "body two", "b/one"},
		{"ambiguous title, no body match", "convention", "Same title", "", "edited", ""},
		{"wrong kind", "playbook", "Same title", "", "body one", ""},
		{"playbook by slug", "playbook", "Renamed", "exes", "x", "p/x"},
		{"no match", "convention", "Other", "", "", ""},
		// codex r1: the title names one playbook and the slug another, so it
		// is ambiguous and nothing is adopted.
		{"title and slug disagree", "playbook", "Exes", "wyes", "x", ""},
	}
	for _, c := range cases {
		if got := collections.MatchLegacyBuiltin(entries, c.kind, c.title, c.slug, c.content); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// codex r1 (High): an item created in the very second the origin migration
// was applied (timestamps are second-precision) is still a legacy item.
func TestTASK3462U4_SameSecondAsTheMigrationIsLegacy(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, wsID := task3462Workspace(t, srv, "Same second 3462", "startup")
		makeLegacy(t, srv, wsID)
		var appliedAt string
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT applied_at FROM schema_migrations WHERE version LIKE ?`), "%item_builtin_origin.sql").Scan(&appliedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = ? WHERE workspace_id = ?`), appliedAt, wsID); err != nil {
			t.Fatal(err)
		}
		res, err := srv.store.AdoptLegacyBuiltins()
		if err != nil {
			t.Fatal(err)
		}
		if res.Adopted == 0 {
			t.Fatalf("items created in the migration's second were not adopted: %+v", res)
		}
	})
}

// codex r2 (High): workspace import keeps each item's created_at from the
// bundle, so an old bundle imported after the origin migration but before the
// pass first ran would look pre-cutoff. Every import mints a NEW workspace
// (none writes into an existing one), so the workspace's own created_at is
// the arrival bound.
func TestTASK3462U4_ImportedAfterTheCutoffIsNotLegacy(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Import src 3462", "startup")
		makeLegacy(t, srv, wsID)
		data, err := srv.store.ExportWorkspace(slug)
		if err != nil {
			t.Fatal(err)
		}
		for i := range data.Items {
			data.Items[i].BuiltinOrigin = nil
		}
		ws, err := srv.store.ImportWorkspace(data, "import-dst-3462", "", "")
		if err != nil {
			t.Fatal(err)
		}
		// Pinned after the migration: in a test the two can share a second.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE workspaces SET created_at = '2999-01-01T00:00:00Z' WHERE id = ?`), ws.ID); err != nil {
			t.Fatal(err)
		}
		var oldItems int
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND created_at = '2020-01-01T00:00:00Z'`), ws.ID).Scan(&oldItems); err != nil {
			t.Fatal(err)
		}
		if oldItems == 0 {
			t.Fatal("precondition: the import did not keep the bundle's item timestamps")
		}
		res, err := srv.store.AdoptLegacyBuiltins()
		if err != nil {
			t.Fatal(err)
		}
		// The legacy source is adopted (it shows the pass ran and matched
		// these very titles); the import is not.
		if res.Adopted == 0 {
			t.Fatalf("precondition: the legacy source was not adopted: %+v", res)
		}
		var adopted int
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(`SELECT COUNT(*) FROM item_builtin_origin o JOIN items i ON i.id = o.item_id WHERE i.workspace_id = ?`), ws.ID).Scan(&adopted); err != nil {
			t.Fatal(err)
		}
		if adopted != 0 {
			t.Fatalf("%d items imported after the cutoff were adopted", adopted)
		}
	})
}
