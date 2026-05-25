package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWikiLinks_CreateItemIndexesRefs is the headline Phase 1 test:
// creating an item with `[[REF]]` in its body produces queryable
// backlinks for the target item. End-to-end coverage of the write
// path → resolver → GetBacklinks read path.
func TestWikiLinks_CreateItemIndexesRefs(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target item", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source item",
		"Please see ["+"["+target.CollectionPrefix+"-"+itoa(*target.ItemNumber)+"]] for context.")

	backlinks, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(backlinks) != 1 {
		t.Fatalf("expected 1 backlink, got %d: %+v", len(backlinks), backlinks)
	}
	bl := backlinks[0]
	if bl.SourceItemID != source.ID {
		t.Errorf("SourceItemID: got %q want %q", bl.SourceItemID, source.ID)
	}
	if bl.SourceTitle != "Source item" {
		t.Errorf("SourceTitle: got %q", bl.SourceTitle)
	}
	if bl.SourceRef != source.CollectionPrefix+"-"+itoa(*source.ItemNumber) {
		t.Errorf("SourceRef: got %q", bl.SourceRef)
	}
	if !strings.Contains(bl.Snippet, "for context.") {
		t.Errorf("snippet should contain surrounding text, got %q", bl.Snippet)
	}
}

// TestWikiLinks_UpdateItemReplacesIndex confirms re-parsing kicks in
// on UpdateItem: changing the body to remove a [[]] removes its
// backlink row, and adding a new [[]] adds a row.
func TestWikiLinks_UpdateItemReplacesIndex(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	a := createTestItem(t, s, ws.ID, col.ID, "A", "")
	b := createTestItem(t, s, ws.ID, col.ID, "B", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Mentions [["+refOf(a)+"]] only.")

	// A should have one backlink.
	got, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Fatalf("step 1: expected A to have 1 backlink, got %d", len(got))
	}

	// Rewrite body to mention B instead. Expect: A loses its
	// backlink, B gains one.
	newContent := "Now mentions [[" + refOf(b) + "]] instead."
	if _, err := s.UpdateItem(source.ID, models.ItemUpdate{Content: &newContent}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if got, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(got) != 0 {
		t.Errorf("step 2: A should have 0 backlinks after rewrite, got %d", len(got))
	}
	if got, _ := s.GetBacklinks(b.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(got) != 1 {
		t.Errorf("step 2: B should have 1 backlink after rewrite, got %d", len(got))
	}
}

// TestWikiLinks_DeleteItemCascadesOutboundRows verifies the FK
// ON DELETE CASCADE: when a source item is hard-deleted, its
// outbound wiki-link rows go with it. Soft-delete (the default for
// items) doesn't trigger this — the rows persist but GetBacklinks
// filters via items.deleted_at IS NULL on the source.
func TestWikiLinks_DeleteItemCascadesOutboundRows(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Mentions [["+refOf(target)+"]].")

	if got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(got) != 1 {
		t.Fatalf("baseline: expected 1 backlink, got %d", len(got))
	}

	// Soft-delete the source. Backlinks should hide it via the
	// items.deleted_at IS NULL filter even though the row is still
	// in item_wiki_links.
	if err := s.DeleteItem(source.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true}); len(got) != 0 {
		t.Errorf("after soft-delete source: expected 0 backlinks, got %d", len(got))
	}
}

// TestWikiLinks_SelfLinkHidden — an item that mentions its own
// ref/title in its own body shouldn't appear in its own "Mentioned
// in" panel (PLAN-1593 behavior decision).
func TestWikiLinks_SelfLinkHidden(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// We can't reference an item by ref in its OWN body at create
	// time because the ref isn't assigned until after the INSERT.
	// So create-empty-then-update with the self-link.
	self := createTestItem(t, s, ws.ID, col.ID, "Self-link", "")
	body := "I mention myself: [[" + refOf(self) + "]]."
	if _, err := s.UpdateItem(self.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	got, err := s.GetBacklinks(self.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 backlinks (self filtered), got %d: %+v", len(got), got)
	}
}

// TestWikiLinks_BrokenRefPersisted — a `[[NOTAREAL-99]]` saves with
// target_item_id = NULL. Verifies the broken-link row reaches the
// table even though it can't be queried via the target-id index.
// Feeds the future broken-links report.
func TestWikiLinks_BrokenRefPersisted(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	src := createTestItem(t, s, ws.ID, col.ID, "Has broken ref",
		"This ref doesn't exist: [[NOTAREAL-99]].")

	var count int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE source_item_id = ? AND target_item_id IS NULL AND target_ref = ?
	`), src.ID, "NOTAREAL-99").Scan(&count)
	if err != nil {
		t.Fatalf("count broken refs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 unresolved row, got %d", count)
	}
}

// TestWikiLinks_RepeatedRefStoresMultipleRows — same target mentioned
// N times in one source body produces N rows (different positions).
// Display-side dedupe is the renderer's job; storage keeps every
// occurrence so future "show me where" features can highlight each.
func TestWikiLinks_RepeatedRefStoresMultipleRows(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	ref := refOf(target)
	body := "First [[" + ref + "]], second [[" + ref + "]], third [[" + ref + "]]."
	src := createTestItem(t, s, ws.ID, col.ID, "Mentions thrice", body)

	var count int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE source_item_id = ? AND target_item_id = ?
	`), src.ID, target.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows for repeated ref, got %d", count)
	}

	// GetBacklinks returns one row per stored row (snippet differs
	// per position). Display dedupe is a higher layer's concern.
	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(bls) != 3 {
		t.Errorf("expected 3 backlinks (one per stored row), got %d", len(bls))
	}
}

// TestWikiLinks_CodeBlocksExcludedAtIndexTime is the integration
// counterpart to the parser test: a fenced block with [[REF]] inside
// must NOT produce a backlink row. Parser exclusion proven end-to-end.
func TestWikiLinks_CodeBlocksExcludedAtIndexTime(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	ref := refOf(target)
	body := "Real link: [[" + ref + "]]\n" +
		"```\n" +
		"In code: [[" + ref + "]] should NOT count\n" +
		"```\n" +
		"After block."
	createTestItem(t, s, ws.ID, col.ID, "Mixed", body)

	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(bls) != 1 {
		t.Errorf("expected 1 backlink (code-block one excluded), got %d", len(bls))
	}
}

