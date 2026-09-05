package store

import (
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2710. Two collections in one workspace could declare the same kernel
// trait, and with both live the resolver returned whichever the result order
// put first — arbitrary rather than tie-broken, and measured FLIPPING between
// runs on Postgres. These lock the de-duplication that makes the partial
// unique indexes creatable, and the rule that decides which one survives.

// duplicateTraitFixture reproduces the state from the task body: seed a
// template, rename the conventions collection (which re-slugs it), seed again
// so a fresh `conventions` appears, and both now declare artifact_kind =
// convention. When userWrote is true the user writes a convention into the
// RENAMED collection first, which is the only thing that distinguishes them.
func duplicateTraitFixture(t *testing.T, s *Store, name string, userWrote bool) (ws *models.Workspace, renamedID, dupID string) {
	t.Helper()
	ws = createTestWorkspace(t, s, name)
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("get conventions: %v (nil=%v)", err, conv == nil)
	}
	newName := "House Rules"
	if _, err := s.UpdateCollection(conv.ID, models.CollectionUpdate{Name: &newName}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if userWrote {
		if _, err := s.CreateItem(ws.ID, conv.ID, models.ItemCreate{
			Title: "Our actual house rule", CreatedBy: "user", Source: "web",
		}); err != nil {
			t.Fatalf("user item: %v", err)
		}
	}
	// The duplicate is now UNREPRESENTABLE through ordinary code: the partial
	// unique index refuses it, and SeedCollectionsFromTemplate skips a
	// definition whose kind is already declared rather than colliding with it.
	// That is the fix working. It also means this fixture can only be built
	// the way the state actually exists in the wild — as legacy data written
	// before the index — so the declaration is copied onto a fresh collection
	// with enforcement suspended for that one statement.
	dup, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Conventions", Prefix: "CONV2"})
	if err != nil {
		t.Fatalf("create the would-be reseed: %v", err)
	}
	restore := s.SuspendTraitUniquenessForTesting()
	t.Cleanup(func() {
		// After de-duplication the workspace holds one declaration, so the
		// indexes must be creatable again — which is precisely the precondition
		// the migration needs, asserted here rather than assumed.
		if err := restore(); err != nil {
			t.Errorf("the indexes could not be recreated after de-duplication, so the migration would fail on this state: %v", err)
		}
	})
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), conv.Traits, dup.ID); err != nil {
		t.Fatalf("plant the duplicate declaration: %v", err)
	}
	{
		// The reseed would have populated the duplicate with template rows, and
		// the bare reproduction ties on item count — that tie is what makes it
		// the arbitrary case. Give the planted duplicate the same count in both
		// fixtures, so the ONLY difference between them is the user-written
		// item, which is the thing under test.
		var n int
		if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL`), conv.ID).Scan(&n); err != nil {
			t.Fatalf("count original items: %v", err)
		}
		for i := 0; i < n; i++ {
			if _, err := s.CreateItem(ws.ID, dup.ID, models.ItemCreate{
				Title: "Template rule", CreatedBy: "system", Source: "template",
			}); err != nil {
				t.Fatalf("seed template item into the duplicate: %v", err)
			}
		}
	}
	return ws, conv.ID, dup.ID
}

func declaringConvention(t *testing.T, s *Store, workspaceID string) []string {
	t.Helper()
	traited, err := s.ListTraitedCollections(workspaceID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var out []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind == "convention" {
			out = append(out, tc.Slug)
		}
	}
	return out
}

// TestDedupeKeepsTheCollectionTheUserWroteIn is the fixture that MUST be
// decisive: the renamed collection holds a convention the user wrote, the
// accidental reseed holds only template rows, so the user's collection keeps
// the declaration every time. Measured 8/8 before this shipped; the point of
// the test is that it is not 7/8.
func TestDedupeKeepsTheCollectionTheUserWroteIn(t *testing.T) {
	s := testStore(t)
	ws, _, dupID := duplicateTraitFixture(t, s, "Dedupe User Wrote", true)

	// Control: the fixture really did produce the duplicate state, otherwise
	// the assertion below passes without exercising anything.
	if got := declaringConvention(t, s, ws.ID); len(got) != 2 {
		t.Fatalf("control leg failed: %d collections declare convention, want 2 (%v)", len(got), got)
	}

	if err := s.dedupeTraitDeclarations(); err != nil {
		t.Fatalf("dedupeTraitDeclarations: %v", err)
	}

	got := declaringConvention(t, s, ws.ID)
	if len(got) != 1 {
		t.Fatalf("after de-dup %d collections declare convention, want 1 (%v)", len(got), got)
	}
	if got[0] != "house-rules" {
		t.Errorf("declaration survived on %q, want house-rules — the collection the user actually wrote in", got[0])
	}

	// The loser keeps every item; it loses only the declaration.
	var loserItems int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL`), dupID).Scan(&loserItems); err != nil {
		t.Fatalf("count loser items: %v", err)
	}
	if loserItems == 0 {
		t.Error("the losing collection lost its items; the ruling is that it keeps them and loses only the trait")
	}
}

