package store

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2884. DeleteCollection soft-deletes the collection row only; its items
// keep deleted_at IS NULL and stay fully reachable (GetItemByRef, GetItem,
// SearchItems all return them — no item-bearing read in this package filters
// joined collection liveness). Export dropped the collection and kept the
// items, so the bundle carried items naming a collection it did not contain,
// and ImportWorkspace's orphan gate discarded them without a log line.
//
// pad db migrate is ExportWorkspace piped into ImportWorkspace
// (cmd/pad/cmd_db.go:450,459), so this is the SQLite→Postgres migration path
// losing live rows, not only a backup being lossy.

// archivedCollectionFixture builds a workspace holding one live collection
// with an item, and one collection that is soft-deleted AFTER an item was
// created in it. Returns the workspace and the archived collection's item.
func archivedCollectionFixture(t *testing.T, s *Store, name string) (*models.Workspace, *models.Collection, *models.Item) {
	t.Helper()
	ws := createTestWorkspace(t, s, name)

	live, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Live", Prefix: "LIVE"})
	if err != nil {
		t.Fatalf("CreateCollection(live): %v", err)
	}
	if _, err := s.CreateItem(ws.ID, live.ID, models.ItemCreate{Title: "Live item"}); err != nil {
		t.Fatalf("CreateItem(live): %v", err)
	}

	archived, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Archived", Prefix: "ARCH"})
	if err != nil {
		t.Fatalf("CreateCollection(archived): %v", err)
	}
	it, err := s.CreateItem(ws.ID, archived.ID, models.ItemCreate{
		Title:   "Survivor",
		Content: "body that must round-trip",
	})
	if err != nil {
		t.Fatalf("CreateItem(archived): %v", err)
	}
	if err := s.DeleteCollection(archived.ID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
	return ws, archived, it
}

// TestExportCarriesSoftDeletedCollections locks the bundle SHAPE: a
// soft-deleted collection is exported, carrying its deleted_at, so the items
// section is no longer filtered by a different rule than the collections
// section.
func TestExportCarriesSoftDeletedCollections(t *testing.T) {
	s := testStore(t)
	ws, archived, it := archivedCollectionFixture(t, s, "Export Shape 2884")

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	var gotArchived, gotLive *models.CollectionExport
	for i := range exp.Collections {
		switch exp.Collections[i].ID {
		case archived.ID:
			gotArchived = &exp.Collections[i]
		default:
			if exp.Collections[i].Slug == "live" {
				gotLive = &exp.Collections[i]
			}
		}
	}
	if gotArchived == nil {
		t.Fatal("export dropped the soft-deleted collection; its items are exported, so the bundle names a collection it does not carry")
	}
	if gotArchived.DeletedAt == "" {
		t.Error("soft-deleted collection exported with an empty deleted_at — the importer cannot tell it was archived")
	}
	if gotLive == nil {
		t.Fatal("control leg failed: export dropped the LIVE collection too")
	}
	if gotLive.DeletedAt != "" {
		t.Errorf("live collection exported with deleted_at %q, want empty", gotLive.DeletedAt)
	}

	// Every exported item's collection must be present in the same bundle.
	inBundle := map[string]bool{}
	for _, c := range exp.Collections {
		inBundle[c.ID] = true
	}
	var sawSurvivor bool
	for _, i := range exp.Items {
		if !inBundle[i.CollectionID] {
			t.Errorf("item %q names collection %s, absent from the bundle", i.Title, i.CollectionID)
		}
		if i.ID == it.ID {
			sawSurvivor = true
		}
	}
	if !sawSurvivor {
		t.Fatal("control leg failed: the archived collection's item was not exported at all")
	}
}

// TestRoundTripPreservesItemsUnderSoftDeletedCollection is the headline: the
// item is live in the source (reachable by ref) and must be live in the
// target, under a collection that is still archived there.
func TestRoundTripPreservesItemsUnderSoftDeletedCollection(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "roundtrip2884@test.com", "Owner", "password123")
	ws, _, it := archivedCollectionFixture(t, s, "Round Trip 2884")

	// Control: the item is live and ref-reachable in the SOURCE. If this ever
	// stops holding, the premise of the whole fix is gone and the test says so
	// here rather than by silently passing below.
	if src, err := s.GetItemByRef(ws.ID, "ARCH", *it.ItemNumber); err != nil || src == nil {
		t.Fatalf("control leg failed: item under a soft-deleted collection is not reachable in the source (item=%v err=%v)", src != nil, err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	imported, err := s.ImportWorkspace(exp, "round-trip-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	items, err := s.ListItems(imported.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	titles := make([]string, 0, len(items))
	for _, i := range items {
		titles = append(titles, i.Title)
	}
	var survivor *models.Item
	for i := range items {
		if items[i].Title == "Survivor" {
			survivor = &items[i]
		}
	}
	if survivor == nil {
		t.Fatalf("the archived collection's item vanished on import; imported items = %v", titles)
	}
	if survivor.Content != "body that must round-trip" {
		t.Errorf("survivor content = %q, want the source body", survivor.Content)
	}

	// The collection came back, and came back ARCHIVED — importing it live
	// would resurrect a collection the user deleted.
	coll, err := s.GetCollectionAnyState(survivor.CollectionID)
	if err != nil {
		t.Fatalf("GetCollectionAnyState: %v", err)
	}
	if coll == nil {
		t.Fatal("imported item names a collection row that does not exist")
	}
	if coll.DeletedAt == nil || coll.DeletedAt.IsZero() {
		t.Error("the archived collection imported LIVE — a deleted collection reappears in the target's collection list")
	}
	// ...and it is absent from the live listing, exactly as in the source.
	live, err := s.ListCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for _, c := range live {
		if c.Slug == coll.Slug {
			t.Errorf("archived collection %q appears in the imported workspace's live collection list", c.Slug)
		}
	}
}

// TestRoundTripPreservesDependentsUnderSoftDeletedCollection covers the four
// sections that inherit the item set. Losing the item lost all of them, each
// for a different reason (comments/versions/reminders resolve through the item
// map), so they are asserted together rather than trusted to follow.
func TestRoundTripPreservesDependentsUnderSoftDeletedCollection(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "deps2884@test.com", "Owner", "password123")
	ws, _, it := archivedCollectionFixture(t, s, "Dependents 2884")

	if _, err := s.CreateComment(ws.ID, it.ID, "", models.CommentCreate{Author: "Rook", Body: "comment on an archived-collection item"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	newBody := "edited, which mints a version"
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Content: &newBody}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	remindAt := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	if _, err := s.CreateReminder(ws.ID, it.ID, remindAt); err != nil {
		t.Fatalf("CreateReminder: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(exp.Comments) == 0 || len(exp.ItemVersions) == 0 || len(exp.Reminders) == 0 {
		t.Fatalf("control leg failed: export carried comments=%d versions=%d reminders=%d, want all non-zero",
			len(exp.Comments), len(exp.ItemVersions), len(exp.Reminders))
	}

	imported, err := s.ImportWorkspace(exp, "dependents-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(imported.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var survivorID string
	for _, i := range items {
		if i.Title == "Survivor" {
			survivorID = i.ID
		}
	}
	if survivorID == "" {
		t.Fatal("the archived collection's item vanished on import; its dependents cannot be checked")
	}

	comments, err := s.ListComments(survivorID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(comments) != 1 {
		t.Errorf("imported comments = %d, want 1", len(comments))
	}
	reminders, err := s.ListRemindersForItem(imported.ID, survivorID)
	if err != nil {
		t.Fatalf("ListRemindersForItem: %v", err)
	}
	if len(reminders) != 1 {
		t.Errorf("imported reminders = %d, want 1", len(reminders))
	}
	versions, err := s.ListItemVersions(survivorID)
	if err != nil {
		t.Fatalf("ListItemVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Error("imported item carries no version history; the source had some")
	}
}

// TestImportRoutingIgnoresSoftDeletedCollections guards the seam this fix
// OPENS. Trait routing is live-only (ListTraitedCollections filters
// deleted_at IS NULL), so an archived collection now travelling in the bundle
// must not participate in the import's duplicate-declaration scan, and must
// not be mistaken for the workspace's conventions collection.
func TestImportRoutingIgnoresSoftDeletedCollections(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "routing2884@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Routing 2884")
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// An archived collection carrying the conventions declaration, alongside
	// the live seeded conventions collection.
	convs, err := s.GetCollectionBySlug(ws.ID, "conventions")
	if err != nil || convs == nil {
		t.Fatalf("GetCollectionBySlug(conventions): %v (nil=%v)", err, convs == nil)
	}
	ghost, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Old Conventions",
		Prefix: "OCONV",
		Traits: convs.Traits,
	})
	if err != nil {
		t.Fatalf("CreateCollection(ghost): %v", err)
	}
	if err := s.DeleteCollection(ghost.ID, ""); err != nil {
		t.Fatalf("DeleteCollection(ghost): %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	var captured []slog.Record
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(&recordCapturingHandler{records: &captured}))

	imported, err := s.ImportWorkspace(exp, "routing-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	slog.SetDefault(prev)

	// DISCRIMINATION. The two assertions below are the ones that die when the
	// scan skip and the inference skip are reverted; the ListTraitedCollections
	// check further down does NOT discriminate them, because that query filters
	// deleted rows anyway and would pass either way. Kept as the end-to-end leg,
	// labelled so nobody reads it as coverage for the skips.
	//
	// (1) The duplicate-declaration scan must not fire on the archived
	// collection: it declares the same artifact kind as the live conventions
	// collection, and without the skip the import warns about an ambiguity
	// that cannot exist — routing is live-only.
	for _, r := range captured {
		if r.Level != slog.LevelWarn {
			continue
		}
		if strings.Contains(r.Message, "declares one artifact kind on two collections") ||
			strings.Contains(r.Message, "declares invocation routing on two collections") {
			var colls string
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "collections" {
					colls = a.Value.String()
				}
				return true
			})
			t.Errorf("import warned about a duplicate declaration that only exists because an ARCHIVED collection joined the scan: %q (collections=%s)", r.Message, colls)
		}
	}

	traited, err := s.ListTraitedCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListTraitedCollections: %v", err)
	}
	var declaring []string
	for _, tc := range traited {
		if tc.Traits.ArtifactKind != nil && tc.Traits.ArtifactKind.Kind != "" {
			declaring = append(declaring, tc.Slug+":"+tc.Traits.ArtifactKind.Kind)
		}
	}
	for _, d := range declaring {
		if strings.HasPrefix(d, "old-conventions:") {
			t.Errorf("the archived collection is routing in the imported workspace: %v", declaring)
		}
	}
}

// TestLegacyBundleWithoutDeletedAtImportsLive is a COMPATIBILITY LOCK, not a
// regression instrument: it passes before and after this change. A bundle
// written before deleted_at existed on CollectionExport decodes it as "", and
// "" must mean live — the absent→live direction is what keeps every archive
// ever written importable.
func TestLegacyBundleWithoutDeletedAtImportsLive(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "legacy2884@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Legacy 2884")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Widgets", Prefix: "WID"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Legacy item"}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	for i := range exp.Collections {
		exp.Collections[i].DeletedAt = "" // as a pre-BUG-2884 archive decodes
	}

	imported, err := s.ImportWorkspace(exp, "legacy-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	live, err := s.ListCollections(imported.ID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var found bool
	for _, c := range live {
		if c.Slug == "widgets" {
			found = true
		}
	}
	if !found {
		t.Errorf("a legacy bundle's collection did not import LIVE; live collections = %d", len(live))
	}
}

// TestImportSurvivesOrphanedItemDependents covers a SECOND defect, found by
// the dependents test above while the export half was still unfixed, and
// widened by the Postgres gate: a dependent row hanging off an ORPHANED item —
// one whose collection the bundle does not carry — took the ENTIRE workspace
// import down.
//
// Two mechanisms, and the second is why the SQLite suite was not enough.
//
//  1. THE GUARD CANNOT FIRE. Every dependent loop tested
//     `itemMap[...] == ""`, and itemMap is populated for EVERY item before the
//     orphan skip — deliberately, because parent resolution reads it for items
//     the loop has not reached. So the INSERT went ahead with an id naming no
//     row and the foreign key refused it.
//
//  2. "SKIP ON ERROR" IS NOT A SKIP ON POSTGRES. The links and versions loops
//     answered the FK failure by continuing, which works on SQLite. On
//     Postgres a failed statement aborts the surrounding transaction, so every
//     later statement fails and COMMIT reports `commit unexpectedly resulted in
//     rollback`. Catching the error cannot recover it; the row has to be
//     refused BEFORE the database sees it. That is exactly what the reminder
//     loop's insertedItems check already did, and it is now what all four do.
//
// One dependent kind per subtest, deliberately: a fixture carrying all three
// would fail whichever loop is checked first and prove nothing about the other
// two.
//
// Fixing the export half removes pad's own route here — a bundle this build
// writes has no orphans — but not a hand-edited, foreign, or pre-fix archive,
// which is exactly the population that needs to import.
func TestImportSurvivesOrphanedItemDependents(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed hangs exactly ONE kind of dependent row off the orphaned item.
		seed func(t *testing.T, s *Store, ws *models.Workspace, orphan, live *models.Item)
		// present asserts the bundle actually carries it, so a green cannot
		// come from the fixture never reaching the loop under test.
		present func(exp *models.WorkspaceExport, orphanID string) bool
	}{
		{
			name: "comment",
			seed: func(t *testing.T, s *Store, ws *models.Workspace, orphan, live *models.Item) {
				if _, err := s.CreateComment(ws.ID, orphan.ID, "", models.CommentCreate{Author: "Rook", Body: "on an orphan"}); err != nil {
					t.Fatalf("CreateComment: %v", err)
				}
			},
			present: func(exp *models.WorkspaceExport, orphanID string) bool {
				for _, cm := range exp.Comments {
					if cm.ItemID == orphanID {
						return true
					}
				}
				return false
			},
		},
		{
			name: "version",
			seed: func(t *testing.T, s *Store, ws *models.Workspace, orphan, live *models.Item) {
				body := "edited, which mints a version"
				if _, err := s.UpdateItem(orphan.ID, models.ItemUpdate{Content: &body}); err != nil {
					t.Fatalf("UpdateItem: %v", err)
				}
			},
			present: func(exp *models.WorkspaceExport, orphanID string) bool {
				for _, v := range exp.ItemVersions {
					if v.ItemID == orphanID {
						return true
					}
				}
				return false
			},
		},
		{
			name: "link",
			seed: func(t *testing.T, s *Store, ws *models.Workspace, orphan, live *models.Item) {
				if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: orphan.ID, LinkType: "blocks"}, live.ID); err != nil {
					t.Fatalf("CreateItemLink: %v", err)
				}
			},
			present: func(exp *models.WorkspaceExport, orphanID string) bool {
				for _, lk := range exp.ItemLinks {
					if lk.TargetID == orphanID || lk.SourceID == orphanID {
						return true
					}
				}
				return false
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			owner := createTestUser(t, s, "orphan-"+tc.name+"-2884@test.com", "Owner", "password123")
			ws, archived, orphan := archivedCollectionFixture(t, s, "Orphan "+tc.name+" 2884")

			var live *models.Item
			items, err := s.ListItems(ws.ID, models.ItemListParams{})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			for i := range items {
				if items[i].Title == "Live item" {
					live = &items[i]
				}
			}
			if live == nil {
				t.Fatal("control leg failed: the fixture has no live item to link from")
			}
			tc.seed(t, s, ws, orphan, live)

			exp, err := s.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatalf("ExportWorkspace: %v", err)
			}

			// Hand-edit the bundle into the pre-fix shape: the item stays, its
			// collection is removed. This is what every archive written before
			// this change looks like, and what a foreign bundle may look like
			// at any time.
			kept := exp.Collections[:0]
			for _, c := range exp.Collections {
				if c.ID != archived.ID {
					kept = append(kept, c)
				}
			}
			if len(kept) == len(exp.Collections) {
				t.Fatal("control leg failed: the archived collection was not in the bundle to remove")
			}
			exp.Collections = kept

			// ISOLATE. Creating and editing an item mints rows of its own, so
			// a bundle assembled for the "comment" case also carries a version
			// for the orphan — and then the fixture would fail at whichever
			// loop runs first no matter which loop is broken. Strip every
			// orphan-attached dependent except the one under test, so each
			// subtest can fail for exactly one reason.
			exp.Comments = keepUnless(exp.Comments, tc.name == "comment", func(cm models.CommentExport) bool { return cm.ItemID == orphan.ID })
			exp.ItemVersions = keepUnless(exp.ItemVersions, tc.name == "version", func(v models.ItemVersionExport) bool { return v.ItemID == orphan.ID })
			exp.ItemLinks = keepUnless(exp.ItemLinks, tc.name == "link", func(lk models.ItemLinkExport) bool {
				return lk.SourceID == orphan.ID || lk.TargetID == orphan.ID
			})

			if !tc.present(exp, orphan.ID) {
				t.Fatalf("control leg failed: the bundle carries no %s on the orphaned item, so the loop under test is never reached", tc.name)
			}
			// ...and nothing else attached to the orphan survived the strip.
			for _, other := range []struct {
				kind    string
				present func(*models.WorkspaceExport, string) bool
			}{
				{"comment", func(e *models.WorkspaceExport, id string) bool {
					for _, cm := range e.Comments {
						if cm.ItemID == id {
							return true
						}
					}
					return false
				}},
				{"version", func(e *models.WorkspaceExport, id string) bool {
					for _, v := range e.ItemVersions {
						if v.ItemID == id {
							return true
						}
					}
					return false
				}},
				{"link", func(e *models.WorkspaceExport, id string) bool {
					for _, lk := range e.ItemLinks {
						if lk.SourceID == id || lk.TargetID == id {
							return true
						}
					}
					return false
				}},
			} {
				if other.kind != tc.name && other.present(exp, orphan.ID) {
					t.Fatalf("fixture is not single-reason: it also carries a %s on the orphaned item", other.kind)
				}
			}

			imported, err := s.ImportWorkspace(exp, "orphan-"+tc.name+"-2884-target", owner.ID)
			if err != nil {
				t.Fatalf("a bundle with one orphaned item carrying a %s aborted the whole import: %v", tc.name, err)
			}

			// The orphan is skipped — that part is correct and unchanged — but
			// the rest of the workspace must land.
			got, err := s.ListItems(imported.ID, models.ItemListParams{})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			if len(got) != 1 || got[0].Title != "Live item" {
				titles := make([]string, 0, len(got))
				for _, i := range got {
					titles = append(titles, i.Title)
				}
				t.Errorf("imported items = %v, want just [Live item]", titles)
			}
		})
	}
}

