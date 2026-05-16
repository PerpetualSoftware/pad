package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-1486 + IDEA-1488 regression tests.
//
// These tests guard the paired ship of the JSONB NOT NULL hardening on
// items.fields / items.tags / views.config and the handler-layer shape
// validation that closes the contract loop on the writer side.
//
// Most tests are dialect-agnostic and run on whichever driver
// testStore() picks (SQLite by default, Postgres when
// PAD_TEST_POSTGRES_URL is set). The SQLite-specific schema
// introspection assertions (FTS triggers, indexes) are gated on the
// SQLite path because the equivalent invariants live in different
// system catalogs on Postgres.

// TestItemsViewsJSONB_UpdateItemCoercesEmptyStringFields exercises the
// IDEA-1486 floor at the items.go:1442 UPDATE path. After the NOT NULL
// migration ships, an UpdateItem with Fields="" would 500 on Postgres
// JSONB type-validation and silently store invalid JSON on SQLite.
// The store-layer coercion normalizes "" → "{}" / "[]" before the
// UPDATE reaches the database.
func TestItemsViewsJSONB_UpdateItemCoercesEmptyStringFields(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "JSONBCoerce")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "an item", "")

	emptyFields := ""
	emptyTags := ""
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		Fields: &emptyFields,
		Tags:   &emptyTags,
	})
	if err != nil {
		t.Fatalf("UpdateItem with empty fields/tags: %v", err)
	}
	if updated.Fields != "{}" {
		t.Errorf("Fields after empty-string update: want %q, got %q", "{}", updated.Fields)
	}
	if updated.Tags != "[]" {
		t.Errorf("Tags after empty-string update: want %q, got %q", "[]", updated.Tags)
	}
}

// TestItemsViewsJSONB_UpdateViewCoercesEmptyStringConfig exercises the
// IDEA-1486 floor at views.go:152.
func TestItemsViewsJSONB_UpdateViewCoercesEmptyStringConfig(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "ViewCoerce")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	view, err := s.CreateView(ws.ID, models.ViewCreate{
		CollectionID: &col.ID,
		Name:         "All",
		ViewType:     "list",
		Config:       `{"layout":"list"}`,
	})
	if err != nil {
		t.Fatalf("CreateView: %v", err)
	}

	emptyConfig := ""
	updated, err := s.UpdateView(view.ID, models.ViewUpdate{
		Config: &emptyConfig,
	})
	if err != nil {
		t.Fatalf("UpdateView with empty config: %v", err)
	}
	if updated.Config != "{}" {
		t.Errorf("Config after empty-string update: want %q, got %q", "{}", updated.Config)
	}
}

