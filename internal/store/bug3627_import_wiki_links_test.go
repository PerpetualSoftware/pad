package store

import "testing"

// BUG-3627: an imported workspace must have its wiki-link index at once.
// Import inserted items without indexing their [[links]], and only the
// server-start backfill filled item_wiki_links, so backlinks and the graph
// stayed empty until the next restart (days, on Cloud).
func TestBUG3627_ImportIndexesWikiLinks(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Linked Source")
	coll := createTestCollection(t, s, ws.ID, "Notes")
	// "Early Note" links forward to an item created after it, so in the
	// bundle the link precedes its target: it resolves only if indexing runs
	// once every item exists.
	createTestItem(t, s, ws.ID, coll.ID, "Early Note", "points ahead to [[Target Note]]")
	createTestItem(t, s, ws.ID, coll.ID, "Target Note", "the thing linked to")
	createTestItem(t, s, ws.ID, coll.ID, "Source Note", "see [[Target Note]] for details")

	// Control: in the SOURCE workspace the link is indexed (the ordinary
	// create path does it), so a missing row after import is import's doing.
	var srcLinks int
	if err := s.DB().QueryRow(s.q(`SELECT COUNT(*) FROM item_wiki_links wl JOIN items i ON i.id = wl.source_item_id WHERE i.workspace_id = ? AND wl.target_item_id IS NOT NULL`), ws.ID).Scan(&srcLinks); err != nil {
		t.Fatalf("count source links: %v", err)
	}
	// Early Note's forward link was unresolved when it was created (its target
	// did not exist yet) and is resolved later by the target's creation, or
	// not: either way the import must resolve it, so the source count is the
	// floor, not the expected import count.
	if srcLinks < 1 {
		t.Fatalf("fixture: the source workspace holds %d resolved wiki-link rows, want at least 1", srcLinks)
	}

	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	owner := createTestUser(t, s, "bug3627@example.com", "Importer", "pw-test-12345")
	imported, err := s.ImportWorkspace(data, "Linked Copy", owner.ID, "import")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}

	var targetID, sourceID string
	if err := s.DB().QueryRow(s.q(`SELECT id FROM items WHERE workspace_id = ? AND title = ?`), imported.ID, "Target Note").Scan(&targetID); err != nil {
		t.Fatalf("find imported target: %v", err)
	}
	if err := s.DB().QueryRow(s.q(`SELECT id FROM items WHERE workspace_id = ? AND title = ?`), imported.ID, "Source Note").Scan(&sourceID); err != nil {
		t.Fatalf("find imported source: %v", err)
	}

	var earlyID string
	if err := s.DB().QueryRow(s.q(`SELECT id FROM items WHERE workspace_id = ? AND title = ?`), imported.ID, "Early Note").Scan(&earlyID); err != nil {
		t.Fatalf("find imported early note: %v", err)
	}
	for _, src := range []struct{ name, id string }{{"Source Note", sourceID}, {"Early Note (forward link)", earlyID}} {
		var n int
		if err := s.DB().QueryRow(s.q(`SELECT COUNT(*) FROM item_wiki_links WHERE source_item_id = ? AND target_item_id = ?`), src.id, targetID).Scan(&n); err != nil {
			t.Fatalf("count links: %v", err)
		}
		if n != 1 {
			t.Errorf("%s: imported [[Target Note]] link rows = %d, want 1: import left the wiki-link index empty until a restart's backfill", src.name, n)
		}
	}
}