// TestDedupeReportsAnArbitraryTieAsArbitrary is the bare reproduction: nobody
// wrote anything, so both collections hold four identical template rows and
// tie on every orderable key. No rule can prefer either — measured as a 4/4
// coin flip — so the requirement is not WHICH one survives but that exactly
// one does and that the log says the choice was arbitrary rather than dressing
// it up as age.
func TestDedupeReportsAnArbitraryTieAsArbitrary(t *testing.T) {
	s := testStore(t)
	ws, _, _ := duplicateTraitFixture(t, s, "Dedupe Arbitrary Tie", false)
	if got := declaringConvention(t, s, ws.ID); len(got) != 2 {
		t.Fatalf("control leg failed: %d collections declare convention, want 2 (%v)", len(got), got)
	}

	var captured []slog.Record
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&recordCapturingHandler{records: &captured}))
	err := s.dedupeTraitDeclarations()
	slog.SetDefault(prev)
	if err != nil {
		t.Fatalf("dedupeTraitDeclarations: %v", err)
	}

	if got := declaringConvention(t, s, ws.ID); len(got) != 1 {
		t.Fatalf("after de-dup %d collections declare convention, want exactly 1 (%v)", len(got), got)
	}

	var sawArbitrary, sawWinner, sawLoser bool
	for _, r := range captured {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, "trait de-duplication") {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "decided_by":
				if strings.Contains(a.Value.String(), "arbitrary") &&
					strings.Contains(a.Value.String(), "NOT an age") {
					sawArbitrary = true
				}
			case "winner":
				sawWinner = true
			case "loser":
				sawLoser = true
			}
			return true
		})
	}
	if !sawArbitrary {
		t.Error("the tie was not reported as arbitrary; an operator reading this log cannot tell a considered resolution from a coin flip")
	}
	if !sawWinner || !sawLoser {
		t.Error("the report does not name both the winner and the loser")
	}
}

// TestTraitUniquenessIsEnforcedByTheDatabase locks the half the indexes own:
// once the duplicates are gone, a second declaration cannot be written at all.
// Raw SQL on purpose — the API gate refuses this too, so going through it
// would assert that the gate works rather than that the constraint does.
func TestTraitUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Trait Uniqueness")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conv, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || conv == nil {
		t.Fatalf("get conventions: %v", err)
	}

	// Control: a collection with NO declaration inserts fine through the same
	// statement shape, so a failure below is the constraint and not the SQL.
	plain, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Plain", Prefix: "PLN"})
	if err != nil {
		t.Fatalf("control leg failed: a trait-free collection was rejected: %v", err)
	}

	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), conv.Traits, plain.ID); err == nil {
		t.Fatal("a second collection took the convention declaration; the unique index is not in force")
	} else {
		t.Logf("second declaration refused: %v", err)
	}
}

