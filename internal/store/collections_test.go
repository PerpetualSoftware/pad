package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
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
//
// The defensive `sql.NullString` scans in collections.go / export.go were
// reverted in the IDEA-1484 follow-up — direct string scans are safe now
// that the column cannot hold NULL.
//
// The import-side `""→"{}"` coercion in ImportWorkspace is INTENTIONALLY
// RETAINED. The NOT NULL DEFAULT '{}' constraint only fires when the
// INSERT omits the column; ImportWorkspace explicitly supplies the value,
// so a `""` from a legacy bundle or external JSON import would bypass the
// default and either fail on Postgres (invalid JSONB) or silently store
// invalid JSON on SQLite. `TestExportImportRoundTripWithEmptyStringSettings`
// guards that boundary normalization.

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
// readback); both surface through GetCollection as the Go string `{}`.
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

// TestUpdateCollectionCoercesEmptyStringSettings guards the second
// boundary normalizer: UpdateCollection writes a `settings=""` PATCH
// verbatim by default, bypassing the NOT NULL DEFAULT '{}' constraint
// (which only fires on column-omission, not on explicit values).
// Postgres would reject `""` at JSONB type-validation; SQLite would
// silently store invalid JSON. The coercion in UpdateCollection matches
// the one in ImportWorkspace — both protect the
// `collections.settings always holds valid JSON` invariant at the API
// boundary, where the schema constraint alone is not sufficient.
func TestUpdateCollectionCoercesEmptyStringSettings(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Update Coerce Empty Settings")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Things",
		Slug:     "things-update-coerce",
		Settings: `{"done_field":"status","done_values":["closed"]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	empty := ""
	updated, err := s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings: &empty,
	})
	if err != nil {
		t.Fatalf("UpdateCollection with empty-string settings should coerce, got error: %v", err)
	}
	if updated == nil {
		t.Fatalf("UpdateCollection returned nil")
	}
	if updated.Settings != "{}" {
		t.Errorf("expected UpdateCollection to coerce empty-string settings to %q, got %q", "{}", updated.Settings)
	}
}

// TestUpdateCollectionExpectedUpdatedAtMatch: a matching optimistic-concurrency
// token (BUG-2265) lets the settings write through unchanged. Mirrors the item
// path's TestUpdateItemExpectedUpdatedAtMatch.
func TestUpdateCollectionExpectedUpdatedAtMatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollMatch")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things",
		Slug: "things-occ-match",
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	expected := created.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	settings := `{"quick_actions":[{"label":"Ship","prompt":"/pad ship","scope":"item"}]}`
	updated, err := s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings:          &settings,
		ExpectedUpdatedAt: expected,
	})
	if err != nil {
		t.Fatalf("UpdateCollection with matching expected_updated_at should succeed: %v", err)
	}
	if updated == nil {
		t.Fatalf("UpdateCollection returned nil")
	}
	// Compare semantically — Postgres re-serializes the JSONB column (spaces /
	// key order differ from the literal we sent), so an exact-string check
	// would be a false failure on PG while passing on SQLite.
	var got models.CollectionSettings
	if err := json.Unmarshal([]byte(updated.Settings), &got); err != nil {
		t.Fatalf("parse updated settings %q: %v", updated.Settings, err)
	}
	if len(got.QuickActions) != 1 || got.QuickActions[0].Label != "Ship" {
		t.Errorf("expected the Ship quick action to be written, got %q", updated.Settings)
	}
}

// TestUpdateCollectionExpectedUpdatedAtConflict: a stale token is rejected with
// *CollectionUpdateConflictError and the write does NOT land — the BUG-2265
// last-write-wins fix. Mirrors TestUpdateItemExpectedUpdatedAtConflict.
func TestUpdateCollectionExpectedUpdatedAtConflict(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollConflict")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Things",
		Slug:     "things-occ-conflict",
		Settings: `{"layout":"balanced"}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	// A timestamp that definitely doesn't match the row's updated_at.
	stale := "2000-01-01T00:00:00Z"
	newSettings := `{"quick_actions":[{"label":"X","prompt":"y","scope":"item"}]}`
	_, err = s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings:          &newSettings,
		ExpectedUpdatedAt: stale,
	})
	if err == nil {
		t.Fatal("expected CollectionUpdateConflictError, got nil")
	}
	var conflict *CollectionUpdateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *CollectionUpdateConflictError, got %T: %v", err, err)
	}
	if conflict.ExpectedUpdatedAt != stale {
		t.Errorf("conflict.ExpectedUpdatedAt: got %q want %q", conflict.ExpectedUpdatedAt, stale)
	}
	if conflict.ActualUpdatedAt.IsZero() {
		t.Error("conflict.ActualUpdatedAt should carry the row's real timestamp")
	}

	// The write must NOT have landed — settings unchanged (semantic compare;
	// see the match test for why exact-string fails on Postgres JSONB).
	reread, err := s.GetCollection(created.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var got models.CollectionSettings
	if err := json.Unmarshal([]byte(reread.Settings), &got); err != nil {
		t.Fatalf("parse reread settings %q: %v", reread.Settings, err)
	}
	if got.Layout != "balanced" || len(got.QuickActions) != 0 {
		t.Errorf("settings should be unchanged after a rejected conflict, got %q", reread.Settings)
	}
}

