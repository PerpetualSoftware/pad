package store

import (
	"strings"
	"testing"

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

	backlinks, err := s.GetBacklinks(target.ID, ws.ID, 50, 0)
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
	got, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0)
	if len(got) != 1 {
		t.Fatalf("step 1: expected A to have 1 backlink, got %d", len(got))
	}

	// Rewrite body to mention B instead. Expect: A loses its
	// backlink, B gains one.
	newContent := "Now mentions [[" + refOf(b) + "]] instead."
	if _, err := s.UpdateItem(source.ID, models.ItemUpdate{Content: &newContent}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if got, _ := s.GetBacklinks(a.ID, ws.ID, 50, 0); len(got) != 0 {
		t.Errorf("step 2: A should have 0 backlinks after rewrite, got %d", len(got))
	}
	if got, _ := s.GetBacklinks(b.ID, ws.ID, 50, 0); len(got) != 1 {
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

	if got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0); len(got) != 1 {
		t.Fatalf("baseline: expected 1 backlink, got %d", len(got))
	}

	// Soft-delete the source. Backlinks should hide it via the
	// items.deleted_at IS NULL filter even though the row is still
	// in item_wiki_links.
	if err := s.DeleteItem(source.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if got, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0); len(got) != 0 {
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
	got, err := s.GetBacklinks(self.ID, ws.ID, 50, 0)
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
	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0)
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

	bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0)
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
	bls, _ := s.GetBacklinks(target.ID, ws.ID, 50, 0)
	if len(bls) != 1 {
		t.Errorf("after backfill: expected 1 backlink, got %d", len(bls))
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