// TestDedupeStripsEveryDeclarationALoserHolds covers the case a collection
// loses BOTH declarations, which the single-declaration fixtures above cannot
// reach. The playbooks definition declares artifact_kind AND invocation_field,
// so a duplicated playbooks collection competes on two fronts at once.
//
// The defect this pins: resolving each declaration in its own pass, with each
// pass re-parsing the row's ORIGINAL traits, made the second write restore what
// the first had stripped. The duplicate survived and the migration would still
// have failed on it — which is precisely the failure the de-dup pass exists to
// prevent, so it would have surfaced as a broken upgrade rather than a test.
func TestDedupeStripsEveryDeclarationALoserHolds(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Dedupe Both Declarations")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	play, err := s.GetCollectionBySlug(ws.ID, "playbooks")
	if err != nil || play == nil {
		t.Fatalf("get playbooks: %v (nil=%v)", err, play == nil)
	}
	// Control: the seeded definition really does declare both, otherwise this
	// test is a duplicate of the single-declaration ones.
	seeded, perr := models.ParseCollectionTraits(play.Traits)
	if perr != nil {
		t.Fatalf("parse seeded traits: %v", perr)
	}
	if seeded.ArtifactKind == nil || seeded.ArtifactKind.Kind == "" || seeded.InvocationField == "" {
		t.Fatalf("control leg failed: playbooks declares kind=%v field=%q, want both", seeded.ArtifactKind, seeded.InvocationField)
	}

	dup, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Old Playbooks", Prefix: "OPLAY"})
	if err != nil {
		t.Fatalf("create duplicate: %v", err)
	}
	restore := s.SuspendTraitUniquenessForTesting()
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Errorf("indexes not recreatable after de-dup, so the migration would fail here: %v", err)
		}
	})
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), play.Traits, dup.ID); err != nil {
		t.Fatalf("plant the double declaration: %v", err)
	}
	// The loser must be the planted one: the seeded playbooks holds the
	// template's items and the plant holds none, so "most user-written items"
	// ties at zero and the terminator decides. Give the seeded one a
	// user-written item so the outcome is determined rather than a coin flip —
	// this test is about stripping both declarations, not about the tie-break.
	if _, err := s.CreateItem(ws.ID, play.ID, models.ItemCreate{
		Title: "A real playbook", CreatedBy: "user", Source: "web",
	}); err != nil {
		t.Fatalf("user item: %v", err)
	}

	if err := s.dedupeTraitDeclarations(); err != nil {
		t.Fatalf("dedupeTraitDeclarations: %v", err)
	}

	var raw string
	if err := s.db.QueryRow(s.q(`SELECT traits FROM collections WHERE id = ?`), dup.ID).Scan(&raw); err != nil {
		t.Fatalf("read loser traits: %v", err)
	}
	got, perr := models.ParseCollectionTraits(raw)
	if perr != nil {
		t.Fatalf("parse loser traits %q: %v", raw, perr)
	}
	if got.ArtifactKind != nil && got.ArtifactKind.Kind != "" {
		t.Errorf("the loser kept its artifact_kind (%q); both declarations should be gone", got.ArtifactKind.Kind)
	}
	if got.InvocationField != "" {
		t.Errorf("the loser kept its invocation_field (%q); both declarations should be gone", got.InvocationField)
	}
}

// TestMalformedTraitsDoNotBreakTheInvariant covers the row nobody writes on
// purpose: a collections row whose traits blob is not valid JSON.
//
// SQLite's json_extract RAISES on malformed JSON rather than returning NULL,
// so an unguarded expression in either the index predicate or the de-dup scan
// would fail STARTUP — not a request, startup — on a database holding one bad
// blob. Every other reader treats malformed traits as declaring nothing, and
// the guard makes these two agree with that rather than being fatal.
func TestMalformedTraitsDoNotBreakTheInvariant(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Malformed Traits")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	junk, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Junk", Prefix: "JUNK"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Written raw: every door validates traits, so this is the shape of a row
	// that arrived some other way — a hand-edited database, or a write from a
	// build that predates validation.
	//
	// THE TWO DRIVERS DIFFER HERE, and that difference is the point rather
	// than an inconvenience. On Postgres traits is JSONB, so the column type
	// REFUSES malformed content and the state is unrepresentable at rest —
	// which is exactly why migration 064 carries no json_valid guard while 087
	// does. That claim was prose in the migration until this test found the
	// refusal; asserting it here makes it a checked fact, and turns this into
	// the test that would catch someone "fixing" the asymmetry by adding a
	// guard Postgres does not need, or removing the one SQLite does.
	_, plantErr := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), `{"artifact_kind":`, junk.ID)
	if s.dialect.Driver() == DriverPostgres {
		if plantErr == nil {
			t.Fatal("Postgres accepted a malformed traits blob; the JSONB column type is what makes migration 064's missing json_valid guard correct, and it just stopped being true")
		}
		t.Logf("Postgres refused the malformed blob, as the JSONB column requires: %v", plantErr)
		return
	}
	if plantErr != nil {
		t.Fatalf("plant malformed traits: %v", plantErr)
	}

	// The de-dup pass must survive it — this is what runs before migrate().
	if err := s.dedupeTraitDeclarations(); err != nil {
		t.Fatalf("de-dup failed on a malformed traits blob; startup would fail here: %v", err)
	}

	// And the indexes must still be creatable, which is the migration itself.
	restore := s.SuspendTraitUniquenessForTesting()
	if err := restore(); err != nil {
		t.Fatalf("the indexes could not be created against a malformed traits blob; the migration would fail here: %v", err)
	}

	// The malformed row is outside the invariant, not inside it: a live
	// collection can still take a declaration the junk row appears to hold.
	var stored string
	if err := s.db.QueryRow(s.q(`SELECT traits FROM collections WHERE id = ?`), junk.ID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != `{"artifact_kind":` {
		t.Errorf("the malformed blob was rewritten to %q; this unit does not repair traits, it only removes duplicate declarations", stored)
	}
}