// TestWikiLinks_BackfillIdempotent — calling BackfillWikiLinks twice
// produces the same final state. The first call populates rows; the
// second short-circuits via the EXISTS check and inserts nothing new.
func TestWikiLinks_BackfillIdempotent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "Links to [["+refOf(target)+"]].")

	// Wipe to simulate "backfill has never run."
	if _, err := s.db.Exec(`DELETE FROM item_wiki_links`); err != nil {
		t.Fatalf("clear table: %v", err)
	}

	bf1, err := s.BackfillWikiLinks()
	if err != nil {
		t.Fatalf("BackfillWikiLinks (1st): %v", err)
	}
	if bf1.LinksInserted == 0 {
		t.Errorf("first backfill should have inserted rows, got 0")
	}

	bf2, err := s.BackfillWikiLinks()
	if err != nil {
		t.Fatalf("BackfillWikiLinks (2nd): %v", err)
	}
	if bf2.LinksInserted != 0 {
		t.Errorf("second backfill should be a no-op, got %d new rows", bf2.LinksInserted)
	}

	// The reverse-index query should still find the source.
	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 1 {
		t.Errorf("after backfill: expected 1 backlink, got %d", len(bls))
	}
}

// TestWikiLinks_EmptyDisplayDistinct regresses Codex round-12 P3:
// `[[TASK|]]` (explicit empty display override) and `[[TASK]]` (no
// override) are distinct shapes in the editor and must store distinct
// rows. Empty override → display_text=”, no override → display_text
// IS NULL.
func TestWikiLinks_EmptyDisplayDistinct(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	tref := refOf(target)

	// Two source items: one references with an explicit empty
	// override `[[REF|]]`, the other with no override `[[REF]]`.
	srcWith := createTestItem(t, s, ws.ID, col.ID, "With empty override",
		"See ["+"["+tref+"|]] for details.")
	srcNo := createTestItem(t, s, ws.ID, col.ID, "No override",
		"See ["+"["+tref+"]] for details.")

	var withDispValid bool
	var withText string
	err := s.db.QueryRow(s.q(`
		SELECT display_text IS NOT NULL, COALESCE(display_text, '')
		FROM item_wiki_links WHERE source_item_id = ?
	`), srcWith.ID).Scan(&withDispValid, &withText)
	if err != nil {
		t.Fatalf("query empty-override row: %v", err)
	}
	if !withDispValid {
		t.Errorf("[[REF|]] should store display_text as '' (not NULL)")
	}
	if withText != "" {
		t.Errorf("[[REF|]] display_text should be empty string, got %q", withText)
	}

	var noDispValid bool
	err = s.db.QueryRow(s.q(`
		SELECT display_text IS NOT NULL FROM item_wiki_links WHERE source_item_id = ?
	`), srcNo.ID).Scan(&noDispValid)
	if err != nil {
		t.Fatalf("query no-override row: %v", err)
	}
	if noDispValid {
		t.Errorf("[[REF]] should store display_text as NULL")
	}

	// Wire-shape check: GetBacklinks must surface the distinction
	// via the *string typed DisplayText field. Codex round-13 P2.
	withBLs, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	var withBL, noBL *models.Backlink
	for i, bl := range withBLs {
		switch bl.SourceItemID {
		case srcWith.ID:
			withBL = &withBLs[i]
		case srcNo.ID:
			noBL = &withBLs[i]
		}
	}
	if withBL == nil || noBL == nil {
		t.Fatalf("expected both source backlinks; got withBL=%v noBL=%v", withBL, noBL)
	}
	if withBL.DisplayText == nil || *withBL.DisplayText != "" {
		t.Errorf("[[REF|]] DisplayText should be non-nil pointer to \"\", got %+v", withBL.DisplayText)
	}
	if noBL.DisplayText != nil {
		t.Errorf("[[REF]] DisplayText should be nil, got %+v", *noBL.DisplayText)
	}
}

// TestWikiLinks_SnippetIsValidUTF8 — `snippetAround` may slice
// `content` at boundaries that fall mid-rune. The function must
// extend both edges to rune boundaries so the snippet is always
// valid UTF-8. Codex round-8 P3.
func TestWikiLinks_SnippetIsValidUTF8(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	tref := refOf(target)
	// Pad each side of the link with enough multi-byte runes that
	// the ±40-byte snippet window will likely cut through one.
	// "🎉" is 4 bytes. "héllo" has é = 2 bytes. Both should round-trip.
	body := "🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉 héllo " + "[" + "[" + tref + "]] héllo 🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉"
	createTestItem(t, s, ws.ID, col.ID, "Source", body)

	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(bls) != 1 {
		t.Fatalf("expected 1 backlink, got %d", len(bls))
	}
	// `utf8.ValidString` is the canonical check; the snippet must
	// pass regardless of where the ±40-byte cut landed.
	if !utf8.ValidString(bls[0].Snippet) {
		t.Errorf("snippet is invalid UTF-8: %q", bls[0].Snippet)
	}
}

// TestWikiLinks_MixedCaseRefIndexed regresses Codex round-1 P2: a
// body containing `[[task-5]]` must produce a backlink row pointing
// at the same target as `[[TASK-5]]`. Renderer permissiveness and
// indexer behavior have to agree, otherwise users see broken silent
// data divergence.
func TestWikiLinks_MixedCaseRefIndexed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	// Author writes a mixed-case ref. Renderer accepts; indexer
	// must too. The store should normalize to the canonical
	// uppercase form so the backlinks query (which compares
	// against `collections.prefix`, canonically uppercase)
	// returns the row.
	lc := strings.ToLower(refOf(target))
	createTestItem(t, s, ws.ID, col.ID, "Lowercased ref source", "See ["+"["+lc+"]] please.")

	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(bls) != 1 {
		t.Fatalf("expected 1 backlink for mixed-case ref input, got %d", len(bls))
	}
	// The stored target_ref should be the canonical uppercase form.
	var storedRef string
	err = s.db.QueryRow(s.q(`SELECT target_ref FROM item_wiki_links WHERE source_item_id = ?`),
		bls[0].SourceItemID).Scan(&storedRef)
	if err != nil {
		t.Fatalf("read stored target_ref: %v", err)
	}
	if storedRef != refOf(target) {
		t.Errorf("stored target_ref should be uppercase %q, got %q", refOf(target), storedRef)
	}
}

