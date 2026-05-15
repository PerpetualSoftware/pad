package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListCollectionsMinimalHandlesNullSettings is the regression guard for
// BUG-1482: `ListCollectionsMinimal` previously used `COALESCE(settings, '')`
// which fails at planner time on Postgres because `collections.settings` is
// JSONB and `''` is not valid JSON (SQLSTATE 22P02). The query failed
// regardless of row contents. SQLite's loose typing hid the issue.
//
// This test exercises both drivers and explicitly stores a NULL `settings`
// to cover the column-nullability branch — neither the SQLite migration
// (`settings TEXT DEFAULT '{}'`) nor the Postgres one (`settings JSONB
// DEFAULT '{}'`) marks the column NOT NULL, so production data can legally
// hold NULL. The contract is that NULL surfaces as `""` so existing
// `if c.Settings != ""` guards in downstream consumers continue to work.
func TestListCollectionsMinimalHandlesNullSettings(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "ListCollectionsMinimal NULL Settings")

	if err := s.SeedDefaultCollections(ws.ID); err != nil {
		t.Fatalf("SeedDefaultCollections error: %v", err)
	}

	// Force one collection's settings to NULL via direct SQL to simulate
	// legacy / partially-initialized rows. CreateCollection's normal path
	// coerces empty settings to "{}", so we have to bypass it.
	if _, err := s.db.Exec(s.q(`UPDATE collections SET settings = NULL WHERE workspace_id = ?`), ws.ID); err != nil {
		t.Fatalf("force NULL settings: %v", err)
	}

	colls, err := s.ListCollectionsMinimal(ws.ID)
	if err != nil {
		t.Fatalf("ListCollectionsMinimal error (BUG-1482 regression): %v", err)
	}
	if len(colls) == 0 {
		t.Fatalf("ListCollectionsMinimal returned 0 collections; expected the seeded defaults")
	}
	for _, c := range colls {
		if c.Settings != "" {
			t.Errorf("collection %q: expected NULL settings to surface as empty string sentinel, got %q", c.ID, c.Settings)
		}
	}
}

// TestListCollectionsMinimalReturnsSettingsJSON verifies the happy path:
// a collection with non-NULL JSON settings round-trips through the minimal
// query intact on both drivers.
func TestListCollectionsMinimalReturnsSettingsJSON(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "ListCollectionsMinimal JSON Settings")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Things",
		Slug:     "things",
		Settings: `{"done_field":"status","done_values":["closed"]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	colls, err := s.ListCollectionsMinimal(ws.ID)
	if err != nil {
		t.Fatalf("ListCollectionsMinimal error: %v", err)
	}
	var found bool
	for _, c := range colls {
		if c.ID != created.ID {
			continue
		}
		found = true
		if c.Settings == "" {
			t.Errorf("expected non-empty settings JSON, got empty string")
		}
	}
	if !found {
		t.Fatalf("created collection %q not returned by ListCollectionsMinimal", created.ID)
	}
}

// TestSeedFromBlankTemplate verifies that bootstrapping a workspace from the
// blank template (IDEA-1479) produces exactly two collections (Conventions,
// Playbooks) and zero items. Drift here means the template silently grew
// (or shrunk) its starter pack and the motivating "agent-self workspace
// with no ghost collections" use case is no longer protected.
func TestSeedFromBlankTemplate(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Blank Test")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "blank"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate(blank) error: %v", err)
	}

	colls, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	if len(colls) != 2 {
		t.Fatalf("blank workspace has %d collections, want 2; got %+v", len(colls), collectionSlugs(colls))
	}

	wantSlugs := map[string]bool{"conventions": true, "playbooks": true}
	for _, c := range colls {
		if !wantSlugs[c.Slug] {
			t.Errorf("blank workspace has unexpected collection slug %q", c.Slug)
		}
		delete(wantSlugs, c.Slug)
	}
	for slug := range wantSlugs {
		t.Errorf("blank workspace missing required system collection %q", slug)
	}

	// No items should be seeded — neither sample items, conventions, nor
	// playbooks.
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("blank workspace has %d items, want 0", len(items))
	}
}

// TestBlankWorkspaceSurvivesSeedDefaultCollections is the regression guard
// for codex round-2: the server runs SeedDefaultCollections against every
// workspace at startup as an auto-upgrade rescue. Before the fix, that hook
// unconditionally re-materialized the standard Software-template collections
// (tasks/ideas/plans/docs) into any workspace missing them — including
// blank-template workspaces, which would silently grow ghost collections on
// every server restart. The fix gates the rescue on "workspace has zero
// collections" so blank (which ships 2 system collections) is a no-op.
func TestBlankWorkspaceSurvivesSeedDefaultCollections(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Blank Survival Test")

	if err := s.SeedCollectionsFromTemplate(ws.ID, "blank"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate(blank) error: %v", err)
	}

	// Simulate a server restart firing the auto-upgrade hook.
	if err := s.SeedDefaultCollections(ws.ID); err != nil {
		t.Fatalf("SeedDefaultCollections error: %v", err)
	}

	colls, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	if len(colls) != 2 {
		t.Fatalf("blank workspace has %d collections after auto-upgrade, want 2 (Conventions + Playbooks only); got %+v", len(colls), collectionSlugs(colls))
	}
	for _, c := range colls {
		if c.Slug == "tasks" || c.Slug == "ideas" || c.Slug == "plans" || c.Slug == "docs" {
			t.Errorf("blank workspace acquired user-facing software collection %q after SeedDefaultCollections — auto-upgrade rescue gate is broken", c.Slug)
		}
	}

	// And repeated invocations remain no-ops.
	if err := s.SeedDefaultCollections(ws.ID); err != nil {
		t.Fatalf("SeedDefaultCollections (second run) error: %v", err)
	}
	colls, _ = s.ListCollections(ws.ID)
	if len(colls) != 2 {
		t.Errorf("blank workspace has %d collections after second auto-upgrade pass, want 2", len(colls))
	}
}

// TestEmptyWorkspaceStillGetsDefaults verifies the rescue path still works
// for a workspace that genuinely has zero collections — the original intent
// of the SeedDefaultCollections hook (predates templates; see git blame on
// cmd/pad/main.go's auto-upgrade block). If a workspace was created before
// the seed-on-init flow existed, or a partial init failed before any
// collection landed, the auto-upgrade must still materialize the Software
// defaults.
func TestEmptyWorkspaceStillGetsDefaults(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Empty Rescue Test")

	// No SeedCollectionsFromTemplate — workspace starts with zero
	// collections (simulating a pre-templates-era workspace, or a
	// partial init).
	if err := s.SeedDefaultCollections(ws.ID); err != nil {
		t.Fatalf("SeedDefaultCollections error: %v", err)
	}

	colls, err := s.ListCollections(ws.ID)
	if err != nil {
		t.Fatalf("ListCollections error: %v", err)
	}
	slugs := make(map[string]bool, len(colls))
	for _, c := range colls {
		slugs[c.Slug] = true
	}
	for _, want := range []string{"tasks", "ideas", "plans", "docs", "conventions", "playbooks"} {
		if !slugs[want] {
			t.Errorf("empty workspace rescue did not materialize default collection %q (got %v)", want, collectionSlugs(colls))
		}
	}
}

func collectionSlugs(colls []models.Collection) []string {
	out := make([]string, 0, len(colls))
	for _, c := range colls {
		out = append(out, c.Slug)
	}
	return out
}