// TestImportDeduplicatesAConflictingArchive covers TASK-2710 item 4. Before
// the indexes, import warned about a duplicate declaration and inserted both.
// With them the second INSERT is refused, the whole transaction rolls back and
// the workspace minted beforehand survives as a husk — so an archive carrying
// a duplicate would become unimportable, and those archives are exactly the
// ones this release exists to repair.
func TestImportDeduplicatesAConflictingArchive(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "importdedupe@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Import Dedupe Source")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// The conflicting archive: a second collection carrying the conventions
	// declaration. Built in the BUNDLE rather than the database, which is how
	// it actually arrives — a hand-edited or foreign archive.
	var convTraits string
	for _, c := range exp.Collections {
		if c.Slug == "conventions" {
			convTraits = c.Traits
		}
	}
	if convTraits == "" {
		t.Fatal("control leg failed: the exported bundle carries no conventions declaration to duplicate")
	}
	dupe := exp.Collections[0]
	dupe.ID = dupe.ID + "-dupe"
	dupe.Slug = "old-conventions"
	dupe.Name = "Old Conventions"
	dupe.Prefix = "OCONV"
	dupe.Traits = convTraits
	exp.Collections = append(exp.Collections, dupe)

	imported, err := s.ImportWorkspace(exp, "import-dedupe-target", owner.ID)
	if err != nil {
		t.Fatalf("an archive carrying a duplicate declaration failed to import: %v", err)
	}

	// Exactly one collection declares it, and the LATER one lost it.
	traited, err := s.ListTraitedCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var declaring []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind == "convention" {
			declaring = append(declaring, tc.Slug)
		}
	}
	if len(declaring) != 1 {
		t.Fatalf("collections declaring convention after import = %v, want exactly 1", declaring)
	}
	if declaring[0] != "conventions" {
		t.Errorf("declaration landed on %q; bundle order gives it to the first, which is conventions", declaring[0])
	}

	// The stripped collection still exists — it lost the declaration, not its
	// place in the workspace.
	if got, err := s.GetCollectionBySlug(imported.ID, "old-conventions"); err != nil || got == nil {
		t.Errorf("the stripped collection is missing from the import (err=%v); it should keep everything but the declaration", err)
	}
}

// TestImportDeduplicatesAfterCanonicalInference covers the ordering trap in the
// import de-duplication: inference runs AFTER it.
//
// A pre-traits archive carries `conventions` with an EMPTY traits blob, and
// import infers the canonical declaration for it from its slug (BUG-2702's
// fix — without that, restoring an old archive comes up inert). If the same
// bundle also carries another collection that explicitly declares
// artifact_kind=convention, the de-dup pass sees only ONE declaration and
// leaves both alone; inference then adds the second, and the unique index
// aborts the whole import.
//
// So the pass has to run on the traits each collection will ACTUALLY be
// inserted with, not on the ones the bundle carries.
func TestImportDeduplicatesAfterCanonicalInference(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "inferdedupe@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Infer Dedupe Source")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var convTraits string
	for i := range exp.Collections {
		if exp.Collections[i].Slug == "conventions" {
			convTraits = exp.Collections[i].Traits
			// The pre-traits archive shape: the key is absent, which decodes
			// to "". Inference is what gives it back its declaration.
			exp.Collections[i].Traits = ""
		}
	}
	if convTraits == "" {
		t.Fatal("control leg failed: the export carries no conventions declaration to work from")
	}

	// A second collection that DOES declare it explicitly.
	rival := exp.Collections[0]
	rival.ID = rival.ID + "-rival"
	rival.Slug = "house-rules"
	rival.Name = "House Rules"
	rival.Prefix = "HRULE"
	rival.Traits = convTraits
	exp.Collections = append(exp.Collections, rival)

	imported, err := s.ImportWorkspace(exp, "infer-dedupe-target", owner.ID)
	if err != nil {
		t.Fatalf("import failed on an archive whose duplicate only appears after inference: %v", err)
	}

	traited, err := s.ListTraitedCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var declaring []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind == "convention" {
			declaring = append(declaring, tc.Slug)
		}
	}
	if len(declaring) != 1 {
		t.Errorf("collections declaring convention after import = %v, want exactly 1", declaring)
	}
}