// TestWikiLinks_VisibilityAwarePagination regresses Codex round-1 P1:
// when GetBacklinks is called with a visibility-restricted
// `visibleCollectionIDs` slice, the LIMIT/OFFSET counts only visible
// rows. A page asking for `limit=2` must return 2 visible rows even
// if the underlying raw set has hidden rows interleaved with them.
//
// Setup:
//   - target receives 3 backlinks: src1 (visible), src2 (HIDDEN), src3 (visible).
//   - With nil visibility (no restriction): all 3 are returned.
//   - With visibility = [visible collection only]: limit=2 returns
//     [src3, src1] (newest-first), NOT [src3] (which would be the
//     bug — hidden src2 silently consuming a slot).
func TestWikiLinks_VisibilityAwarePagination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	visible := createTestCollection(t, s, ws.ID, "Visible")
	hidden := createTestCollection(t, s, ws.ID, "Hidden")

	target := createTestItem(t, s, ws.ID, visible.ID, "Target", "")
	tref := refOf(target)
	// Insert in update_at order: src1 → src2 → src3.
	// Newest (src3) appears first in the sorted result.
	createTestItem(t, s, ws.ID, visible.ID, "Src1 visible", "Refs ["+"["+tref+"]].")
	createTestItem(t, s, ws.ID, hidden.ID, "Src2 hidden", "Refs ["+"["+tref+"]].")
	createTestItem(t, s, ws.ID, visible.ID, "Src3 visible", "Refs ["+"["+tref+"]].")

	t.Run("nil visibility returns all 3", func(t *testing.T) {
		bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
		if err != nil {
			t.Fatalf("GetBacklinks: %v", err)
		}
		if len(bls) != 3 {
			t.Errorf("nil visibility: expected 3 backlinks, got %d", len(bls))
		}
	})

	t.Run("visible-only limit=2 returns 2 visible rows", func(t *testing.T) {
		bls, err := s.GetBacklinks(target.ID, ws.ID, 2, 0, BacklinksVisibility{
			FullCollectionIDs: []string{visible.ID},
		})
		if err != nil {
			t.Fatalf("GetBacklinks: %v", err)
		}
		if len(bls) != 2 {
			t.Fatalf("expected exactly 2 visible backlinks (limit honored), got %d: %+v", len(bls), bls)
		}
		for _, bl := range bls {
			if bl.SourceCollectionSlug != "visible" {
				t.Errorf("expected only visible-collection sources, got slug=%q", bl.SourceCollectionSlug)
			}
		}
	})

	t.Run("empty visibility returns nothing", func(t *testing.T) {
		bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{})
		if err != nil {
			t.Fatalf("GetBacklinks: %v", err)
		}
		if len(bls) != 0 {
			t.Errorf("empty visibility set: expected 0 backlinks, got %d", len(bls))
		}
	})
}

// TestWikiLinks_ItemGrantPagination regresses Codex round-2 P1: when
// a guest has an item-level grant on ONE source item in a collection
// they can't otherwise see, the rest of that collection must NOT
// leak into the page. Previously the handler computed
// `visibleCollectionIDs` as the UNION (full-collection grants ∪
// collections containing granted items), then filtered each row's
// item-level visibility in Go AFTER fetching — letting hidden rows
// consume LIMIT slots. The refactor pushes the precise
// (FullCollectionIDs OR GrantedItemIDs) predicate into SQL.
//
// Setup:
//   - target in collection A (no relevance to grants — just the item
//     being linked to).
//   - Three sources, all linking to target, in collection B (which
//     the guest has NO full access to):
//   - src_g: the granted item
//   - src_x, src_y: also in B, not granted (hidden)
//   - Guest visibility: FullCollectionIDs=[] (no full access),
//     GrantedItemIDs=[src_g.ID].
//
// Expectation: limit=2 returns exactly [src_g], not [src_g] + leaked
// rows from B. The bad-pagination version returned 0 or 1 depending
// on how the hidden rows interleaved.
func TestWikiLinks_ItemGrantPagination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	a := createTestCollection(t, s, ws.ID, "A")
	b := createTestCollection(t, s, ws.ID, "B")

	target := createTestItem(t, s, ws.ID, a.ID, "Target", "")
	tref := refOf(target)

	// Three sources in B, all referencing target. src_g is the one
	// we'll grant the guest access to; src_x and src_y are hidden.
	srcG := createTestItem(t, s, ws.ID, b.ID, "Granted", "Refs [["+tref+"]].")
	createTestItem(t, s, ws.ID, b.ID, "Hidden X", "Refs [["+tref+"]].")
	createTestItem(t, s, ws.ID, b.ID, "Hidden Y", "Refs [["+tref+"]].")

	// Sanity: unrestricted view sees all three.
	all, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks unrestricted: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("baseline: expected 3 unrestricted backlinks, got %d", len(all))
	}

	// Guest with item-grant only on src_g, no full collection access.
	vis := BacklinksVisibility{
		FullCollectionIDs: nil, // no full-collection grants
		GrantedItemIDs:    []string{srcG.ID},
	}
	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, vis)
	if err != nil {
		t.Fatalf("GetBacklinks restricted: %v", err)
	}
	if len(bls) != 1 {
		t.Fatalf("expected exactly 1 backlink (the granted item), got %d: %+v", len(bls), bls)
	}
	if bls[0].SourceItemID != srcG.ID {
		t.Errorf("expected granted item %q, got %q", srcG.ID, bls[0].SourceItemID)
	}

	// Pagination must agree: limit=2 should still return exactly
	// [srcG] without the hidden B-collection rows consuming slots.
	bls2, err := s.GetBacklinks(target.ID, ws.ID, 2, 0, vis)
	if err != nil {
		t.Fatalf("GetBacklinks restricted limit=2: %v", err)
	}
	if len(bls2) != 1 {
		t.Errorf("limit=2 should return 1 visible row, not silently shrink: got %d", len(bls2))
	}
}

// refOf builds a PREFIX-NUMBER string from a fresh item — used by the
// integration tests since the canonical ref isn't on the model
// directly. ItemNumber is *int on the model so handle nil
// defensively (shouldn't happen for created items but the type lets
// callers express "no number yet").
func refOf(item *models.Item) string {
	num := 0
	if item.ItemNumber != nil {
		num = *item.ItemNumber
	}
	return item.CollectionPrefix + "-" + itoa(num)
}

// -- Phase 2a (TASK-1595): title-form backlinks --

// TestWikiLinks_TitleFormIndexed exercises the headline Phase 2a path:
// `[[Title]]` in a source's body produces a backlink for the target
// when the target's title matches (case-insensitive).
func TestWikiLinks_TitleFormIndexed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Project Goals", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Please see [[Project Goals]] for context.")

	got, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 backlink, got %d: %+v", len(got), got)
	}
	if got[0].SourceItemID != source.ID {
		t.Errorf("SourceItemID: got %q want %q", got[0].SourceItemID, source.ID)
	}
}

// TestWikiLinks_TitleFormCaseInsensitive — resolveTitleTx uses LOWER()
// to mirror the renderer's `.toLowerCase()` comparison. A source that
// writes `[[project goals]]` must still resolve to an item titled
// "Project Goals".
func TestWikiLinks_TitleFormCaseInsensitive(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Project Goals", "")
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"See [[project goals]] for the plan.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected 1 backlink (case-insensitive resolution), got %d", len(got))
	}
}