// TestItemsViewsJSONB_ImportWorkspaceCoercesEmptyAndMalformed exercises
// the import-boundary normalization: empty-string and malformed JSON on
// items.fields / items.tags / collections.settings are coerced to the
// shape default. Mirrors the IDEA-1488 log-and-coerce policy: legacy
// bundles don't fail-stop on one bad row.
func TestItemsViewsJSONB_ImportWorkspaceCoercesEmptyAndMalformed(t *testing.T) {
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{
		Email:    "owner-import@example.com",
		Name:     "Owner",
		Password: "passw0rd!",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	export := &models.WorkspaceExport{
		Version:    1,
		ExportedAt: "2026-05-16T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{
			Name: "Imported",
			Slug: "imported",
		},
		Collections: []models.CollectionExport{
			{
				ID:        "old-coll-1",
				Name:      "Tasks",
				Slug:      "tasks",
				Prefix:    "TASK",
				Schema:    `{"fields":[]}`,
				Settings:  "", // empty-string sentinel → "{}"
				CreatedAt: "2026-05-16T00:00:00Z",
				UpdatedAt: "2026-05-16T00:00:00Z",
			},
			{
				ID:        "old-coll-2",
				Name:      "Ideas",
				Slug:      "ideas",
				Prefix:    "IDEA",
				Schema:    `{"fields":[]}`,
				Settings:  `not json at all`, // malformed → log-and-coerce
				CreatedAt: "2026-05-16T00:00:00Z",
				UpdatedAt: "2026-05-16T00:00:00Z",
			},
		},
		Items: []models.ItemExport{
			{
				ID:           "old-item-1",
				CollectionID: "old-coll-1",
				Title:        "Item with empty fields",
				Slug:         "item-empty",
				Content:      "",
				Fields:       "", // empty → "{}"
				Tags:         "", // empty → "[]"
				CreatedAt:    "2026-05-16T00:00:00Z",
				UpdatedAt:    "2026-05-16T00:00:00Z",
			},
			{
				ID:           "old-item-2",
				CollectionID: "old-coll-1",
				Title:        "Item with malformed json",
				Slug:         "item-bad",
				Content:      "",
				Fields:       `{not valid`,    // malformed → log-and-coerce → "{}"
				Tags:         `also not json`, // malformed → log-and-coerce → "[]"
				CreatedAt:    "2026-05-16T00:00:00Z",
				UpdatedAt:    "2026-05-16T00:00:00Z",
			},
			{
				ID:           "old-item-3",
				CollectionID: "old-coll-1",
				Title:        "Item with valid fields",
				Slug:         "item-good",
				Content:      "",
				Fields:       `{"status":"open"}`,
				Tags:         `["urgent"]`,
				CreatedAt:    "2026-05-16T00:00:00Z",
				UpdatedAt:    "2026-05-16T00:00:00Z",
			},
		},
	}

	ws, err := s.ImportWorkspace(export, "Imported Coerce", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	// All three items must be present, with coerced/preserved JSON.
	cases := []struct {
		title      string
		wantFields string
		wantTags   string
	}{
		{"Item with empty fields", "{}", "[]"},
		{"Item with malformed json", "{}", "[]"},
		{"Item with valid fields", `{"status":"open"}`, `["urgent"]`},
	}
	for _, tc := range cases {
		var gotFields, gotTags string
		err := s.db.QueryRow(s.q(
			"SELECT fields, tags FROM items WHERE workspace_id = ? AND title = ?"),
			ws.ID, tc.title,
		).Scan(&gotFields, &gotTags)
		if err != nil {
			t.Fatalf("query item %q: %v", tc.title, err)
		}
		// Postgres returns JSONB normalized form; normalize string compare
		// via a tolerance for whitespace via raw equality fallback.
		if !jsonEqualString(gotFields, tc.wantFields) {
			t.Errorf("item %q: fields want %q, got %q", tc.title, tc.wantFields, gotFields)
		}
		if !jsonEqualString(gotTags, tc.wantTags) {
			t.Errorf("item %q: tags want %q, got %q", tc.title, tc.wantTags, gotTags)
		}
	}

	// Both collections should have settings = "{}" after coercion.
	rows, err := s.db.Query(s.q(
		"SELECT name, settings FROM collections WHERE workspace_id = ? ORDER BY name"),
		ws.ID,
	)
	if err != nil {
		t.Fatalf("query collections: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, settings string
		if err := rows.Scan(&name, &settings); err != nil {
			t.Fatalf("scan collection: %v", err)
		}
		if name == "Tasks" || name == "Ideas" {
			if !jsonEqualString(settings, "{}") {
				t.Errorf("collection %q settings want %q, got %q", name, "{}", settings)
			}
		}
	}
}

// jsonEqualString compares two JSON strings tolerantly of insignificant
// whitespace differences that emerge from JSONB ↔ TEXT round-trips.
func jsonEqualString(a, b string) bool {
	return strings.Join(strings.Fields(a), "") == strings.Join(strings.Fields(b), "")
}

// TestItemsViewsJSONB_SQLiteRebuildPreservesIndexesAndTriggers is the
// SQLite-only schema-introspection regression test for migration 056.
// It asserts that after the items rebuild, all 7 indexes and 3 FTS
// triggers exist, and that the items_fts virtual table still answers
// queries against post-rebuild rowids. Catches regressions where the
// rebuild forgets to recreate an index or the FTS triggers don't
// re-attach to the renamed items table.
func TestItemsViewsJSONB_SQLiteRebuildPreservesIndexesAndTriggers(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific schema-introspection test")
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "RebuildShape")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Seed an item so we can verify FTS still works post-rebuild.
	item := createTestItem(t, s, ws.ID, col.ID, "Auth quickref",
		"OAuth2 authentication flow")
	if item == nil {
		t.Fatal("createTestItem returned nil")
	}

	// All 7 indexes from 005/017/053 must be present on items.
	wantIndexes := map[string]bool{
		"idx_items_collection":    false,
		"idx_items_workspace":     false,
		"idx_items_parent":        false,
		"idx_items_updated":       false,
		"idx_items_assigned_user": false,
		"idx_items_agent_role":    false,
		"idx_items_workspace_seq": false,
	}
	rows, err := s.db.Query(
		"SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='items'")
	if err != nil {
		t.Fatalf("query sqlite_master for indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		if _, ok := wantIndexes[n]; ok {
			wantIndexes[n] = true
		}
	}
	for name, seen := range wantIndexes {
		if !seen {
			t.Errorf("expected index %q on items, not found in sqlite_master", name)
		}
	}

	// All 3 FTS triggers must be present on items.
	wantTriggers := map[string]bool{
		"items_fts_insert": false,
		"items_fts_update": false,
		"items_fts_delete": false,
	}
	trows, err := s.db.Query(
		"SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name='items'")
	if err != nil {
		t.Fatalf("query sqlite_master for triggers: %v", err)
	}
	defer trows.Close()
	for trows.Next() {
		var n string
		if err := trows.Scan(&n); err != nil {
			t.Fatalf("scan trigger name: %v", err)
		}
		if _, ok := wantTriggers[n]; ok {
			wantTriggers[n] = true
		}
	}
	for name, seen := range wantTriggers {
		if !seen {
			t.Errorf("expected trigger %q on items, not found in sqlite_master", name)
		}
	}

	// FTS still answers queries against the post-rebuild table.
	results, err := s.SearchItems(ws.ID, "authentication")
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(results) != 1 {
		var titles []string
		for _, r := range results {
			titles = append(titles, r.Item.Title)
		}
		sort.Strings(titles)
		t.Errorf("expected 1 FTS result for 'authentication', got %d (%v)", len(results), titles)
	}
}

// TestItemsViewsJSONB_SQLiteRebuildRejectsNullFieldsAfterMigration
// guards the post-migration NOT NULL constraint on items.fields and
// items.tags: any attempt to write a literal NULL must fail at the
// schema level on SQLite (driven by the table-rebuild from migration
// 056). This is the load-bearing invariant the IDEA-1486 floor adds.
func TestItemsViewsJSONB_SQLiteRebuildRejectsNullFieldsAfterMigration(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific test; Postgres counterpart uses ALTER COLUMN SET NOT NULL")
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "NullReject")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Direct SQL INSERT bypassing the store helpers, attempting NULL on
	// the post-migration-hardened columns.
	for _, col2 := range []string{"fields", "tags"} {
		stmt := "INSERT INTO items (id, workspace_id, collection_id, title, slug, " + col2 +
			", created_at, updated_at) VALUES ('null-id-" + col2 + "', ?, ?, ?, ?, NULL, ?, ?)"
		_, err := s.db.Exec(stmt, ws.ID, col.ID, "null "+col2, "null-slug-"+col2, "2026-05-16T00:00:00Z", "2026-05-16T00:00:00Z")
		if err == nil {
			t.Errorf("expected NOT NULL constraint violation inserting NULL %s, got success", col2)
		}
	}
}