// TestImportDoesNotInferTraitsForArchivedCollections discriminates the SECOND
// archived-collection skip: canonical-trait inference.
//
// Inference exists so a pre-traits archive comes up ROUTING (BUG-2702). An
// archived collection never routes — ListTraitedCollections filters
// deleted_at IS NULL — so stamping it writes a declaration nothing reads, and
// where the archived row is a renamed predecessor of a live one, puts the
// canonical set on two rows in the same workspace.
//
// The fixture is a workspace whose conventions collection was DELETED with no
// live replacement, which is the only shape that reaches inference: the slug
// is what inference keys on, and slug uniqueness spans deleted rows, so an
// archived "conventions" and a live one cannot coexist.
func TestImportDoesNotInferTraitsForArchivedCollections(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "inference2884@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Inference 2884")

	conv, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Conventions", Prefix: "CONV"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if conv.Slug != "conventions" {
		t.Fatalf("fixture assumed slug %q, got %q — inference keys on the slug", "conventions", conv.Slug)
	}
	// A pre-traits archive's shape: the column holds no declarations. Written
	// directly because the create path may stamp them.
	if _, err := s.db.Exec(s.q(`UPDATE collections SET traits = '{}' WHERE id = ?`), conv.ID); err != nil {
		t.Fatalf("blank traits: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, conv.ID, models.ItemCreate{Title: "A rule"}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := s.DeleteCollection(conv.ID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	// Control: the bundle carries the archived collection, blank, and it is
	// the slug inference would act on. Without this the assertion below could
	// pass because nothing reached inference at all.
	var carried bool
	for _, c := range exp.Collections {
		if c.Slug == "conventions" {
			carried = true
			if c.DeletedAt == "" {
				t.Fatal("control leg failed: the archived collection exported without its mark")
			}
			if parsed, perr := models.ParseCollectionTraits(c.Traits); perr != nil || !parsed.IsZero() {
				t.Fatalf("control leg failed: exported traits are not blank (err=%v), so inference would not have fired either way", perr)
			}
		}
	}
	if !carried {
		t.Fatal("control leg failed: the bundle carries no conventions collection")
	}

	imported, err := s.ImportWorkspace(exp, "inference-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	var stored string
	if err := s.db.QueryRow(s.q(`SELECT traits FROM collections WHERE workspace_id = ? AND slug = ?`), imported.ID, "conventions").Scan(&stored); err != nil {
		t.Fatalf("read imported traits: %v", err)
	}
	parsed, perr := models.ParseCollectionTraits(stored)
	if perr != nil {
		t.Fatalf("ParseCollectionTraits(%q): %v", stored, perr)
	}
	if !parsed.IsZero() {
		t.Errorf("inference stamped the canonical set onto an ARCHIVED collection; stored traits = %s", stored)
	}
}

// keepUnless drops the entries matching drop, unless keep is true, in which
// case the slice is returned untouched. Used to reduce an exported bundle to
// exactly one kind of orphan-attached dependent.
func keepUnless[T any](in []T, keep bool, drop func(T) bool) []T {
	if keep {
		return in
	}
	out := in[:0]
	for _, v := range in {
		if !drop(v) {
			out = append(out, v)
		}
	}
	return out
}

// TestImportSurvivesOrphanedParent covers the parent_id half of the same
// class, which my own sweep of the itemMap consumers missed and codex round 3
// caught: the guard I added to the second pass protects the item BEING
// updated, not the parent VALUE it writes.
//
// Both passes resolve a parent through itemMap, which holds an entry for an
// orphaned item, so a live child of an archived-collection parent had a
// nonexistent parent_id written into a foreign key. On Postgres that aborts
// the transaction and the whole import is lost; the child is created before
// the orphan is skipped, so this is reachable on the first pass too.
//
// Correct behaviour is the one the bundle can express: import the child with
// NO parent. The parent is not in the bundle, so there is nothing to point at,
// and dropping the edge is what item_links already does for a missing
// endpoint.
func TestImportSurvivesOrphanedParent(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "orphanparent2884@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Orphan Parent 2884")

	// The parent is created FIRST so it sorts ahead of the child in the
	// export's created_at ordering — that is what makes the first pass, not
	// only the second, resolve the parent through itemMap.
	archived, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Archived", Prefix: "ARCH"})
	if err != nil {
		t.Fatalf("CreateCollection(archived): %v", err)
	}
	parent, err := s.CreateItem(ws.ID, archived.ID, models.ItemCreate{Title: "Archived parent"})
	if err != nil {
		t.Fatalf("CreateItem(parent): %v", err)
	}
	live, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Live", Prefix: "LIVE"})
	if err != nil {
		t.Fatalf("CreateCollection(live): %v", err)
	}
	child, err := s.CreateItem(ws.ID, live.ID, models.ItemCreate{Title: "Live child", ParentID: &parent.ID})
	if err != nil {
		t.Fatalf("CreateItem(child): %v", err)
	}
	if err := s.DeleteCollection(archived.ID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	kept := exp.Collections[:0]
	for _, c := range exp.Collections {
		if c.ID != archived.ID {
			kept = append(kept, c)
		}
	}
	exp.Collections = kept

	// Order the bundle parent-first. The export sorts by created_at then id,
	// and both rows land in the same second, so the id tie-break decides —
	// leaving the first-pass path exercised only about half the time. A bundle
	// may legitimately arrive in any order, and this is the order that reaches
	// BOTH passes, so the test pins it rather than rolling for it.
	ordered := make([]models.ItemExport, 0, len(exp.Items))
	for _, it := range exp.Items {
		if it.ID == parent.ID {
			ordered = append(ordered, it)
		}
	}
	for _, it := range exp.Items {
		if it.ID != parent.ID {
			ordered = append(ordered, it)
		}
	}
	exp.Items = ordered

	// Controls: the bundle carries the child pointing at an item it does not
	// carry, and the parent now sorts first. Without both, the write under
	// test is never attempted.
	var sawChild, sawParent bool
	var parentIdx, childIdx = -1, -1
	for i, it := range exp.Items {
		if it.ID == child.ID {
			sawChild, childIdx = true, i
			if it.ParentID != parent.ID {
				t.Fatalf("control leg failed: exported child's parent_id = %q, want the archived parent", it.ParentID)
			}
		}
		if it.ID == parent.ID {
			sawParent, parentIdx = true, i
		}
	}
	if !sawChild || !sawParent {
		t.Fatalf("control leg failed: bundle carries child=%v parent=%v", sawChild, sawParent)
	}
	if parentIdx > childIdx {
		t.Fatalf("control leg failed: parent sorts after child (%d > %d), so the first pass never resolves it", parentIdx, childIdx)
	}

	imported, err := s.ImportWorkspace(exp, "orphan-parent-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("a live item whose parent is orphaned aborted the whole import: %v", err)
	}

	items, err := s.ListItems(imported.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Live child" {
		titles := make([]string, 0, len(items))
		for _, i := range items {
			titles = append(titles, i.Title)
		}
		t.Fatalf("imported items = %v, want just [Live child]", titles)
	}
	if items[0].ParentID != nil && *items[0].ParentID != "" {
		t.Errorf("imported child kept a parent_id (%q) naming an item the bundle never carried", *items[0].ParentID)
	}
}

// TestImportSurvivesDuplicateLinks covers the residual half of the links loop.
// The insertedItems guard removes the foreign-key route, but a hand-edited or
// foreign bundle can still carry the same (source, target, link_type) twice —
// the source's UNIQUE constraint bounds what pad writes, not what arrives. The
// loop answered that with `continue`, which is not a recovery on Postgres: the
// unique violation has already aborted the transaction, so COMMIT fails and
// the entire restore is lost over a row the loop meant to ignore.
func TestImportSurvivesDuplicateLinks(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "duplinks2884@test.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Duplicate Links 2884")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Widgets", Prefix: "WID"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	a, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "A"})
	if err != nil {
		t.Fatalf("CreateItem(a): %v", err)
	}
	b, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "B"})
	if err != nil {
		t.Fatalf("CreateItem(b): %v", err)
	}
	if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: b.ID, LinkType: "blocks"}, a.ID); err != nil {
		t.Fatalf("CreateItemLink: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if len(exp.ItemLinks) != 1 {
		t.Fatalf("control leg failed: exported links = %d, want 1", len(exp.ItemLinks))
	}
	// The hand-edit: the same edge twice, with distinct ids, exactly as a
	// merged or concatenated bundle would carry it.
	dup := exp.ItemLinks[0]
	dup.ID = dup.ID + "-dup"
	exp.ItemLinks = append(exp.ItemLinks, dup)

	imported, err := s.ImportWorkspace(exp, "duplicate-links-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("a bundle carrying one duplicated link aborted the whole import: %v", err)
	}

	items, err := s.ListItems(imported.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("imported items = %d, want 2", len(items))
	}
}