// TestUpdateCollectionAppliesMigrationsAtomically (BUG-2265 Codex P1): the
// schema change and its field-value migration commit together in ONE tx — the
// item's renamed option value lands with the schema, and the concurrency token
// advances once. (Failure-rollback is covered structurally: the migration runs
// inside the same tx, so an error returns before commit.)
func TestUpdateCollectionAppliesMigrationsAtomically(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollMigrate")

	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks-occ-migrate",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","closed"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "T", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	token := coll.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	newSchema := `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","closed"]}]}`
	updated, err := s.UpdateCollection(coll.ID, models.CollectionUpdate{
		Schema:            &newSchema,
		ExpectedUpdatedAt: token,
		Migrations:        []models.FieldMigration{{Field: "status", RenameOptions: map[string]string{"open": "active"}}},
	})
	if err != nil {
		t.Fatalf("UpdateCollection with migration: %v", err)
	}
	// Schema change landed (compare semantically — Postgres re-serializes the
	// JSONB schema column, so an exact-string check would false-fail on PG).
	var gotSchema models.CollectionSchema
	if err := json.Unmarshal([]byte(updated.Schema), &gotSchema); err != nil {
		t.Fatalf("parse updated schema %q: %v", updated.Schema, err)
	}
	if len(gotSchema.Fields) != 1 || len(gotSchema.Fields[0].Options) != 2 ||
		gotSchema.Fields[0].Options[0] != "active" || gotSchema.Fields[0].Options[1] != "closed" {
		t.Errorf("schema options not updated to [active closed]: %q", updated.Schema)
	}
	// Token advanced exactly (strictly past the accepted token).
	if !updated.UpdatedAt.After(coll.UpdatedAt) {
		t.Errorf("token did not advance: %s !> %s", updated.UpdatedAt.UTC(), coll.UpdatedAt.UTC())
	}
	// Item value migrated in the SAME commit.
	got, err := s.GetItem(item.ID)
	if err != nil || got == nil {
		t.Fatalf("GetItem: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(got.Fields), &fields); err != nil {
		t.Fatalf("parse item fields %q: %v", got.Fields, err)
	}
	if fields["status"] != "active" {
		t.Errorf("item field value not migrated, got %v (fields %q)", fields["status"], got.Fields)
	}
}

// TestUpdateCollectionMigrationNoDeadlockWithItemCreate is the BUG-2265 Codex
// round-4 lock-ordering regression guard. UpdateCollection's migration path and
// item creation both lock the workspace advisory/seq lock AND a collection row;
// they MUST take them in the same order (workspace lock first, then the
// collection row) or a concurrent item-create + schema-migration ABBA-deadlocks.
// On Postgres a wrong order surfaces as a "deadlock detected" error (the
// detector aborts a tx rather than hanging); on SQLite the single-writer lock
// serializes everything. Either way, with the correct order every worker
// succeeds.
func TestUpdateCollectionMigrationNoDeadlockWithItemCreate(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollDeadlock")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks-occ-deadlock",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["a","b"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	const workers = 8
	var ready sync.WaitGroup
	ready.Add(workers)
	release := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ready.Done()
			<-release // all workers contend simultaneously
			if n%2 == 0 {
				// Migration path: takes the workspace seq lock + the collection
				// row FOR UPDATE. A non-matching rename still exercises both
				// locks (len(Migrations) > 0 is the only gate).
				_, e := s.UpdateCollection(coll.ID, models.CollectionUpdate{
					Migrations: []models.FieldMigration{{Field: "status", RenameOptions: map[string]string{"z": "y"}}},
				})
				errCh <- e
			} else {
				// Item creation: takes the workspace advisory lock, then the
				// collection-row FK lock on INSERT.
				_, e := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
					Title:  fmt.Sprintf("i%d", n),
					Fields: `{"status":"a"}`,
				})
				errCh <- e
			}
		}(i)
	}
	ready.Wait()
	close(release)
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatalf("concurrent item-create + schema-migration errored (ABBA-deadlock regression?): %v", e)
		}
	}
}