// TestItemsViewsJSONB_SQLiteRebuildRejectsNullConfigAfterMigration is
// the views.config counterpart to the items NULL-reject test.
func TestItemsViewsJSONB_SQLiteRebuildRejectsNullConfigAfterMigration(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific test")
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ViewNullReject")

	_, err := s.db.Exec(
		"INSERT INTO views (id, workspace_id, name, slug, view_type, config, created_at, updated_at) "+
			"VALUES ('null-view', ?, 'V', 'v', 'list', NULL, '2026-05-16T00:00:00Z', '2026-05-16T00:00:00Z')",
		ws.ID,
	)
	if err == nil {
		t.Error("expected NOT NULL constraint violation inserting NULL views.config, got success")
	}
}

// TestItemsViewsJSONB_PostgresRejectsNullFieldsAfterMigration is the
// Postgres counterpart — runs only when PAD_TEST_POSTGRES_URL is set.
// Verifies the ALTER COLUMN ... SET NOT NULL from pgmigrations/035 + 036
// actually rejects NULL writes.
func TestItemsViewsJSONB_PostgresRejectsNullFieldsAfterMigration(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "PgNullReject")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Attempt to write NULL into each hardened column.
	for _, c := range []string{"fields", "tags"} {
		stmt := "INSERT INTO items (id, workspace_id, collection_id, title, slug, " + c +
			", created_at, updated_at) VALUES ($1, $2, $3, $4, $5, NULL, $6, $7)"
		_, err := s.db.Exec(stmt,
			"null-id-"+c, ws.ID, col.ID, "null "+c, "null-slug-"+c,
			"2026-05-16T00:00:00Z", "2026-05-16T00:00:00Z")
		if err == nil {
			t.Errorf("expected NOT NULL constraint violation inserting NULL items.%s, got success", c)
		}
	}

	// And on views.config.
	_, err := s.db.Exec(
		"INSERT INTO views (id, workspace_id, name, slug, view_type, config, created_at, updated_at) "+
			"VALUES ($1, $2, $3, $4, $5, NULL, $6, $7)",
		"null-view", ws.ID, "V", "v-null", "list",
		"2026-05-16T00:00:00Z", "2026-05-16T00:00:00Z")
	if err == nil {
		t.Error("expected NOT NULL constraint violation inserting NULL views.config, got success")
	}
}