// TestWikiLinks_BrokenTitlePersistedThenResolved covers two beats:
//
//   - A `[[Title]]` whose target doesn't exist yet persists with
//     target_item_id=NULL so the row is queryable later.
//   - When the missing item is CREATED, resolveBrokenTitleLinks flips
//     the row to point at it — backlinks resolve on next query without
//     waiting for a content rewrite or a backfill run.
func TestWikiLinks_BrokenTitlePersistedThenResolved(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Source first, target later. At this point [[Future Title]]
	// doesn't resolve.
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Anchor for [[Future Title]] which doesn't exist yet.")
	_ = source

	// Now create the target. The create hook should flip the broken
	// row to point at the new item.
	target := createTestItem(t, s, ws.ID, col.ID, "Future Title", "")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected source to resolve to target after creation, got %d backlinks", len(got))
	}
}

// TestWikiLinks_TitleRenameCascadesContentAndBacklinks is the headline
// Phase 2a behavior: rename an item and (a) sources that referenced
// the old title get their bodies rewritten in-band, (b) their
// index rows refresh, (c) "who mentions me?" still finds them under
// the new title.
func TestWikiLinks_TitleRenameCascadesContentAndBacklinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Old Title", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Please see [[Old Title]] and again [[Old Title]] here.")

	// Baseline: target has 1 backlink (multiplicity stored but the
	// query returns one row per source by snippet position — we get
	// 2 rows actually since multiplicity is preserved per PLAN-1593;
	// either way both should point at the same source).
	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 2 {
		t.Fatalf("baseline: expected 2 rows (multiplicity preserved), got %d", len(bls))
	}

	// Rename the target. Cascade should rewrite the source's content
	// and refresh its index rows. The renamed target still has its
	// backlinks queryable.
	newTitle := "New Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}

	// Source's content should have been rewritten in-band.
	updatedSource, err := s.GetItem(source.ID)
	if err != nil {
		t.Fatalf("GetItem source: %v", err)
	}
	if strings.Contains(updatedSource.Content, "[[Old Title]]") {
		t.Errorf("expected [[Old Title]] to be rewritten, source content: %q", updatedSource.Content)
	}
	if !strings.Contains(updatedSource.Content, "[[New Title]]") {
		t.Errorf("expected [[New Title]] in rewritten content, got %q", updatedSource.Content)
	}

	// Target's backlinks still find the source via the new title.
	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 2 {
		t.Errorf("post-rename: expected 2 backlinks, got %d", len(got))
	}
	for _, bl := range got {
		if bl.SourceItemID != source.ID {
			t.Errorf("backlink source should still be %q, got %q", source.ID, bl.SourceItemID)
		}
	}
}

// TestWikiLinks_TitleRenameCascadesAliasedForms regresses Codex round 1
// finding: the cascade must preserve display aliases when rewriting
// title-form links. `[[Old Title|alias]]` AND `[[old title]]` (mixed
// case) both index via target_item_id, so without alias-aware /
// case-insensitive rewrite, the trailing re-parse drops them.
func TestWikiLinks_TitleRenameCascadesAliasedForms(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Old Title", "")
	source := createTestItem(t, s, ws.ID, col.ID, "Source",
		"Plain [[Old Title]], aliased [[Old Title|see this]], "+
			"mixed case [[old title]], and qualified [[tasks/Old Title|qual]] all here.")

	// Baseline: 4 backlink rows (multiplicity preserved).
	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 4 {
		t.Fatalf("baseline: expected 4 rows (multiplicity), got %d", len(bls))
	}

	// Rename and verify all 4 forms got rewritten in source body AND
	// stayed resolved.
	newTitle := "New Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}
	updated, _ := s.GetItem(source.ID)
	for _, oldShape := range []string{
		"[[Old Title]]",
		"[[Old Title|see this]]",
		"[[old title]]",
		"[[tasks/Old Title|qual]]",
	} {
		if strings.Contains(updated.Content, oldShape) {
			t.Errorf("expected %q to be rewritten, content: %q", oldShape, updated.Content)
		}
	}
	for _, newShape := range []string{
		"[[New Title]]",
		"[[New Title|see this]]",
		"[[tasks/New Title|qual]]",
	} {
		if !strings.Contains(updated.Content, newShape) {
			t.Errorf("expected %q in rewritten content, got %q", newShape, updated.Content)
		}
	}

	// All 4 rows still resolve after rename — none flipped to broken.
	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 4 {
		t.Errorf("post-rename: expected 4 resolved backlinks, got %d", len(got))
	}
}

// TestWikiLinks_CollectionQualifiedTitleResolved covers the qualified
// `[[collection_slug/Title]]` form. Stage 1 (full-key match) misses
// because no item is literally titled "tasks/Setup"; stage 2 (split
// fallback) finds the item titled "Setup" in collection "tasks".
func TestWikiLinks_CollectionQualifiedTitleResolved(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks") // slug = "tasks"

	target := createTestItem(t, s, ws.ID, col.ID, "Setup", "")
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"See [[tasks/Setup]] for the install steps.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("collection-qualified link should resolve, got %d backlinks", len(got))
	}
}

// TestWikiLinks_FullKeyTitleBeatsQualifiedSplit regresses Codex
// finding #3 from the planning round: an item literally titled
// "tasks/Setup" must win stage 1 BEFORE the resolver splits on `/`
// and looks up by collection slug. If we split first, the wrong item
// would resolve.
func TestWikiLinks_FullKeyTitleBeatsQualifiedSplit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks") // slug = "tasks"

	// Two items: one literally titled "tasks/Setup" (the trick
	// case), one titled "Setup" in the tasks collection (the
	// fallback-match case). The link `[[tasks/Setup]]` must
	// resolve to the FIRST — the literal title beats the qualified-
	// split interpretation per renderer order.
	literalTitle := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")
	fallback := createTestItem(t, s, ws.ID, col.ID, "Setup", "")
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"Link: [[tasks/Setup]] here.")

	literalBls, _ := s.GetBacklinks(literalTitle.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(literalBls) != 1 {
		t.Errorf("literal-title item should win stage 1, got %d backlinks", len(literalBls))
	}
	fallbackBls, _ := s.GetBacklinks(fallback.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(fallbackBls) != 0 {
		t.Errorf("fallback item should NOT resolve when stage 1 hits, got %d backlinks", len(fallbackBls))
	}
}

// TestWikiLinks_BrokenPipeInBodyRetargetsOnLiteralArrival regresses
// Codex round 4: a source body `[[A|B]]` written BEFORE any matching
// item exists must store target_title="A|B" (the full body), not the
// split key "A". Otherwise resolveBrokenTitleLinks for a later-
// arriving item titled "A|B" can't find the row — the index goes
// stale while the renderer's full-body interpretation would resolve
// the link correctly.
func TestWikiLinks_BrokenPipeInBodyRetargetsOnLiteralArrival(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Source first; nothing matches.
	createTestItem(t, s, ws.ID, col.ID, "Source", "Future: [[A|B]] arrives later.")

	// Now create an item literally titled "A|B".
	target := createTestItem(t, s, ws.ID, col.ID, "A|B", "")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected broken pipe-in-body row to retarget on literal arrival, got %d backlinks", len(got))
	}
}

