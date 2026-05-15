package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-1484: the BUG-1482 NULL-settings regression tests
// (TestListCollectionsMinimalHandlesNullSettings,
// TestGetCollectionHandlesNullSettings,
// TestListCollectionsHandlesNullSettings,
// TestExportWorkspaceHandlesNullSettings) were removed when migration
// 055 / pg 034 made collections.settings NOT NULL DEFAULT '{}'. With the
// constraint in place, the `UPDATE collections SET settings = NULL` setup
// those tests relied on is now a hard write error, and the scenario they
// guarded (production data legally holding NULL) is no longer reachable.
// The defensive `sql.NullString` scans in collections.go / export.go and
// the paired import-side `""→"{}"` coercion (along with
// TestExportImportRoundTripWithEmptyStringSettings) were reverted in the
// IDEA-1484 follow-up now that the schema constraint is the load-bearing
// invariant.

// TestCollectionsSettingsNotNullEnforced is the IDEA-1484 outcome guard:
// after migration 055 / pg 034, attempting to write a literal SQL NULL
// into collections.settings must fail at the driver level. The error
// shape differs across SQLite ("NOT NULL constraint failed:
// collections.settings") and Postgres ("null value in column ... violates
// not-null constraint", SQLSTATE 23502), so we only assert that an error
// surfaces — not its content.
func TestCollectionsSettingsNotNullEnforced(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NOT NULL Enforcement")

	_, err := s.db.Exec(s.q(`
		INSERT INTO collections (id, workspace_id, name, slug, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, ?, ?)
	`), "test-col-not-null", ws.ID, "Things", "things-not-null", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z")
	if err == nil {
		t.Fatalf("expected NOT NULL constraint violation when inserting NULL settings, got nil error")
	}
}

// TestCollectionsSettingsDefaultsToEmptyObject is the companion guard:
// when an INSERT omits the settings column entirely, the column DEFAULT
// must materialize as the empty JSON object `{}`. SQLite stores it as
// TEXT and Postgres stores it as JSONB (which normalizes to `{}` on
// readback); both surface through GetCollection's defensive scan as the
// Go string `{}`.
func TestCollectionsSettingsDefaultsToEmptyObject(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Settings Default")

	const id = "test-col-default-settings"
	if _, err := s.db.Exec(s.q(`
		INSERT INTO collections (id, workspace_id, name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`), id, ws.ID, "Things", "things-default", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("INSERT omitting settings failed: %v", err)
	}

	got, err := s.GetCollection(id)
	if err != nil {
		t.Fatalf("GetCollection error: %v", err)
	}
	if got == nil {
		t.Fatalf("GetCollection returned nil for %q", id)
	}
	if got.Settings != "{}" {
		t.Errorf("expected default settings to materialize as %q, got %q", "{}", got.Settings)
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
	// Compare settings semantically. Postgres JSONB normalizes formatting and
	// key order, so a byte-for-byte string compare against the input literal
	// would be brittle across drivers. Unmarshal both sides and assert the
	// decoded values are equal — this verifies the JSON actually round-trips
	// rather than just that *some* non-empty string came back.
	want := map[string]any{
		"done_field":  "status",
		"done_values": []any{"closed"},
	}
	var found bool
	for _, c := range colls {
		if c.ID != created.ID {
			continue
		}
		found = true
		if c.Settings == "" {
			t.Fatalf("expected non-empty settings JSON, got empty string")
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(c.Settings), &got); err != nil {
			t.Fatalf("settings is not valid JSON: %v (raw=%q)", err, c.Settings)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("settings round-trip mismatch:\n  got:  %#v\n  want: %#v", got, want)
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