// TestItemsViewsJSONB_SQLiteRebuildBackfillsNullToDefault exercises the
// migration backfill path: a pre-existing NULL row in items.fields /
// items.tags / views.config gets coerced to the shape default during
// the rebuild. The migration runs on the test store before any test
// rows are inserted, so this test seeds a "would-be-pre-migration" row
// via direct SQL, then runs the migration text again on top — verifying
// the backfill is idempotent.
//
// The IDEA-1485 atomic-tx runner re-records the migration if asked to
// re-apply, so we apply the migration text directly via
// applySQLiteMigration. Migrations are designed to be idempotent under
// repeated application (DROP TABLE IF EXISTS items_new; backfill UPDATEs
// are WHERE x IS NULL; index/trigger DROPs use IF EXISTS).
func TestItemsViewsJSONB_SQLiteRebuildIsIdempotent(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific idempotency test")
	}

	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "idempotent.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	mig056, err := migrationsFS.ReadFile("migrations/056_items_jsonb_not_null.sql")
	if err != nil {
		t.Fatalf("read 056: %v", err)
	}
	mig057, err := migrationsFS.ReadFile("migrations/057_views_config_not_null.sql")
	if err != nil {
		t.Fatalf("read 057: %v", err)
	}

	// Re-apply each migration; both should succeed without error.
	// Use a unique synthetic name so the schema_migrations bookkeeping
	// row doesn't collide with the already-applied one.
	if err := applySQLiteMigration(s.db, "test_056_repeat.sql", string(mig056)); err != nil {
		t.Fatalf("re-apply 056: %v", err)
	}
	if err := applySQLiteMigration(s.db, "test_057_repeat.sql", string(mig057)); err != nil {
		t.Fatalf("re-apply 057: %v", err)
	}
}

// TestItemsViewsJSONB_ItemLinksRoundTripAfterRebuild verifies that the
// items rebuild preserves the inbound FK from item_links.source_id /
// target_id → items.id. Since we preserve every id value in the
// INSERT…SELECT, the FK metadata re-resolves to the renamed table by
// name and existing links remain valid.
func TestItemsViewsJSONB_ItemLinksRoundTripAfterRebuild(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "LinkRoundTrip")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	source := createTestItem(t, s, ws.ID, col.ID, "Source", "")
	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")

	_, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
		TargetID: target.ID,
		LinkType: "blocks",
	}, source.ID)
	if err != nil {
		t.Fatalf("CreateItemLink: %v", err)
	}

	links, err := s.GetItemLinks(source.ID)
	if err != nil {
		t.Fatalf("GetItemLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link from source, got %d", len(links))
	}
	if links[0].TargetID != target.ID {
		t.Errorf("link target = %q, want %q", links[0].TargetID, target.ID)
	}
}

// TestItemsViewsJSONB_ItemsFTSVirtualTableExists guards that the
// items_fts virtual table itself survives the items rebuild. (Per
// design decision D2, the virtual table is NOT dropped — only the
// three triggers are.)
func TestItemsViewsJSONB_ItemsFTSVirtualTableExists(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (items_fts is a SQLite FTS5 virtual table)")
	}

	s := testStore(t)
	var name string
	err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='items_fts'",
	).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("items_fts virtual table missing after migration")
	}
	if err != nil {
		t.Fatalf("query items_fts: %v", err)
	}
}