// TestWikiLinks_LiteralPipeInTitleResolves regresses Codex round 3 P1:
// an item literally titled "A|B" (pipe in title) must match
// `[[A|B]]` in source content — the renderer's L516-525 tries the
// full body as a title BEFORE splitting on the pipe. Without this,
// the index would split-then-look-up "A" and either resolve to a
// different item or fail to resolve at all, leaving a backlink the
// UI shows but the index can't surface.
func TestWikiLinks_LiteralPipeInTitleResolves(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Item literally titled with a pipe character. Pad allows this.
	target := createTestItem(t, s, ws.ID, col.ID, "A|B", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[A|B]] for the doc.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected literal-pipe title to resolve, got %d backlinks", len(got))
	}
}

// TestWikiLinks_LiteralPipeInTitleFallsThroughToSplit covers the
// complement of the above: when no item is literally titled "A|B"
// but an item titled "A" exists, the split interpretation still
// kicks in (title="A", display="B"). The candidate-order in
// replaceWikiLinks tries full body first, then falls through.
func TestWikiLinks_LiteralPipeInTitleFallsThroughToSplit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "A", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[A|B]] for the doc.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("split fallback should resolve `[[A|B]]` to item titled 'A', got %d backlinks", len(got))
	}
}

// TestWikiLinks_SecondItemSameTitleDoesNotStealBacklinks regresses
// Codex round 3 P2: dropping the IS NULL constraint on the stage-1
// UPDATE made the broken-row flip too aggressive. A row already
// resolved to <Foo-v1> via stage-1 literal match must stay there
// when a SECOND item titled "Foo-v1" is created — titles aren't
// unique, the renderer's first-match is array-order-dependent, and
// the index churning would silently move backlinks without warning.
// Stage 3 (literal-arrival retarget) is gated to titles containing
// `/` for exactly this reason.
func TestWikiLinks_SecondItemSameTitleDoesNotStealBacklinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	original := createTestItem(t, s, ws.ID, col.ID, "Foo", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "Mentions [[Foo]] here.")
	bls, _ := s.GetBacklinks(original.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 1 {
		t.Fatalf("baseline: expected 1 backlink, got %d", len(bls))
	}

	// Create a second item with the same title.
	duplicate := createTestItem(t, s, ws.ID, col.ID, "Foo", "")

	// Original should retain its backlink — no theft.
	originalBls, _ := s.GetBacklinks(original.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(originalBls) != 1 {
		t.Errorf("original `Foo` lost its backlink to duplicate creation: got %d", len(originalBls))
	}
	// Duplicate should NOT have inherited any backlinks.
	dupBls, _ := s.GetBacklinks(duplicate.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(dupBls) != 0 {
		t.Errorf("duplicate stole backlinks, got %d", len(dupBls))
	}
}

// TestWikiLinks_LiteralTitleArrivalRetargetsQualifiedFallback regresses
// Codex round 2 P2: a row resolved via qualified-fallback (stage 2)
// must flip to point at a later-arriving literal-title match (stage 1
// always wins per renderer order at markdown.ts:541).
//
//	Step 1: source writes `[[tasks/Setup]]`. No literal "tasks/Setup"
//	        item exists; item "Setup" exists in collection "tasks".
//	        Row resolves via qualified fallback to <Setup>.
//	Step 2: item literally titled "tasks/Setup" is created.
//	        Row MUST retarget to <tasks/Setup> — the literal match.
//
// Without the stage-1 NULL-constraint drop, the index would stay
// stale until the source's content was rewritten.
func TestWikiLinks_LiteralTitleArrivalRetargetsQualifiedFallback(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks") // slug = "tasks"

	// Step 1: fallback resolution.
	fallback := createTestItem(t, s, ws.ID, col.ID, "Setup", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[tasks/Setup]].")
	fallbackBls, _ := s.GetBacklinks(fallback.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(fallbackBls) != 1 {
		t.Fatalf("step 1: fallback should resolve, got %d backlinks", len(fallbackBls))
	}

	// Step 2: literal arrival should win stage 1 and steal the row.
	literal := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")

	got, _ := s.GetBacklinks(literal.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("literal arrival should retarget qualified-fallback row, got %d backlinks", len(got))
	}
	fallbackBls, _ = s.GetBacklinks(fallback.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(fallbackBls) != 0 {
		t.Errorf("fallback should lose the row after literal arrival, got %d backlinks", len(fallbackBls))
	}
}

// TestWikiLinks_TitleRenameResolvesPreExistingBrokenRows covers the
// other half of cascadeTitleRename: a source that wrote `[[New Title]]`
// BEFORE any item had that title — so its row stored as broken —
// should resolve when an existing item gets renamed TO "New Title".
// No content rewrite needed; only the target_item_id flip.
func TestWikiLinks_TitleRenameResolvesPreExistingBrokenRows(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Source mentions "New Title" before any such item exists.
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"I expect a [[New Title]] item to exist someday.")

	// Now rename an existing item TO "New Title". The cascade's
	// stage 2 should flip the broken row.
	target := createTestItem(t, s, ws.ID, col.ID, "Placeholder", "")
	newTitle := "New Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateItem rename: %v", err)
	}

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected pre-existing broken row to resolve after rename, got %d backlinks", len(got))
	}
}

// TestWikiLinks_TitleRenameNoChangeIsNoOp — calling UpdateItem with
// the SAME title (or with no title field at all) must not trigger
// the cascade. Cheap path that paid nothing pre-Phase-2a should pay
// nothing now either.
func TestWikiLinks_TitleRenameNoChangeIsNoOp(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Stable Title", "")
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"See [[Stable Title]] for details.")

	// Update with same title — cascade should early-return on
	// oldTitle == newTitle.
	same := "Stable Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &same}); err != nil {
		t.Fatalf("UpdateItem with same title: %v", err)
	}

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("backlinks unaffected by no-op rename, got %d", len(got))
	}
}

