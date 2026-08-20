package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollectionTraitsBackfill exercises the backfill half of migration
// 080 (SQLite) / 058 (Postgres) — the part that protects EXISTING workspaces.
//
// Ordinary tests can't reach it: migrations run before any collection exists,
// and every collection created afterwards gets its traits from the template.
// So the backfill statements — the only thing standing between an upgraded
// deployment and a silently trait-less conventions collection — would ship
// with zero coverage. This test manufactures the pre-upgrade state (traits
// reset to '{}') and re-runs the migration's UPDATE against it.
//
// Runs against whichever dialect testStore provides, so `make test-pg` covers
// the Postgres statements and the default suite covers SQLite.
func TestCollectionTraitsBackfill(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Backfill Test")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("no conventions collection: %v", err)
	}
	pb, err := s.GetCollectionBySlug(ws.ID, "playbooks")
	if err != nil || pb == nil {
		t.Fatalf("no playbooks collection: %v", err)
	}

	// Manufacture the pre-upgrade state: a workspace whose system collections
	// carry no declarations, exactly as every workspace created before
	// TASK-2657 does.
	for _, id := range []string{conv.ID, pb.ID} {
		if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = '{}' WHERE id = ?`), id); err != nil {
			t.Fatalf("reset traits: %v", err)
		}
	}

	// Control leg: confirm the reset actually landed. Without this the
	// assertions below would pass on a no-op reset, proving nothing.
	if got := mustTraits(t, s, conv.ID); !got.IsZero() {
		t.Fatalf("control leg failed: traits still populated after reset: %+v", got)
	}

	runBackfillStatements(t, s)

	convTraits := mustTraits(t, s, conv.ID)
	if len(convTraits.BootstrapInclude) != 2 {
		t.Errorf("conventions backfill: got %d bootstrap includes, want 2", len(convTraits.BootstrapInclude))
	}
	bodies := convTraits.BootstrapIncludeForKey("conventions")
	if bodies == nil {
		t.Fatal("conventions backfill: no bodies include")
	}
	if bodies.Mode != models.BootstrapModeBodies {
		t.Errorf("conventions bodies include mode = %q, want %q", bodies.Mode, models.BootstrapModeBodies)
	}
	// status=active is the amendment that keeps DRAFT conventions out of the
	// boot payload; a backfill that omits it silently ships draft rules to
	// every agent in an upgraded workspace.
	if bodies.Filter["status"] != "active" || bodies.Filter["trigger"] != "always" {
		t.Errorf("conventions bodies filter = %+v, want status=active trigger=always", bodies.Filter)
	}
	if idx := convTraits.BootstrapIncludeForKey("convention_index"); idx == nil {
		t.Error("conventions backfill: no convention_index include")
	} else if idx.Mode != models.BootstrapModeMetadata || idx.Filter["status"] != "active" {
		t.Errorf("convention_index include = %+v, want metadata/status=active", idx)
	}
	if convTraits.ArtifactKind == nil || convTraits.ArtifactKind.Kind != "convention" {
		t.Errorf("conventions artifact_kind = %+v, want convention", convTraits.ArtifactKind)
	}

	pbTraits := mustTraits(t, s, pb.ID)
	if pbTraits.InvocationField != models.InvocationSlugField {
		t.Errorf("playbooks invocation_field = %q, want %q", pbTraits.InvocationField, models.InvocationSlugField)
	}
	if pbTraits.ArtifactKind == nil || pbTraits.ArtifactKind.Kind != "playbook" {
		t.Errorf("playbooks artifact_kind = %+v, want playbook", pbTraits.ArtifactKind)
	}
	if inc := pbTraits.BootstrapIncludeForKey("playbooks"); inc == nil {
		t.Error("playbooks backfill: no include")
	} else if len(inc.Filter) != 0 {
		t.Errorf("playbooks include filter = %+v, want empty (draft playbooks are listed deliberately)", inc.Filter)
	}

	// Everything the backfill writes must satisfy the same grammar the API
	// enforces — otherwise upgraded workspaces carry declarations that a
	// later edit through the handler would reject.
	if err := convTraits.Validate(); err != nil {
		t.Errorf("backfilled conventions traits fail validation: %v", err)
	}
	if err := pbTraits.Validate(); err != nil {
		t.Errorf("backfilled playbooks traits fail validation: %v", err)
	}

	// Re-running must not disturb populated declarations — the statements are
	// guarded on traits='{}' so an operator re-applying a migration, or a
	// partially-applied upgrade, can't clobber a workspace's own edits.
	//
	// The workspace's declarations have to DIFFER from what the backfill
	// writes for this to prove anything: re-running an unguarded backfill
	// would write the identical canonical value, so asserting "still 2
	// includes" passes with or without the guard.
	custom := models.CollectionTraits{
		BootstrapInclude: []models.BootstrapInclude{
			{Mode: models.BootstrapModeBodies, Filter: map[string]string{"status": "active"}, Key: "conventions"},
		},
		ArtifactKind: &models.ArtifactKindTrait{Kind: "convention"},
	}
	customJSON, err := custom.JSON()
	if err != nil {
		t.Fatalf("marshal custom traits: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), customJSON, conv.ID); err != nil {
		t.Fatalf("write custom traits: %v", err)
	}

	runBackfillStatements(t, s)

	again := mustTraits(t, s, conv.ID)
	if len(again.BootstrapInclude) != 1 {
		t.Errorf("re-running the backfill clobbered a workspace's own declarations: got %d includes, want the 1 it had set", len(again.BootstrapInclude))
	}
	if inc := again.BootstrapIncludeForKey("conventions"); inc != nil && inc.Filter["trigger"] == "always" {
		t.Error("re-running the backfill overwrote the workspace's filter with the canonical one")
	}
}

// runBackfillStatements replays the migration's UPDATE statements. The
// migration itself is version-tracked and won't re-run, so the statements are
// read out of the embedded migration file rather than duplicated here — a copy
// would drift from the SQL that actually ships.
func runBackfillStatements(t *testing.T, s *Store) {
	t.Helper()

	name, dir := "migrations/080_collection_traits.sql", migrationsFS
	if s.dialect.Driver() == DriverPostgres {
		name = "pgmigrations/058_collection_traits.sql"
		dir = pgMigrationsFS
	}
	raw, err := dir.ReadFile(name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}

	// Strip comment lines before splitting. The migration's prose contains
	// semicolons, so a naive split on ";" tears statements apart mid-comment.
	// (The production runner sidesteps this by consuming leading comment lines
	// before it looks for a statement end — see execMulti.)
	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	var ran int
	for _, stmt := range strings.Split(sb.String(), ";") {
		trimmed := strings.TrimSpace(stmt)
		// Only the backfill UPDATEs — the ALTER TABLE already ran as part of
		// normal migration and would error on a second application.
		if !strings.Contains(strings.ToUpper(trimmed), "UPDATE COLLECTIONS") {
			continue
		}
		if _, err := s.db.Exec(trimmed); err != nil {
			t.Fatalf("backfill statement failed: %v\n%s", err, trimmed)
		}
		ran++
	}
	// Guard against the test silently exercising nothing if the migration is
	// ever restructured (renamed, split, statements reworded).
	if ran != 2 {
		t.Fatalf("expected 2 backfill UPDATE statements in %s, ran %d", name, ran)
	}
}

func mustTraits(t *testing.T, s *Store, collID string) models.CollectionTraits {
	t.Helper()
	coll, err := s.GetCollection(collID)
	if err != nil || coll == nil {
		t.Fatalf("get collection %s: %v", collID, err)
	}
	traits, err := models.ParseCollectionTraits(coll.Traits)
	if err != nil {
		t.Fatalf("parse traits: %v (raw %q)", err, coll.Traits)
	}
	return traits
}

// TestFilterKeyRuleMatchesStoreSanitizer pins CollectionTraits.Validate's
// filter-key rule to the store's own field-key sanitizer.
//
// The two are duplicated on purpose — models cannot import store — and the
// duplication is load-bearing in the fail-OPEN direction: ListItems silently
// drops any field key isValidFieldKey rejects, removing that predicate from
// the WHERE clause instead of matching nothing. If the store's rule ever grows
// stricter than the validator's, a declaration would pass validation and then
// have its filter silently discarded, shipping an unfiltered payload. This
// test fails the moment the two rules disagree.
func TestFilterKeyRuleMatchesStoreSanitizer(t *testing.T) {
	keys := []string{
		"status", "trigger", "invocation_slug", "agent-role", "a1", "_leading",
		"", " ", "stat us", "status;drop", "sta.tus", "status'", "état", "a\tb",
		"status\n", "$status", "sta/tus",
	}
	for _, k := range keys {
		storeAccepts := isValidFieldKey(k)
		validatorAccepts := models.CollectionTraits{
			BootstrapInclude: []models.BootstrapInclude{
				{Mode: models.BootstrapModeBodies, Key: "x", Filter: map[string]string{k: "active"}},
			},
		}.Validate() == nil
		if storeAccepts != validatorAccepts {
			t.Errorf("key %q: store sanitizer accepts=%v, trait validator accepts=%v — a disagreement here lets a declaration pass validation and then lose its filter silently", k, storeAccepts, validatorAccepts)
		}
	}
}

// TestImportLegacyArchiveInfersTraits locks the compatibility inference for
// archives written before the traits column existed (Codex round 4, P1).
//
// The migration backfill cannot help here: it ran long before these rows were
// inserted, and nothing re-runs it per import. So without inference an imported
// pre-TASK-2657 workspace comes up with a conventions collection that declares
// NOTHING — no always-on rules in bootstrap, no playbook invocation routing, no
// artifact export. The archive looks fine and the workspace is quietly inert:
// exactly the BUG-2702 failure mode, reintroduced through a different door.
func TestImportLegacyArchiveInfersTraits(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "legacy-archive-owner@test.com", "Legacy Owner", "password123")
	src := createTestWorkspace(t, s, "Legacy Archive Source")
	if err := s.SeedCollectionsFromTemplate(src.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	// Simulate an archive written before the column existed: the key is
	// absent, which decodes to "".
	var sawConventions bool
	for i := range exp.Collections {
		if exp.Collections[i].Slug == "conventions" {
			sawConventions = true
		}
		exp.Collections[i].Traits = ""
	}
	if !sawConventions {
		t.Fatal("control leg failed: export carried no conventions collection to strip")
	}

	imported, err := s.ImportWorkspace(exp, "legacy-archive-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	conv, err := s.GetCollectionBySlug(imported.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("imported workspace has no conventions collection: %v", err)
	}
	traits, err := models.ParseCollectionTraits(conv.Traits)
	if err != nil {
		t.Fatalf("parse imported traits: %v (raw %q)", err, conv.Traits)
	}
	if traits.IsZero() {
		t.Fatal("imported conventions collection declares nothing — a legacy archive imports as an inert workspace")
	}
	if len(traits.BootstrapInclude) != 2 {
		t.Errorf("imported conventions: got %d bootstrap includes, want 2", len(traits.BootstrapInclude))
	}
	if bodies := traits.BootstrapIncludeForKey("conventions"); bodies == nil {
		t.Error("imported conventions declares no bodies include")
	} else if bodies.Filter["status"] != "active" {
		t.Errorf("imported conventions bodies filter = %+v, want status=active", bodies.Filter)
	}

	pb, err := s.GetCollectionBySlug(imported.ID, "playbooks")
	if err != nil || pb == nil {
		t.Fatalf("imported workspace has no playbooks collection: %v", err)
	}
	pbTraits, err := models.ParseCollectionTraits(pb.Traits)
	if err != nil {
		t.Fatalf("parse imported playbook traits: %v", err)
	}
	if pbTraits.InvocationField != models.InvocationSlugField {
		t.Errorf("imported playbooks invocation_field = %q, want %q — invocation routing would be dead", pbTraits.InvocationField, models.InvocationSlugField)
	}

	// Inference must not leak onto a collection that has no canonical set.
	ordinary, err := s.GetCollectionBySlug(imported.ID, "tasks")
	if err != nil || ordinary == nil {
		t.Fatalf("imported workspace has no tasks collection: %v", err)
	}
	if ot, err := models.ParseCollectionTraits(ordinary.Traits); err != nil || !ot.IsZero() {
		t.Errorf("inference leaked onto an ordinary collection: %+v (err %v)", ot, err)
	}
}

// TestImportDoesNotOverrideSurvivingTraits is the other half of the inference
// rule: an archive that DOES carry declarations keeps them verbatim. Inference
// is a compatibility shim for archives written before the column existed, not
// a normalizer — silently rewriting a workspace's own declarations to the
// canonical set on every import would make export/import lossy in a way no
// caller could see.
//
// The custom declarations below differ from the canonical set on purpose; a
// test using the canonical values would pass whether or not the guard exists.
func TestImportDoesNotOverrideSurvivingTraits(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "custom-traits-owner@test.com", "Custom Owner", "password123")
	src := createTestWorkspace(t, s, "Custom Traits Source")
	if err := s.SeedCollectionsFromTemplate(src.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Legal but NOT canonical. It must be legal because import validates
	// before inferring — an invalid declaration is discarded and then
	// replaced by the canonical set, which is correct behaviour but would
	// make this test assert the opposite of its name. (An earlier version
	// used `mode: metadata` on the `conventions` key, which round 7's
	// first-party mode pinning subsequently made invalid; the test caught
	// the interaction.) It must differ from canonical or the assertion
	// cannot tell "kept" from "overwritten".
	custom := models.CollectionTraits{
		BootstrapInclude: []models.BootstrapInclude{
			{Mode: models.BootstrapModeBodies, Filter: map[string]string{"status": "active"}, Key: "conventions"},
		},
	}
	customJSON, err := custom.JSON()
	if err != nil {
		t.Fatalf("marshal custom traits: %v", err)
	}
	// Control leg: the custom set must actually differ from the canonical
	// one, or the assertion below cannot discriminate.
	canonicalJSON, _ := collections.CanonicalTraitsForSlug("conventions").JSON()
	if customJSON == canonicalJSON {
		t.Fatal("control leg failed: the 'custom' traits are identical to the canonical set")
	}

	exp, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	for i := range exp.Collections {
		if exp.Collections[i].Slug == "conventions" {
			exp.Collections[i].Traits = customJSON
		}
	}

	imported, err := s.ImportWorkspace(exp, "custom-traits-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	conv, err := s.GetCollectionBySlug(imported.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("imported workspace has no conventions collection: %v", err)
	}
	got, err := models.ParseCollectionTraits(conv.Traits)
	if err != nil {
		t.Fatalf("parse imported traits: %v", err)
	}
	gotJSON, _ := got.JSON()
	if gotJSON != customJSON {
		t.Errorf("import rewrote the workspace's own declarations\n got: %s\nwant: %s", gotJSON, customJSON)
	}
}

// TestBackfillSQLMatchesCanonicalTraits pins the migration's hardcoded JSON to
// the single Go definition every other surface uses. The SQL cannot import the
// Go constant, so this is the only thing stopping the two from drifting — and
// a drift would be silent, since both sides independently produce valid traits.
func TestBackfillSQLMatchesCanonicalTraits(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Backfill Parity")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, slug := range []string{"conventions", "playbooks"} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			t.Fatalf("no %s collection: %v", slug, err)
		}
		if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = '{}' WHERE id = ?`), coll.ID); err != nil {
			t.Fatalf("reset traits: %v", err)
		}
	}
	runBackfillStatements(t, s)

	for _, slug := range []string{"conventions", "playbooks"} {
		coll, err := s.GetCollectionBySlug(ws.ID, slug)
		if err != nil || coll == nil {
			t.Fatalf("no %s collection: %v", slug, err)
		}
		fromSQL, err := models.ParseCollectionTraits(coll.Traits)
		if err != nil {
			t.Fatalf("parse backfilled %s traits: %v", slug, err)
		}
		want := collections.CanonicalTraitsForSlug(slug)
		gotJSON, _ := fromSQL.JSON()
		wantJSON, _ := want.JSON()
		if gotJSON != wantJSON {
			t.Errorf("%s: migration backfill has drifted from CanonicalTraitsForSlug\n  SQL: %s\n   Go: %s", slug, gotJSON, wantJSON)
		}
	}
}
