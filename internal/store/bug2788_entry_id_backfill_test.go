package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2788: the startup backfill persists an id on every stored entry that
// lacks a usable, unique one, SILENTLY (no seq, no updated_at, no outbox
// event; lead-ruled), and an item it repaired stays writable through the
// doors the Q1 census found in use: a fields_patch update and an append.

func storedNoteIDs(t *testing.T, s *Store, itemID string) []string {
	t.Helper()
	it, err := s.GetItem(itemID)
	if err != nil || it == nil {
		t.Fatalf("GetItem: %v", err)
	}
	var ids []string
	for _, n := range models.ExtractItemImplementationNotes(it.Fields) {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestBackfillStructuredEntryIDs(t *testing.T) {
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Backfill"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "legacy", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	clean, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "clean", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem clean: %v", err)
	}
	// Legacy blobs, written the way pre-BUG-3163 writes and imports left them:
	// an idless note and a duplicated id; the clean item already has ids.
	legacy := `{"status":"open","implementation_notes":[{"summary":"idless"},{"id":"d","summary":"first"},{"id":"d","summary":"second"}]}`
	cleanBlob := `{"status":"open","implementation_notes":[{"id":"n1","summary":"ok"}]}`
	for id, blob := range map[string]string{it.ID: legacy, clean.ID: cleanBlob} {
		if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), blob, id); err != nil {
			t.Fatalf("seed legacy blob: %v", err)
		}
	}
	before, err := s.GetItem(it.ID)
	if err != nil || before == nil {
		t.Fatalf("GetItem: %v", err)
	}
	cleanBefore, _ := s.GetItem(clean.ID)
	var outboxBefore int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), it.ID).Scan(&outboxBefore); err != nil {
		t.Fatalf("count outbox: %v", err)
	}

	res, err := s.BackfillStructuredEntryIDs()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.RowsRepaired != 1 {
		t.Errorf("RowsRepaired = %d, want 1 (only the legacy row)", res.RowsRepaired)
	}

	after, _ := s.GetItem(it.ID)
	ids := storedNoteIDs(t, s, it.ID)
	if len(ids) != 3 || ids[0] == "" || ids[1] != "d" || ids[2] == "d" || ids[2] == "" {
		t.Errorf("repaired ids = %q, want [minted d minted]", ids)
	}
	// Silent, by ruling.
	if after.Seq != before.Seq {
		t.Errorf("seq moved %d → %d; the backfill must not bump it", before.Seq, after.Seq)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved %v → %v", before.UpdatedAt, after.UpdatedAt)
	}
	var outboxAfter int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`), it.ID).Scan(&outboxAfter); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxAfter != outboxBefore {
		t.Errorf("outbox rows for the item %d → %d; the backfill must emit nothing", outboxBefore, outboxAfter)
	}
	// Content other than the ids is untouched.
	var got map[string]any
	if err := json.Unmarshal([]byte(after.Fields), &got); err != nil || got["status"] != "open" {
		t.Errorf("fields after repair = %s", after.Fields)
	}
	// A clean row is not rewritten at all.
	if cleanAfter, _ := s.GetItem(clean.ID); cleanAfter.Fields != cleanBefore.Fields {
		t.Errorf("a clean row was rewritten: %s → %s", cleanBefore.Fields, cleanAfter.Fields)
	}

	// Idempotent.
	again, err := s.BackfillStructuredEntryIDs()
	if err != nil || again.RowsRepaired != 0 {
		t.Errorf("second run: repaired=%d err=%v, want 0", again.RowsRepaired, err)
	}

	// The lead's pin: the repaired item stays writable through the doors the
	// census found in use.
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
		t.Errorf("fields_patch update after the backfill: %v", err)
	}
	note := models.ItemImplementationNote{ID: models.NewStructuredEntryID("note"), Summary: "appended"}
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{ImplementationNoteToAppend: &note}); err != nil {
		t.Errorf("append after the backfill: %v", err)
	}
	final := storedNoteIDs(t, s, it.ID)
	if len(final) != 4 || strings.Join(final[:3], ",") != strings.Join(ids, ",") {
		t.Errorf("after append ids = %q, want the three repaired ids unchanged plus one", final)
	}
}

// Workspace import is the one live door that brings entries in as written
// elsewhere; it persists ids before the row exists.
func TestImportWorkspacePersistsStructuredEntryIDs(t *testing.T) {
	s := testStore(t)
	src, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Src"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	it, err := s.CreateItem(src.ID, coll.ID, models.ItemCreate{Title: "carried", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), `{"implementation_notes":[{"summary":"idless"}]}`, it.ID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	export, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	dest, err := s.ImportWorkspace(export, "Imported", "", "test")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	items, err := s.ListItems(dest.ID, models.ItemListParams{})
	if err != nil || len(items) == 0 {
		t.Fatalf("ListItems: %v (%d)", err, len(items))
	}
	full, _ := s.GetItem(items[0].ID)
	notes := models.ExtractItemImplementationNotes(full.Fields)
	if len(notes) != 1 || notes[0].ID == "" {
		t.Errorf("imported notes = %+v, want one with a persisted id", notes)
	}
}