// TestWikiLinks_TitleRenameRewritesSelfReferences — Codex round 5
// finding 2 caught: when the renamed item mentions ITSELF by its
// (now-old) title in its body, the cascade MUST also rewrite the
// self-reference. Otherwise a title-only rename leaves the body's
// `[[Old Title]]` bracket pointing at a now-non-existent title
// while the index still records a working backlink — index and
// renderer drift apart. Self-link visibility filtering happens at
// GetBacklinks query time, so the panel still hides it.
func TestWikiLinks_TitleRenameRewritesSelfReferences(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Item that mentions itself by title.
	item := createTestItem(t, s, ws.ID, col.ID, "Self Title", "")
	body := "I mention myself: [[Self Title]]."
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("seed self-ref content: %v", err)
	}

	// Title-only rename. Cascade INCLUDES self, so the self-ref
	// gets rewritten in-band — body becomes `[[Renamed Self]]`
	// pointing at this same item (now titled "Renamed Self").
	newTitle := "Renamed Self"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename self: %v", err)
	}

	post, _ := s.GetItem(item.ID)
	if strings.Contains(post.Content, "[[Self Title]]") {
		t.Errorf("self-reference should have been rewritten by cascade, got %q", post.Content)
	}
	if !strings.Contains(post.Content, "[[Renamed Self]]") {
		t.Errorf("expected `[[Renamed Self]]` in cascade-rewritten content, got %q", post.Content)
	}

	// Self still hidden from own backlinks panel — GetBacklinks
	// filters self-links at query time independent of indexing.
	got, _ := s.GetBacklinks(item.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 0 {
		t.Errorf("self-link must stay hidden in panel, got %d", len(got))
	}
}

// TestWikiLinks_RefShapedWithWhitespaceFallsThroughToTitle regresses
// Codex round 10 P2: a ref-shaped body with surrounding whitespace
// (`[[ TASK-5 ]]`) that doesn't match any actual ref falls through
// to title lookup using the UNTRIMMED key — matches the renderer's
// `key.toLowerCase()` (no trim) at markdown.ts:541-543. An item
// literally titled " TASK-5 " (with spaces) must resolve via the
// fallback when no real TASK-5 ref exists.
func TestWikiLinks_RefShapedWithWhitespaceFallsThroughToTitle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks") // prefix "TASKS"

	// Item literally titled with whitespace and ref shape. There's
	// no TASKS collection prefix variant matching "TASK-5", so the
	// ref resolution misses and title fallback must use the raw
	// (untrimmed) key.
	target := createTestItem(t, s, ws.ID, col.ID, " TASK-5 ", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[ TASK-5 ]] anyway.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected ref-fallback to use untrimmed key, got %d backlinks", len(got))
	}
}

// TestWikiLinks_SoftDeletedTargetRetargetsOnNewItem regresses Codex
// round 8 P2: when item A "Foo" resolves a backlink and is then
// soft-deleted, a NEW item B titled "Foo" must flip the row to
// point at B. The renderer ignores deleted-target links, so an
// index that keeps pointing at deleted A means GetBacklinks(B)
// misses the link the UI would actually render to B.
func TestWikiLinks_SoftDeletedTargetRetargetsOnNewItem(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	a := createTestItem(t, s, ws.ID, col.ID, "Foo", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "Mention [[Foo]].")
	aBls, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(aBls) != 1 {
		t.Fatalf("baseline: A should have 1 backlink, got %d", len(aBls))
	}

	// Soft-delete A.
	if err := s.DeleteItem(a.ID); err != nil {
		t.Fatalf("delete A: %v", err)
	}

	// Create B with same title — should flip the row.
	b := createTestItem(t, s, ws.ID, col.ID, "Foo", "")

	bBls, _ := s.GetBacklinks(b.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bBls) != 1 {
		t.Errorf("B should inherit the backlink after A's soft-delete + B's creation, got %d", len(bBls))
	}
}

// TestWikiLinks_CascadeDoesNotCorruptLiteralPipeNeighbor regresses
// Codex round 7 finding 2: when items A "Old Title" and B
// "Old Title|alias" are both referenced from one source, renaming A
// must NOT rewrite the B reference. The prior broad-regex cascade
// matched `[[Old Title|alias]]` against the A-rename pattern,
// corrupting the B link. Position-based per-row cascade fixes this
// — only the brackets whose wl row resolves to A get rewritten.
func TestWikiLinks_CascadeDoesNotCorruptLiteralPipeNeighbor(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	a := createTestItem(t, s, ws.ID, col.ID, "Old Title", "")
	b := createTestItem(t, s, ws.ID, col.ID, "Old Title|alias", "")
	createTestItem(t, s, ws.ID, col.ID, "Source",
		"Mentions [[Old Title]] (to A) and [[Old Title|alias]] (to B).")

	// Rename only A.
	newTitle := "New Title"
	if _, err := s.UpdateItem(a.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("rename A: %v", err)
	}

	// A's reference rewritten; B's reference left intact.
	// Look up the source item by querying via items that mention A.
	aBacklinksProbe, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(aBacklinksProbe) == 0 {
		t.Fatalf("expected at least one backlink to A so we can locate source")
	}
	srcItem, err := s.GetItem(aBacklinksProbe[0].SourceItemID)
	if err != nil {
		t.Fatalf("GetItem source: %v", err)
	}
	srcContent := srcItem.Content
	if !strings.Contains(srcContent, "[[New Title]]") {
		t.Errorf("A's reference should be rewritten to [[New Title]], got: %q", srcContent)
	}
	if !strings.Contains(srcContent, "[[Old Title|alias]]") {
		t.Errorf("B's reference [[Old Title|alias]] should be untouched, got: %q", srcContent)
	}
	if strings.Contains(srcContent, "[[New Title|alias]]") {
		t.Errorf("B's reference was incorrectly rewritten via A's cascade, got: %q", srcContent)
	}

	// A's backlinks should include the source (via the rewritten
	// [[New Title]] bracket).
	aBls, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(aBls) != 1 {
		t.Errorf("A should have 1 backlink post-rename, got %d", len(aBls))
	}
	// B's backlinks should still include the source (untouched).
	bBls, _ := s.GetBacklinks(b.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bBls) != 1 {
		t.Errorf("B should still have 1 backlink (its bracket was untouched), got %d", len(bBls))
	}
}

// TestWikiLinks_RefShapedFallsThroughToTitle regresses Codex round 6
// finding 1. A ref-shaped body like `[[ISO-9001]]` should resolve to
// an item literally titled "ISO-9001" when no ISO-9001 ref-item
// exists — the renderer falls through (markdown.ts:513), so the
// index must too. Without the fallback, GetBacklinks would never
// find the backlink even though the renderer renders the link.
func TestWikiLinks_RefShapedFallsThroughToTitle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks") // prefix "TASKS"

	// Item literally titled with a ref shape that does NOT match
	// any existing collection's prefix-number. There's no ISO
	// collection in this workspace, so the body fails the ref
	// resolution and must fall through to title.
	target := createTestItem(t, s, ws.ID, col.ID, "ISO-9001", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[ISO-9001]] for the standard.")

	got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 1 {
		t.Errorf("expected ref-shaped body to fall through to title, got %d backlinks", len(got))
	}
}