// TestImportKeepsTheLiveDeclarationWhenAnArchivedCollectionSharesIt is the
// rebase test for this branch against BUG-2884 (checkpoint 6 on TASK-2710).
//
// BUG-2884 made the bundle CARRY soft-deleted collections, so an archived
// collection can now travel alongside the live one that replaced it, still
// declaring the same artifact kind. Import's de-duplication runs in bundle
// order and has no notion of liveness, so the archived collection claims the
// kind and the LIVE one is stripped of it. The workspace then imports with its
// convention routing owned by a row every resolver filters out
// (ListTraitedCollections is deleted_at IS NULL) — the workspace comes up
// inert, which is the failure BUG-2884's own pre-pass skip existed to prevent
// and which this branch's relocation of the check into the insert loop
// reintroduced.
//
// The archived collection must also KEEP its declaration. Nothing routes to it,
// the partial unique indexes exclude it (`AND deleted_at IS NULL`), and
// stripping it would edit data the operator archived rather than deleted.
func TestImportKeepsTheLiveDeclarationWhenAnArchivedCollectionSharesIt(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "archivedtrait@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Archived Trait Source")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var convTraits string
	liveIndex := -1
	for i, c := range exp.Collections {
		if c.Slug == "conventions" {
			convTraits = c.Traits
			liveIndex = i
		}
	}
	if convTraits == "" || liveIndex < 0 {
		t.Fatal("control leg failed: the exported bundle carries no live conventions declaration to contend with")
	}

	// The archived predecessor, placed FIRST in bundle order — which is what
	// makes it the claimant under a de-duplication that only counts order.
	// Built in the bundle rather than the database because that is how it
	// arrives: BUG-2884 put soft-deleted collections into the archive.
	archived := exp.Collections[liveIndex]
	archived.ID = archived.ID + "-archived"
	archived.Slug = "old-conventions"
	archived.Name = "Old Conventions"
	archived.Prefix = "OCONV"
	archived.Traits = convTraits
	archived.DeletedAt = "2026-01-02T03:04:05Z"
	exp.Collections = append([]models.CollectionExport{archived}, exp.Collections...)

	imported, err := s.ImportWorkspace(exp, "archived-trait-target", owner.ID)
	if err != nil {
		t.Fatalf("import failed with an archived collection sharing a declaration: %v", err)
	}

	// The live collection answers for the kind. This is the assertion a naive
	// rebase fails: the archived row claims `convention` first and the live
	// conventions collection is stripped, so nothing resolves.
	traited, err := s.ListTraitedCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var declaring []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind == "convention" {
			declaring = append(declaring, tc.Slug)
		}
	}
	if len(declaring) != 1 || declaring[0] != "conventions" {
		t.Fatalf("live collections declaring convention = %v, want exactly [conventions]; an archived collection took the declaration and left the workspace with no live convention routing", declaring)
	}

	// And the archived one still holds its own. Read straight from the table:
	// every store-level reader filters it out, which is the whole reason it is
	// harmless for it to keep the declaration.
	var archivedTraits string
	if err := s.db.QueryRow(s.q(`
		SELECT traits FROM collections
		WHERE workspace_id = ? AND slug = ? AND deleted_at IS NOT NULL`),
		imported.ID, "old-conventions").Scan(&archivedTraits); err != nil {
		t.Fatalf("read back the archived collection: %v", err)
	}
	parsed, perr := models.ParseCollectionTraits(archivedTraits)
	if perr != nil {
		t.Fatalf("archived traits did not parse: %v", perr)
	}
	if parsed.ArtifactKind == nil || parsed.ArtifactKind.Kind != "convention" {
		t.Errorf("the archived collection was stripped to %q; nothing routes to it and the indexes exclude it, so import has no business editing data the operator archived", archivedTraits)
	}
}

