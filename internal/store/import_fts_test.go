package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2758. ImportWorkspace and RemapAttachmentReferencesInWorkspace each ran
// a post-commit rebuildFTSForWorkspace that re-inserted every item of the
// workspace into items_fts, one autocommit write per item. On SQLite items_fts
// is an EXTERNAL-CONTENT table (content='items') kept by the items_fts_*
// triggers, which had already indexed those rows inside the transaction. So
// the rebuild did two harmful things:
//
//   - it double-indexed every imported item, which FTS5's own integrity-check
//     reports as a malformed table. (Measured on the defect: search RESULTS
//     looked right — one hit per item, and an edited-away word stopped
//     matching — so the integrity check is the only instrument here that
//     sees the corruption. Nothing below claims a search-result symptom.);
//   - it turned an N-item import into N extra write transactions after the
//     commit, a burst that stalled every other writer on the database. Under
//     e2e load the stall outlasted the 30 s busy_timeout.
//
// The integrity check is the instrument for the defect. The search legs guard
// the other direction: with the rebuild gone, the TRIGGERS are the only thing
// indexing an imported or remapped row, and a search that finds it (and stops
// finding an edited-away word) is what shows they did. Postgres has no
// items_fts, so these legs are SQLite-only and skip there.

func ftsIntegrity(t *testing.T, s *Store) error {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO items_fts(items_fts, rank) VALUES('integrity-check', 1)`)
	return err
}

func ftsImportBundle(content string) *models.WorkspaceExport {
	const ts = "2026-09-24T12:00:00Z"
	return &models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "FTS import", Slug: "fts-import", Settings: "{}"},
		Collections: []models.CollectionExport{
			{ID: "docs", Name: "Docs", Slug: "docs", Prefix: "DOC", Schema: `{"fields":[]}`, Settings: "{}", CreatedAt: ts, UpdatedAt: ts},
		},
		Items: []models.ItemExport{
			{ID: "old-1", CollectionID: "docs", Title: "First doc", Slug: "first-doc", Content: content, Fields: "{}", Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
			{ID: "old-2", CollectionID: "docs", Title: "Second doc", Slug: "second-doc", Content: "unrelated body", Fields: "{}", Tags: "[]", ItemNumber: 2, CreatedAt: ts, UpdatedAt: ts},
		},
	}
}

func skipUnlessSQLite(t *testing.T, s *Store) {
	t.Helper()
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("items_fts is SQLite-only; Postgres search_vector is a column on items")
	}
}

func TestImportWorkspaceLeavesSearchIndexConsistent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	// CONTROL: the check passes on a store nobody has imported into, so a
	// failure below is about the import rather than about the fixture DB.
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("control: items_fts integrity-check fails before any import: %v", err)
	}

	ws, err := s.ImportWorkspace(ftsImportBundle("the quokkaword lives here"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after ImportWorkspace: %v (the import double-indexed its rows)", err)
	}

	hits, err := s.SearchItems(ws.ID, "quokkaword")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search for an imported word: %d hits, want 1", len(hits))
	}

	// Edit the imported item so the word is gone: the update trigger must
	// reindex a row the insert trigger indexed during the import.
	item, err := s.GetItemBySlug(ws.ID, "first-doc")
	if err != nil || item == nil {
		t.Fatalf("imported item: %v, %v", item, err)
	}
	replaced := "the body was rewritten"
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &replaced}); err != nil {
		t.Fatal(err)
	}
	stale, err := s.SearchItems(ws.ID, "quokkaword")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("search for a word the edit removed: %d hits, want 0", len(stale))
	}
	fresh, err := s.SearchItems(ws.ID, "rewritten")
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("search for the edited word: %d hits, want 1", len(fresh))
	}
}

// The remap is the second caller the rebuild had, and a bundle with
// attachments always reaches it. The import is asserted clean FIRST, so this
// leg fails on the remap alone rather than inheriting the import's damage.
func TestRemapAttachmentReferencesLeavesSearchIndexConsistent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	skipUnlessSQLite(t, s)

	const oldID = "11111111-1111-4111-8111-111111111111"
	const newID = "22222222-2222-4222-8222-222222222222"
	ws, err := s.ImportWorkspace(ftsImportBundle("see ![shot](pad-attachment:"+oldID+") wombatword"), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("precondition: items_fts integrity-check after the import: %v", err)
	}

	if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetItemBySlug(ws.ID, "first-doc")
	if err != nil || item == nil {
		t.Fatalf("imported item: %v, %v", item, err)
	}
	if want := "pad-attachment:" + newID; !strings.Contains(item.Content, want) {
		t.Fatalf("remap did not rewrite the reference; content %q", item.Content)
	}
	if err := ftsIntegrity(t, s); err != nil {
		t.Fatalf("items_fts integrity-check after RemapAttachmentReferencesInWorkspace: %v", err)
	}
	hits, err := s.SearchItems(ws.ID, "wombatword")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("search after the remap: %d hits, want 1", len(hits))
	}
}