// TestWikiLinks_TitleAndContentRenameLeavesUserContentVerbatim
// documents the behavior choice for combined title+content updates:
// the user's just-submitted content is authoritative, so the cascade
// EXCLUDES self in this case (excludeSelf=true). If their new
// content contains `[[Old Title]]`, it stays as written and the
// index correctly stores it as broken — matching what the renderer
// would render. This mirrors documents.go::updateLinksInTx, which
// also leaves the renamed entity's own content alone.
//
// The title-only case (input.Content == nil) is the complement:
// cascade INCLUDES self because there's no fresh user input —
// stale `[[Old Title]]` would otherwise go broken. Covered by
// TestWikiLinks_TitleRenameRewritesSelfReferences.
func TestWikiLinks_TitleAndContentRenameLeavesUserContentVerbatim(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item := createTestItem(t, s, ws.ID, col.ID, "Old Title", "")
	body := "I mention myself: [[Old Title]] in old content."
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("seed self-ref content: %v", err)
	}

	// Combined title + content rename. New content still contains
	// `[[Old Title]]` because the user wrote it that way. Cascade
	// must respect the user's submission — DO NOT auto-rewrite.
	newTitle := "New Title"
	newBody := "Now updated: [[Old Title]] still mentioned in new content."
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{
		Title:   &newTitle,
		Content: &newBody,
	}); err != nil {
		t.Fatalf("combined rename + content update: %v", err)
	}

	post, _ := s.GetItem(item.ID)
	// User content stands as written.
	if !strings.Contains(post.Content, "[[Old Title]]") {
		t.Errorf("combined update: user's [[Old Title]] should remain verbatim, got %q", post.Content)
	}
	// Backlinks-of-self should be empty: index stores the bracket
	// as broken (no item titled "Old Title" exists), self-link
	// filter in GetBacklinks would hide anyway. Either way: 0.
	got, _ := s.GetBacklinks(item.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(got) != 0 {
		t.Errorf("self/broken refs should produce 0 backlinks, got %d", len(got))
	}
}

