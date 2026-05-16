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
			{
				// IDEA-1486 R1 codex P2: imported fields=null and
				// tags=null must coerce to defaults. Without the
				// non-nil check in coerceJSONForImport,
				// json.Unmarshal("null", &m) returns err=nil with m
				// staying nil, and the raw "null" string would land
				// in the column — JSONB null on Postgres or text
				// "null" on SQLite, both of which break downstream
				// readers expecting an object/array.
				ID:           "old-item-null",
				CollectionID: "old-coll-1",
				Title:        "Item with json null fields and tags",
				Slug:         "item-null",
				Content:      "",
				Fields:       `null`,
				Tags:         `null`,
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
		{"Item with json null fields and tags", "{}", "[]"},
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

	// All 8 indexes from 005/017/053/054 must be present on items.
	// idx_items_invocation_slug_per_collection was added to migration
	// 056 in IDEA-1486 R1 codex P1.1 after the original draft forgot
	// to recreate it; this test grew to 8 entries alongside that fix.
	wantIndexes := map[string]bool{
		"idx_items_collection":                     false,
		"idx_items_workspace":                      false,
		"idx_items_parent":                         false,
		"idx_items_updated":                        false,
		"idx_items_assigned_user":                  false,
		"idx_items_agent_role":                     false,
		"idx_items_workspace_seq":                  false,
		"idx_items_invocation_slug_per_collection": false,
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

// TestItemsViewsJSONB_SQLiteBackfillRepairsMalformedShapes is the
// codex R2 P1 regression test. It applies migrations 001-055 against
// a fresh DB (stopping BEFORE the 056/057 hardening), seeds rows that
// violate the post-migration shape contract in every observable way
// (empty string, JSON null, wrong-shape JSON, non-JSON garbage), then
// applies 056 + 057 and asserts every malformed row is repaired to
// the default.
//
// The CREATE UNIQUE INDEX idx_items_invocation_slug_per_collection
// in 056 calls json_extract on every fields value — a single row
// with malformed JSON would error mid-migration. The "garbage"
// seeding below is the actual ship-breaker scenario; without the
// widened WHERE clause, the migration aborts.
func TestItemsViewsJSONB_SQLiteBackfillRepairsMalformedShapes(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific (json_valid / json_type)")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "backfill_shape.db")
	dsn := dbPath + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("WAL: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}

	// Apply migrations up to BUT NOT INCLUDING 054 (which creates the
	// partial UNIQUE index on json_extract(fields, '$.invocation_slug')),
	// 055, 056, 057. We then seed pre-054 malformed rows, then apply
	// 056 + 057. The codex R2 P1 ship-breaker scenario is precisely:
	// rows with malformed items.fields exist, then 056 recreates the
	// partial UNIQUE index over those rows, and CREATE INDEX errors
	// mid-migration when json_extract evaluates on the bad rows.
	//
	// We skip 054 in the pre-seed pass because 054's own CREATE INDEX
	// would also error on malformed rows; 056 is the migration under
	// test, and its widened backfill WHERE clause is the fix. After
	// 056 repairs and rebuilds, the post-56 partial index sees only
	// well-shaped rows.
	names, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	postSeed := map[string]bool{
		"054_playbooks_invocation_fields.sql":   true,
		"055_collections_settings_not_null.sql": true,
		"056_items_jsonb_not_null.sql":          true,
		"057_views_config_not_null.sql":         true,
	}
	for _, name := range names {
		if postSeed[name] {
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	// Seed prerequisites — a workspace + collection so item FKs resolve.
	const wsID = "ws-bf"
	const collID = "coll-bf"
	const ts = "2026-05-16T00:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO workspaces (id, name, slug, settings, created_at, updated_at)
		 VALUES (?, ?, ?, '{}', ?, ?)`,
		wsID, "BFShape", "bfshape", ts, ts,
	); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO collections (id, workspace_id, name, slug, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		collID, wsID, "Tasks", "tasks-bf", ts, ts,
	); err != nil {
		t.Fatalf("seed collection: %v", err)
	}

	// Seed items with every observable shape pathology. Pre-056 the
	// items.fields column is nullable + no shape check, so every one
	// of these INSERTs succeeds.
	itemSeeds := []struct {
		id     string
		slug   string
		fields any // string or nil for SQL NULL
		tags   any
	}{
		{"i-null", "i-null", nil, nil},                        // SQL NULL on both
		{"i-empty", "i-empty", "", ""},                        // empty-string sentinel
		{"i-jsnull", "i-jsnull", "null", "null"},              // JSON null literal
		{"i-wrongshape", "i-wrong", "[]", "{}"},               // wrong shape on each
		{"i-garbage", "i-garbage", "not json", "garb"},        // non-JSON text
		{"i-valid", "i-valid", `{"k":"v"}`, `["a"]`},          // already valid — must be preserved
		{"i-slug", "i-slug", `{"invocation_slug":"x"}`, `[]`}, // exercises the partial UNIQUE index
	}
	for _, sd := range itemSeeds {
		if _, err := db.Exec(
			`INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, tags, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sd.id, wsID, collID, sd.id, sd.slug, sd.fields, sd.tags, ts, ts,
		); err != nil {
			t.Fatalf("seed item %s: %v", sd.id, err)
		}
	}

	// Seed views with shape pathologies on config.
	viewSeeds := []struct {
		id     string
		slug   string
		config any
	}{
		{"v-null", "v-null", nil},
		{"v-empty", "v-empty", ""},
		{"v-jsnull", "v-jsnull", "null"},
		{"v-arr", "v-arr", "[]"},
		{"v-garbage", "v-garb", "{garbage"},
		{"v-valid", "v-valid", `{"layout":"list"}`},
	}
	for _, sd := range viewSeeds {
		if _, err := db.Exec(
			`INSERT INTO views (id, workspace_id, name, slug, view_type, config, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'list', ?, ?, ?)`,
			sd.id, wsID, sd.id, sd.slug, sd.config, ts, ts,
		); err != nil {
			t.Fatalf("seed view %s: %v", sd.id, err)
		}
	}

	// Now apply 055, 056, 057. 054 is deliberately skipped because
	// its own CREATE INDEX on json_extract(fields, '$.invocation_slug')
	// would error on the malformed seed rows — and in production, 054
	// already shipped without that error, which means no production
	// row carried malformed fields at 054-run-time. The codex R2 P1
	// concern is forward-looking: if malformed rows appeared between
	// 054 and 056 (or any other source), 056's REBUILD recreates the
	// same partial UNIQUE index and would re-hit the json_extract
	// error. The widened backfill WHERE clause in 056 repairs those
	// rows BEFORE the rebuild, so the migration succeeds end-to-end.
	postSeedOrder := []string{
		"055_collections_settings_not_null.sql",
		"056_items_jsonb_not_null.sql",
		"057_views_config_not_null.sql",
	}
	for _, name := range postSeedOrder {
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := applySQLiteMigration(db, name, string(data)); err != nil {
			t.Fatalf("apply %s (codex R2 P1 ship-breaker): %v", name, err)
		}
	}

	// Assert every malformed item.fields was repaired to "{}", every
	// malformed item.tags to "[]", and the valid rows survived.
	type wantRow struct{ fields, tags string }
	itemWant := map[string]wantRow{
		"i-null":       {`{}`, `[]`},
		"i-empty":      {`{}`, `[]`},
		"i-jsnull":     {`{}`, `[]`},
		"i-wrongshape": {`{}`, `[]`},
		"i-garbage":    {`{}`, `[]`},
		"i-valid":      {`{"k":"v"}`, `["a"]`},
		"i-slug":       {`{"invocation_slug":"x"}`, `[]`},
	}
	for id, want := range itemWant {
		var f, ta string
		err := db.QueryRow(`SELECT fields, tags FROM items WHERE id = ?`, id).Scan(&f, &ta)
		if err != nil {
			t.Fatalf("query item %s: %v", id, err)
		}
		if f != want.fields {
			t.Errorf("item %s fields: want %q, got %q", id, want.fields, f)
		}
		if ta != want.tags {
			t.Errorf("item %s tags: want %q, got %q", id, want.tags, ta)
		}
	}

	viewWant := map[string]string{
		"v-null":    `{}`,
		"v-empty":   `{}`,
		"v-jsnull":  `{}`,
		"v-arr":     `{}`,
		"v-garbage": `{}`,
		"v-valid":   `{"layout":"list"}`,
	}
	for id, want := range viewWant {
		var c string
		err := db.QueryRow(`SELECT config FROM views WHERE id = ?`, id).Scan(&c)
		if err != nil {
			t.Fatalf("query view %s: %v", id, err)
		}
		if c != want {
			t.Errorf("view %s config: want %q, got %q", id, want, c)
		}
	}

	// The partial UNIQUE index must exist after the rebuild AND the
	// uniqueness constraint must actually fire on duplicate
	// invocation_slug — proof that the index works on post-rebuild rows
	// AND that the migration succeeded end-to-end (which it could not
	// have if json_extract had errored mid-CREATE INDEX).
	var idxName string
	err = db.QueryRow(
		`SELECT name FROM sqlite_master
		 WHERE type='index' AND tbl_name='items'
		   AND name='idx_items_invocation_slug_per_collection'`,
	).Scan(&idxName)
	if err != nil {
		t.Fatalf("invocation_slug index missing after migration: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, tags, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"i-slug-dup", wsID, collID, "dup", "i-slug-dup",
		`{"invocation_slug":"x"}`, `[]`, ts, ts,
	); err == nil {
		t.Error("expected UNIQUE constraint violation on duplicate invocation_slug, got success")
	}
}

// TestItemsViewsJSONB_PostgresBackfillRepairsMalformedShapes is the
// Postgres counterpart. JSONB columns reject invalid JSON at write
// time, so the only pre-existing pathologies are SQL NULL and JSONB-
// valid-but-wrong-shape (e.g. JSONB null, an array where an object is
// required, a primitive). Codex R2 P1: the original NULL-only WHERE
// would have left wrong-shape rows in place.
func TestItemsViewsJSONB_PostgresBackfillRepairsMalformedShapes(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	_ = pgURL // testStore picks up the env

	s := testStore(t)
	ws := createTestWorkspace(t, s, "PgBackfill")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Direct INSERT with wrong-shape JSONB. Bypasses store helpers and
	// writes JSONB null / array / primitive into the columns. JSONB
	// null satisfies NOT NULL (SQL NULL ≠ JSONB null) but fails
	// jsonb_typeof = 'object'.
	const ts = "2026-05-16T00:00:00Z"
	mustInsert := func(t *testing.T, id, fields, tags string) {
		t.Helper()
		_, err := s.db.Exec(
			`INSERT INTO items (id, workspace_id, collection_id, title, slug, fields, tags, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9)`,
			id, ws.ID, col.ID, id, id, fields, tags, ts, ts,
		)
		if err != nil {
			t.Fatalf("seed item %s: %v", id, err)
		}
	}
	mustInsert(t, "pg-jsnull", `null`, `null`)
	mustInsert(t, "pg-wrongshape", `[]`, `{}`)
	mustInsert(t, "pg-prim", `42`, `"a string"`)
	mustInsert(t, "pg-valid", `{"k":"v"}`, `["tag"]`)

	// Re-run the backfill UPDATE clauses from pgmigrations/035 (the
	// migration itself already ran during testStore init; this
	// exercises the UPDATE shape against rows seeded above).
	if _, err := s.db.Exec(
		"UPDATE items SET fields = '{}'::jsonb WHERE fields IS NULL OR jsonb_typeof(fields) != 'object'"); err != nil {
		t.Fatalf("re-run fields backfill: %v", err)
	}
	if _, err := s.db.Exec(
		"UPDATE items SET tags = '[]'::jsonb WHERE tags IS NULL OR jsonb_typeof(tags) != 'array'"); err != nil {
		t.Fatalf("re-run tags backfill: %v", err)
	}

	want := map[string]struct{ fields, tags string }{
		"pg-jsnull":     {`{}`, `[]`},
		"pg-wrongshape": {`{}`, `[]`},
		"pg-prim":       {`{}`, `[]`},
		"pg-valid":      {`{"k": "v"}`, `["tag"]`}, // Postgres adds whitespace
	}
	for id, exp := range want {
		var f, ta string
		err := s.db.QueryRow(`SELECT fields::text, tags::text FROM items WHERE id = $1`, id).Scan(&f, &ta)
		if err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if !jsonEqualString(f, exp.fields) {
			t.Errorf("item %s fields want %q got %q", id, exp.fields, f)
		}
		if !jsonEqualString(ta, exp.tags) {
			t.Errorf("item %s tags want %q got %q", id, exp.tags, ta)
		}
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