// TestUpdateCollectionTokenAdvancesPreventsSameSecondClobber is the BUG-2265
// same-second regression guard (Codex P1): two guarded writes reusing the SAME
// token must not both succeed even when they land in the same wall-clock second
// (now() is one-second precision). The first accepted write advances updated_at
// strictly past the token, so replaying the stale token conflicts — DETERMINISTIC
// regardless of timing.
func TestUpdateCollectionTokenAdvancesPreventsSameSecondClobber(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollSameSecond")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things",
		Slug: "things-occ-samesecond",
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	token := created.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")

	first := `{"layout":"content-primary"}`
	updated, err := s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings:          &first,
		ExpectedUpdatedAt: token,
	})
	if err != nil {
		t.Fatalf("first guarded update should succeed: %v", err)
	}
	// The accepted write must have advanced updated_at strictly past the token
	// even if it committed in the same second the token was read.
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at must advance past the token: created=%s updated=%s",
			created.UpdatedAt.UTC(), updated.UpdatedAt.UTC())
	}

	// Replaying the SAME (now stale) token must conflict, not clobber.
	second := `{"layout":"fields-primary"}`
	_, err = s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings:          &second,
		ExpectedUpdatedAt: token,
	})
	var conflict *CollectionUpdateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("replaying the stale token must conflict, got %T: %v", err, err)
	}

	// The first write must still be the current state (second write rejected).
	reread, err := s.GetCollection(created.ID)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	var got models.CollectionSettings
	if err := json.Unmarshal([]byte(reread.Settings), &got); err != nil {
		t.Fatalf("parse reread settings %q: %v", reread.Settings, err)
	}
	if got.Layout != "content-primary" {
		t.Errorf("expected the first write to survive, got %q", reread.Settings)
	}
}

// TestUpdateCollectionTokenlessWriteStillAdvancesToken is the BUG-2265 Codex
// round-2 guard: a TOKENLESS update must also advance updated_at strictly past
// the row's current value, so it can't regress the concurrency token in the
// same wall-clock second and let a stale guarded token clobber newer data.
func TestUpdateCollectionTokenlessWriteStillAdvancesToken(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollTokenlessMono")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things",
		Slug: "things-occ-tokenless-mono",
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}
	token := created.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")

	// Tokenless write (the CLI/MCP/API path) — must advance updated_at even if
	// it lands in the same second the token was read.
	first := `{"layout":"content-primary"}`
	updated, err := s.UpdateCollection(created.ID, models.CollectionUpdate{Settings: &first})
	if err != nil {
		t.Fatalf("tokenless update should succeed: %v", err)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("tokenless update must advance updated_at: created=%s updated=%s",
			created.UpdatedAt.UTC(), updated.UpdatedAt.UTC())
	}

	// A guarded write replaying the pre-tokenless token must now conflict.
	second := `{"layout":"fields-primary"}`
	_, err = s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings:          &second,
		ExpectedUpdatedAt: token,
	})
	var conflict *CollectionUpdateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale guarded token must conflict after a tokenless write, got %T: %v", err, err)
	}
}