// TestWikiLinks_DuplicateSlashTitleNoTheft — Codex round 5 finding 1
// regression. When item "tasks/Setup" exists and a source has a
// backlink resolved to it via stage 1 (literal match), creating a
// SECOND item titled "tasks/Setup" must NOT steal the row. The
// stage-3 retarget UPDATE has an EXISTS clause that scopes the flip
// to rows whose current target is NOT titled the same as us —
// i.e. rows resolved via stage-2 qualified fallback to a different
// item, not via stage-1 to a literal twin.
func TestWikiLinks_DuplicateSlashTitleNoTheft(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	original := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")
	createTestItem(t, s, ws.ID, col.ID, "Source", "See [[tasks/Setup]].")
	bls, _ := s.GetBacklinks(original.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(bls) != 1 {
		t.Fatalf("baseline: stage-1 literal should resolve, got %d", len(bls))
	}

	// Create a second item with the same slash-containing title.
	duplicate := createTestItem(t, s, ws.ID, col.ID, "tasks/Setup", "")

	// Original keeps its backlink — no theft from a literal twin.
	stillBls, _ := s.GetBacklinks(original.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(stillBls) != 1 {
		t.Errorf("original slash-title item lost backlink to duplicate creation, got %d", len(stillBls))
	}
	// Duplicate has no backlinks — nothing was stolen.
	dupBls, _ := s.GetBacklinks(duplicate.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if len(dupBls) != 0 {
		t.Errorf("duplicate stole backlinks, got %d", len(dupBls))
	}
}

// createChildItem creates a test item with parent_id set in one
// CreateItem call. The wiki_links tests use this for parent↔child
// suppression coverage where createTestItem (which doesn't set
// parent_id) isn't enough. TASK-1607.
func createChildItem(t *testing.T, s *Store, workspaceID, collectionID, parentID, title, content string) *models.Item {
	t.Helper()
	pid := parentID
	item, err := s.CreateItem(workspaceID, collectionID, models.ItemCreate{
		Title:    title,
		Content:  content,
		Fields:   `{"status":"open"}`,
		ParentID: &pid,
	})
	if err != nil {
		t.Fatalf("createChildItem: %v", err)
	}
	return item
}

// TestWikiLinks_ChildMentionOfParentSuppressed: the headline TASK-1607
// case. A direct child of `parent` mentioning `parent` in its body
// should NOT appear in parent's "Mentioned in" panel — children are
// already listed in the Child Items section above. Symmetric with
// the long-standing self-link suppression.
func TestWikiLinks_ChildMentionOfParentSuppressed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent plan", "")
	// Child links to parent. Without TASK-1607, this would surface
	// as a backlink on parent's page.
	child := createChildItem(t, s, ws.ID, col.ID, parent.ID, "Child task",
		"See [["+refOf(parent)+"]] for context.")

	got, err := s.GetBacklinks(parent.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 backlinks (child filtered), got %d: %+v", len(got), got)
	}

	// CountBacklinks must agree — pagination math in
	// handlers_backlinks.go's same-ws/cross-ws split depends on it.
	n, err := s.CountBacklinks(parent.ID, ws.ID, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("CountBacklinks: %v", err)
	}
	if n != 0 {
		t.Errorf("CountBacklinks: got %d, want 0", n)
	}

	// Sanity: the wiki-link row IS in the index (we only suppress
	// at read time, not at write time — keeps the index complete
	// for future broken-link reports etc.).
	var raw int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE source_item_id = ? AND target_item_id = ?
	`), child.ID, parent.ID).Scan(&raw); err != nil {
		t.Fatalf("raw count: %v", err)
	}
	if raw != 1 {
		t.Errorf("raw wiki_links row: got %d, want 1 (index stays complete)", raw)
	}
}

// TestWikiLinks_ParentMentionOnChildPageSuppressed: the symmetric
// case. When the parent's body mentions its child by wiki-link, that
// mention should NOT appear in the child's "Mentioned in" panel —
// the parent is already on screen in the "Parent: …" header above
// the panel. Requires vis.TargetParentID to be set; the handler
// passes item.ParentID from the resolved target.
func TestWikiLinks_ParentMentionOnChildPageSuppressed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent plan", "")
	child := createChildItem(t, s, ws.ID, col.ID, parent.ID, "Child task", "")

	// Now the parent mentions the child. Without the suppression,
	// this would dominate the child's backlinks panel.
	body := "Tracking work in [[" + refOf(child) + "]]."
	if _, err := s.UpdateItem(parent.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("UpdateItem parent: %v", err)
	}

	// Without TargetParentID: the parent shows up (no suppression).
	plain, err := s.GetBacklinks(child.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks (plain): %v", err)
	}
	if len(plain) != 1 {
		t.Fatalf("baseline: expected 1 backlink (parent mention), got %d", len(plain))
	}

	// With TargetParentID set to the parent's ID: the parent's
	// mention is suppressed.
	pid := parent.ID
	vis := BacklinksVisibility{Unrestricted: true, TargetParentID: &pid}
	got, err := s.GetBacklinks(child.ID, ws.ID, 50, 0, vis)
	if err != nil {
		t.Fatalf("GetBacklinks (suppressed): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 backlinks (parent filtered), got %d: %+v", len(got), got)
	}

	n, err := s.CountBacklinks(child.ID, ws.ID, vis)
	if err != nil {
		t.Fatalf("CountBacklinks: %v", err)
	}
	if n != 0 {
		t.Errorf("CountBacklinks: got %d, want 0", n)
	}
}

// TestWikiLinks_SiblingMentionsNotSuppressed: control case. Sibling
// items (same parent) that wiki-link each other are genuine
// cross-references — they aren't implied by any other UI surface, so
// they must still appear in "Mentioned in".
func TestWikiLinks_SiblingMentionsNotSuppressed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent plan", "")
	siblingA := createChildItem(t, s, ws.ID, col.ID, parent.ID, "Sibling A", "")
	siblingB := createChildItem(t, s, ws.ID, col.ID, parent.ID, "Sibling B",
		"Coordinates with [["+refOf(siblingA)+"]] on the API surface.")

	// siblingA's backlinks should include siblingB. Children-
	// suppression filters by `s.parent_id != targetItemID` — i.e.
	// items whose parent IS siblingA. siblingB's parent is the
	// parent plan, not siblingA, so siblingB is not a child of
	// siblingA and must not be filtered.
	got, err := s.GetBacklinks(siblingA.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sibling backlink, got %d: %+v", len(got), got)
	}
	if got[0].SourceItemID != siblingB.ID {
		t.Errorf("expected sibling B as source, got %s", got[0].SourceItemID)
	}

	// Symmetric for the parent-suppression direction: if siblingA
	// passes TargetParentID=parent.ID (as the handler would since
	// siblingA's parent IS parent), siblingB is still NOT the
	// parent, so it survives.
	pid := parent.ID
	vis := BacklinksVisibility{Unrestricted: true, TargetParentID: &pid}
	got, err = s.GetBacklinks(siblingA.ID, ws.ID, 50, 0, vis)
	if err != nil {
		t.Fatalf("GetBacklinks (with parent vis): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 backlink with parent vis set, got %d", len(got))
	}
}

// TestWikiLinks_OrphanBacklinksUnaffected: regression guard for the
// NULL-safe parent_id predicate. Items with parent_id IS NULL
// (orphan / root-level items) must still surface as backlinks; the
// naive form `s.parent_id != ?` would silently drop them because
// SQL three-valued logic treats `NULL != x` as NULL (not TRUE).
func TestWikiLinks_OrphanBacklinksUnaffected(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, "Target", "")
	// Plain createTestItem produces an orphan item (no parent_id).
	orphan := createTestItem(t, s, ws.ID, col.ID, "Orphan source",
		"Mentions [["+refOf(target)+"]] from the root.")

	got, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, BacklinksVisibility{Unrestricted: true})
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected orphan source to surface, got %d backlinks", len(got))
	}
	if got[0].SourceItemID != orphan.ID {
		t.Errorf("expected orphan as source, got %s", got[0].SourceItemID)
	}
}

// TestWikiLinks_PaginationStableAfterSuppression: the suppression
// applies in SQL, so LIMIT/OFFSET counts the filtered set. Mixing
// suppressed and unsuppressed sources, pages 1 and 2 together must
// equal the full set without skipping or doubling. Regression guard
// for the GetBacklinks/CountBacklinks lockstep that the
// handlers_backlinks.go same-ws/cross-ws math depends on.
func TestWikiLinks_PaginationStableAfterSuppression(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	parent := createTestItem(t, s, ws.ID, col.ID, "Parent plan", "")

	// 3 non-children that mention parent — these should all surface.
	var realRefs []string
	for i := 0; i < 3; i++ {
		src := createTestItem(t, s, ws.ID, col.ID, "Real referrer "+itoa(i),
			"Mentions [["+refOf(parent)+"]].")
		realRefs = append(realRefs, src.ID)
	}
	// 5 children that also mention parent — these should be hidden.
	for i := 0; i < 5; i++ {
		createChildItem(t, s, ws.ID, col.ID, parent.ID,
			"Child "+itoa(i),
			"Refs [["+refOf(parent)+"]].")
	}

	vis := BacklinksVisibility{Unrestricted: true}

	// CountBacklinks must reflect the filtered set, not the raw set.
	n, err := s.CountBacklinks(parent.ID, ws.ID, vis)
	if err != nil {
		t.Fatalf("CountBacklinks: %v", err)
	}
	if n != 3 {
		t.Fatalf("CountBacklinks: got %d, want 3 (3 real, 5 children filtered)", n)
	}

	// Page through with LIMIT=2; combined results must hit all 3
	// real referrers exactly once, in updated_at DESC order.
	page1, err := s.GetBacklinks(parent.ID, ws.ID, 2, 0, vis)
	if err != nil {
		t.Fatalf("GetBacklinks page 1: %v", err)
	}
	page2, err := s.GetBacklinks(parent.ID, ws.ID, 2, 2, vis)
	if err != nil {
		t.Fatalf("GetBacklinks page 2: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page 1: got %d, want 2", len(page1))
	}
	if len(page2) != 1 {
		t.Errorf("page 2: got %d, want 1", len(page2))
	}

	combined := make(map[string]bool)
	for _, bl := range append(page1, page2...) {
		if combined[bl.SourceItemID] {
			t.Errorf("duplicate source across pages: %s", bl.SourceItemID)
		}
		combined[bl.SourceItemID] = true
	}
	if len(combined) != 3 {
		t.Errorf("combined unique sources: got %d, want 3", len(combined))
	}
	for _, want := range realRefs {
		if !combined[want] {
			t.Errorf("missing real referrer %s from combined pages", want)
		}
	}
}

// itoa is strconv.Itoa renamed to keep test bodies readable when
// they're already heavy on ref-formatting.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