// TestTheSnapshotIsTakenBeforeTheRepairWrites pins the ORDER of two startup
// steps, which is the whole of what it asserts (codex round 5, P2).
//
// The repair changes data — it strips a declaration, moving which collection
// owns a kernel behavior — and `<db>.pre-<version>` is the operator's rollback
// for a bad upgrade. Calling the repair from the constructor, ahead of
// migrate(), put the altered ownership INSIDE the snapshot: restoring after a
// failed migration handed back the old schema with the repair already applied
// and no record of it, so the one thing the rollback could not undo was the
// only thing that had silently changed routing.
//
// SQLite only. Postgres takes no snapshot — backups there are the operator's
// pg_dump/PITR job — so on that dialect there is no ordering to pin.
func TestTheSnapshotIsTakenBeforeTheRepairWrites(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("no pre-migration snapshot on Postgres; the ordering this pins does not exist there")
	}
	dbPath := s.dbPath
	if dbPath == "" {
		t.Fatal("control leg failed: the test store has no file path, so no snapshot could be written either way")
	}

	ws := createTestWorkspace(t, s, "Snapshot Order")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	convs, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || convs == nil {
		t.Fatalf("GetCollectionBySlug(conventions): %v (nil=%v)", err, convs == nil)
	}
	rival, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "House Rules", Prefix: "HOUSE"})
	if err != nil {
		t.Fatalf("create rival: %v", err)
	}

	// The legacy state: two live collections declaring one kind, with the
	// migration that forbids it not yet applied. Both halves are needed —
	// without the pending migration no snapshot is taken at all, and the test
	// would pass by measuring nothing.
	restore := s.SuspendTraitUniquenessForTesting()
	_ = restore // deliberately not called: the duplicate must survive the reopen
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), convs.Traits, rival.ID); err != nil {
		t.Fatalf("plant the duplicate: %v", err)
	}
	const migration = "087_collection_trait_uniqueness.sql"
	res, err := s.db.Exec(s.q(`DELETE FROM schema_migrations WHERE version = ?`), migration)
	if err != nil {
		t.Fatalf("un-apply %s: %v", migration, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control leg failed: %s was not in schema_migrations (rows=%d), so nothing would be pending and no snapshot taken", migration, n)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	snapshot := dbPath + ".pre-" + sanitizeVersion(BinaryVersion)
	if _, err := os.Stat(snapshot); err == nil {
		t.Fatalf("control leg failed: a snapshot at %s already existed before the reopen, and snapshotBeforeMigrate preserves the first one", snapshot)
	}

	reopened, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen (this is the upgrade the repair runs during): %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	// The live database is repaired: exactly one live collection declares it.
	if got := countDeclaringConvention(t, reopened.db, reopened.q, ws.ID); got != 1 {
		t.Fatalf("collections declaring convention after the upgrade = %d, want 1; the repair did not run", got)
	}

	// And the snapshot still holds the state the operator would roll back TO.
	// Opened with the raw driver, not New(): New() would migrate and repair the
	// snapshot too, which would destroy the very thing being measured.
	snap, err := sql.Open("sqlite", snapshot)
	if err != nil {
		t.Fatalf("open snapshot %s: %v", snapshot, err)
	}
	defer snap.Close()
	if got := countDeclaringConvention(t, snap, func(q string) string { return q }, ws.ID); got != 2 {
		t.Errorf("collections declaring convention in the snapshot = %d, want 2; the repair committed BEFORE the snapshot, so restoring it cannot undo the routing change", got)
	}
}

// countDeclaringConvention counts LIVE collections in a workspace whose traits
// declare artifact_kind=convention, reading the same way the index does.
func countDeclaringConvention(t *testing.T, db *sql.DB, q func(string) string, workspaceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q(`
		SELECT COUNT(*) FROM collections
		WHERE workspace_id = ?
		  AND deleted_at IS NULL
		  AND json_valid(traits)
		  AND json_extract(traits, '$.artifact_kind.kind') = 'convention'`), workspaceID).Scan(&n); err != nil {
		t.Fatalf("count declaring collections: %v", err)
	}
	return n
}