// TestUpdateCollectionNoTokenSkipsConcurrencyCheck: omitting the token keeps
// the legacy last-write-wins path — an unconditional write that always lands
// (CLI / MCP / API callers that don't opt in). BUG-2265 is additive.
func TestUpdateCollectionNoTokenSkipsConcurrencyCheck(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCCollNoToken")

	created, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Things",
		Slug: "things-occ-notoken",
	})
	if err != nil {
		t.Fatalf("CreateCollection error: %v", err)
	}

	settings := `{"layout":"content-primary"}`
	updated, err := s.UpdateCollection(created.ID, models.CollectionUpdate{
		Settings: &settings, // no ExpectedUpdatedAt
	})
	if err != nil {
		t.Fatalf("UpdateCollection without token should succeed: %v", err)
	}
	var got models.CollectionSettings
	if err := json.Unmarshal([]byte(updated.Settings), &got); err != nil {
		t.Fatalf("parse updated settings %q: %v", updated.Settings, err)
	}
	if got.Layout != "content-primary" {
		t.Errorf("settings not written: got %q", updated.Settings)
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

// TestExportImportRoundTripWithEmptyStringSettings guards the import-side
// `""→"{}"` coercion in ImportWorkspace. IDEA-1484 (PR #562) hardened
// collections.settings to NOT NULL DEFAULT '{}', but the DEFAULT clause
// only fires when the INSERT omits the column — ImportWorkspace
// explicitly supplies the value. Without the coercion, a legacy bundle
// or external JSON payload whose settings is "" would fail at Postgres's
// JSONB type-validation (and silently store invalid JSON on SQLite).
// This test simulates the bundle by mutating the in-memory export bundle
// to inject "" settings, then asserts ImportWorkspace materializes them
// back to valid JSON on the destination side.
func TestExportImportRoundTripWithEmptyStringSettings(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "round-trip-owner@test.com", "Round Trip Owner", "password123")
	src := createTestWorkspace(t, s, "Export-Import Round Trip Empty Settings")

	if err := s.SeedDefaultCollections(src.ID); err != nil {
		t.Fatalf("SeedDefaultCollections error: %v", err)
	}

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace error: %v", err)
	}

	// Simulate a legacy/external bundle whose collections carry "" settings.
	for i := range exp.Collections {
		exp.Collections[i].Settings = ""
	}

	imported, err := s.ImportWorkspace(exp, "round-trip-import-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace error (IDEA-1484 import-side coercion regression): %v", err)
	}
	if imported == nil {
		t.Fatalf("ImportWorkspace returned nil workspace")
	}

	// Re-read the imported collections and assert they hold valid JSON
	// (the import-side coercion materialized `""` back to `"{}"`).
	colls, err := s.ListCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListCollections on imported workspace: %v", err)
	}
	if len(colls) == 0 {
		t.Fatalf("imported workspace has 0 collections; expected the round-tripped defaults")
	}
	for _, c := range colls {
		if c.Settings == "" {
			t.Errorf("imported collection %q: settings should have been coerced from \"\" to a valid JSON object, got empty string", c.ID)
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(c.Settings), &got); err != nil {
			t.Errorf("imported collection %q: settings is not valid JSON: %v (raw=%q)", c.ID, err, c.Settings)
		}
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

	// Exactly one seeded item: the universal /pad onboard playbook
	// (PLAN-1496 / TASK-1500). No sample items, no seeded conventions,
	// no other playbooks — the blank template's whole point is "agent
	// drives setup", and the onboard playbook is what makes /pad
	// onboard invokable on day one.
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("blank workspace has %d items, want 1 (the seeded onboard playbook)", len(items))
	}
	if len(items) >= 1 && items[0].Title != "Onboard a workspace" {
		t.Errorf("blank workspace's sole seeded item should be the onboard playbook; got %q", items[0].Title)
	}
}

// TestSeedFromTemplateAlwaysIncludesOnboardPlaybook covers TASK-1500's
// contract: the /pad onboard playbook is auto-seeded into EVERY
// workspace created with a real template (blank, startup, scrum,
// product, hiring, interviewing — anything that calls
// SeedCollectionsFromTemplate with a non-empty templateName).
//
// The empty-template-name path (templateName == "") is the explicit
// backward-compat escape hatch for tests and direct API callers and
// must NOT get the auto-seed; that's covered by the existing
// dashboard / list-items tests in internal/server/ which all rely on
// "empty Template + zero items" semantics.
func TestSeedFromTemplateAlwaysIncludesOnboardPlaybook(t *testing.T) {
	for _, name := range []string{"blank", "startup", "scrum", "product", "hiring", "interviewing"} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, name+" Onboard Seed Test")
			if err := s.SeedCollectionsFromTemplate(ws.ID, name); err != nil {
				t.Fatalf("SeedCollectionsFromTemplate(%s) error: %v", name, err)
			}
			items, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "playbooks"})
			if err != nil {
				t.Fatalf("ListItems error: %v", err)
			}
			var found bool
			for _, it := range items {
				if it.Title == "Onboard a workspace" {
					found = true
					break
				}
			}
			if !found {
				titles := make([]string, 0, len(items))
				for _, it := range items {
					titles = append(titles, it.Title)
				}
				t.Errorf("template %q workspace missing the /pad onboard playbook; got playbook titles: %v", name, titles)
			}
		})
	}
}

// TestSeedWithEmptyTemplateNameSkipsOnboard locks the escape-hatch
// invariant: SeedCollectionsFromTemplate(ws, "") must NOT auto-seed
// the onboard playbook. This is the path tests and direct API
// callers use to get a bare workspace.
func TestSeedWithEmptyTemplateNameSkipsOnboard(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Empty-Template Onboard Skip Test")
	if err := s.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate(ws, \"\") error: %v", err)
	}
	items, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems error: %v", err)
	}
	if len(items) != 0 {
		titles := make([]string, 0, len(items))
		for _, it := range items {
			titles = append(titles, it.Title)
		}
		t.Errorf("empty-template path should seed zero items; got %d (%v)", len(items), titles)
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
