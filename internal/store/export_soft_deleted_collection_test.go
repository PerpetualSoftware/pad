package store

import (
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
	imported, err := s.ImportWorkspace(exp, "routing-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
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

// TestImportSurvivesOrphanedItemWithComment covers a SECOND defect, found by
// the dependents test above while the export half was still unfixed: an
// orphaned item — one whose collection the bundle does not carry — aborted the
// ENTIRE workspace import if it had a comment.
//
// The comment loop guards on `itemMap[cm.ItemID] == ""`, and that guard cannot
// fire for an item the bundle contains: itemMap is populated for EVERY item
// before the orphan skip, deliberately, because parent resolution reads it for
// items the loop has not reached. So the loop INSERTed a comment whose item_id
// names no row and the FK refused it, failing the whole restore. This is the
// same defect the reminder loop already carries a long comment about ("ONE
// orphaned item with a reminder aborted the entire workspace import"); it was
// fixed there and left standing here.
//
// Fixing the export half removes pad's own route to it — a bundle this build
// writes has no orphans — but not a hand-edited, foreign, or pre-fix archive,
// which is exactly the population that needs to import.
func TestImportSurvivesOrphanedItemWithComment(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "orphan2884@test.com", "Owner", "password123")
	ws, archived, it := archivedCollectionFixture(t, s, "Orphan Comment 2884")

	if _, err := s.CreateComment(ws.ID, it.ID, "", models.CommentCreate{Author: "Rook", Body: "on an orphan"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	// Hand-edit the bundle into the pre-fix shape: the item stays, its
	// collection is removed. This is what every archive written before this
	// change looks like, and what a foreign bundle may look like at any time.
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
	var orphanHasComment bool
	for _, cm := range exp.Comments {
		if cm.ItemID == it.ID {
			orphanHasComment = true
		}
	}
	if !orphanHasComment {
		t.Fatal("control leg failed: the bundle carries no comment on the orphaned item")
	}

	imported, err := s.ImportWorkspace(exp, "orphan-comment-2884-target", owner.ID)
	if err != nil {
		t.Fatalf("a bundle with one orphaned item aborted the whole import: %v", err)
	}

	// The orphan is skipped — that part is correct and unchanged — but the
	// rest of the workspace must land.
	items, err := s.ListItems(imported.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Live item" {
		got := make([]string, 0, len(items))
		for _, i := range items {
			got = append(got, i.Title)
		}
		t.Errorf("imported items = %v, want just [Live item]", got)
	}
}
