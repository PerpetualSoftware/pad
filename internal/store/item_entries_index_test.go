package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4491: item_entries is a trigger-maintained index of the notes and
// decisions inside items.fields. These tests pin it to the Go extraction the
// rest of the product reads, through every writer of items.fields.

type itemEntryRow struct {
	Kind    string
	Index   int
	EntryID string
	Head    string
	Body    string
}

var testEntrySeq atomic.Int64

// testEntryID mints a unique entry id. NewStructuredEntryID is unix-nano
// based, and two appends in the same nanosecond would be repaired to a new id
// by EnsureStructuredEntryIDs, so a test that compares ids uses its own.
func testEntryID(prefix string) string {
	return fmt.Sprintf("%s-test-%d", prefix, testEntrySeq.Add(1))
}

func appendTestNote(t *testing.T, s *Store, itemID, summary, details string) string {
	t.Helper()
	id := testEntryID("note")
	if _, err := s.UpdateItem(itemID, models.ItemUpdate{ImplementationNoteToAppend: &models.ItemImplementationNote{ID: id, Summary: summary, Details: details}}); err != nil {
		t.Fatalf("append note: %v", err)
	}
	return id
}

func appendTestDecision(t *testing.T, s *Store, itemID, decision, rationale string) string {
	t.Helper()
	id := testEntryID("decision")
	if _, err := s.UpdateItem(itemID, models.ItemUpdate{DecisionToAppend: &models.ItemDecisionLogEntry{ID: id, Decision: decision, Rationale: rationale}}); err != nil {
		t.Fatalf("append decision: %v", err)
	}
	return id
}

func storedItemEntries(t *testing.T, s *Store, itemID string) []itemEntryRow {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT kind, entry_index, entry_id, head, body FROM item_entries WHERE item_id = ? ORDER BY kind, entry_index`), itemID)
	if err != nil {
		t.Fatalf("read item_entries: %v", err)
	}
	defer rows.Close()
	out := []itemEntryRow{}
	for rows.Next() {
		var r itemEntryRow
		if err := rows.Scan(&r.Kind, &r.Index, &r.EntryID, &r.Head, &r.Body); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func storedItemEntryIDs(t *testing.T, s *Store, itemID string) []int64 {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT id FROM item_entries WHERE item_id = ? ORDER BY kind, entry_index`), itemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// goItemEntries is what the Go extraction says the index must hold.
func goItemEntries(fields string) []itemEntryRow {
	out := []itemEntryRow{}
	for i, d := range models.ExtractItemDecisionLog(fields) {
		out = append(out, itemEntryRow{"decision", i, d.ID, d.Decision, d.Rationale})
	}
	for i, n := range models.ExtractItemImplementationNotes(fields) {
		out = append(out, itemEntryRow{"note", i, n.ID, n.Summary, n.Details})
	}
	return out
}

func storedFields(t *testing.T, s *Store, itemID string) string {
	t.Helper()
	var fields string
	if err := s.db.QueryRow(s.q(`SELECT CAST(fields AS TEXT) FROM items WHERE id = ?`), itemID).Scan(&fields); err != nil {
		t.Fatalf("read fields: %v", err)
	}
	return fields
}

// assertEntriesMatchGo fails unless the index equals the Go extraction of the
// stored fields, and the stored fields hold at least want entries (so a
// writer that silently dropped the log cannot pass on two empty sets).
func assertEntriesMatchGo(t *testing.T, s *Store, itemID string, want int) {
	t.Helper()
	fields := storedFields(t, s, itemID)
	expect := goItemEntries(fields)
	got := storedItemEntries(t, s, itemID)
	if len(expect) < want {
		t.Fatalf("fixture: stored fields carry %d entries, want at least %d: %s", len(expect), want, fields)
	}
	if !reflect.DeepEqual(got, expect) {
		t.Errorf("item_entries disagree with the Go extraction\n got: %+v\nwant: %+v", got, expect)
	}
}

const entriesFixtureFields = `{"status":"open","implementation_notes":[{"id":"n-a","summary":"first summary","details":"first details"},{"id":"n-b","summary":"second summary"}],"decision_log":[{"id":"d-a","decision":"pick sqlite","rationale":"it is embedded"}]}`

func TestItemEntriesMatchGoExtraction(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries parity")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	other := createTestCollection(t, s, ws.ID, "Other")
	newItem := func(t *testing.T, title string) *models.Item {
		t.Helper()
		it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title, Fields: entriesFixtureFields})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}

	// items.go:378 (CreateItem's INSERT).
	t.Run("create", func(t *testing.T) {
		it := newItem(t, "create")
		assertEntriesMatchGo(t, s, it.ID, 3)
	})
	// items.go:3628 (UpdateItem's dynamic UPDATE): an append, a full fields
	// replace and a fields patch.
	t.Run("update append", func(t *testing.T) {
		it := newItem(t, "update append")
		appendTestNote(t, s, it.ID, "third summary", "third details")
		appendTestDecision(t, s, it.ID, "second decision", "")
		assertEntriesMatchGo(t, s, it.ID, 5)
	})
	t.Run("update replace", func(t *testing.T) {
		it := newItem(t, "update replace")
		next := `{"status":"open","implementation_notes":[{"id":"n-b","summary":"second summary"}]}`
		if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: &next}); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, it.ID, 1)
	})
	t.Run("update patch", func(t *testing.T) {
		it := newItem(t, "update patch")
		if _, err := s.UpdateItem(it.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, it.ID, 3)
	})
	// items.go:6249 (MoveItem).
	t.Run("move", func(t *testing.T) {
		it := newItem(t, "move")
		if _, err := s.MoveItem(it.ID, other.ID, entriesFixtureFields); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, it.ID, 3)
	})
	// export.go:1402 (import INSERT) and export.go:1531 (the import's
	// second pass, which rewrites every inserted item's fields).
	t.Run("import", func(t *testing.T) {
		src := createTestWorkspace(t, s, "Entries import source")
		srcCol := createTestCollection(t, s, src.ID, "Tasks")
		for _, title := range []string{"first", "second"} {
			if _, err := s.CreateItem(src.ID, srcCol.ID, models.ItemCreate{Title: title, Fields: entriesFixtureFields}); err != nil {
				t.Fatal(err)
			}
		}
		exp, err := s.ExportWorkspace(src.Slug)
		if err != nil {
			t.Fatal(err)
		}
		dst, err := s.ImportWorkspace(exp, "Entries imported", "", "test")
		if err != nil {
			t.Fatal(err)
		}
		items, err := s.ListItems(dst.ID, models.ItemListParams{})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("imported %d items, want 2", len(items))
		}
		for _, it := range items {
			assertEntriesMatchGo(t, s, it.ID, 3)
		}
	})
	// The cross-workspace copy inserts through createItemTx (items.go:378).
	t.Run("copy", func(t *testing.T) {
		it := newItem(t, "copy")
		dst := createTestWorkspace(t, s, "Entries copy target")
		dstCol := createTestCollection(t, s, dst.ID, "Tasks")
		u, err := s.CreateUser(models.UserCreate{Email: "copy-entries@test.com", Name: "Copier", Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{SourceItemID: it.ID, TargetWorkspaceID: dst.ID, TargetCollectionID: dstCol.ID, Actor: u.ID})
		if err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, res.Item.ID, 3)
	})
	// fenced_items.go:257 (app create) and fenced_items.go:362 (app fields
	// update).
	t.Run("fenced create and update", func(t *testing.T) {
		f := newFenceFixture(t)
		u, err := f.s.CreateUser(models.UserCreate{Email: "fence-entries@test.com", Name: "Fence", Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		actor := FencedActor{Kind: "user", UserID: u.ID}
		ftx, err := f.s.BeginFenced(context.Background(), f.spec())
		if err != nil {
			t.Fatal(err)
		}
		it, err := ftx.CreateItem(FencedItemCreate{CollectionID: f.companion.ID, Title: "app", Fields: entriesFixtureFields, Actor: actor})
		if err != nil {
			_ = ftx.Rollback()
			t.Fatal(err)
		}
		if err := ftx.Commit(); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, f.s, it.ID, 3)

		ftx, err = f.s.BeginFenced(context.Background(), f.spec())
		if err != nil {
			t.Fatal(err)
		}
		key := []byte("etag-key")
		notes := []any{map[string]any{"id": "n-z", "summary": "app summary", "details": "app details"}}
		if _, err := ftx.UpdateItemFields(it.ID, map[string]any{"implementation_notes": notes}, AppItemETag(key, f.install, it.ID, it.Seq), key, actor); err != nil {
			_ = ftx.Rollback()
			t.Fatal(err)
		}
		if err := ftx.Commit(); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, f.s, it.ID, 2)
		if got := storedItemEntries(t, f.s, it.ID); len(got) != 2 || got[1].EntryID != "n-z" {
			t.Errorf("fenced update not indexed: %+v", got)
		}
	})
	// attachments_remap.go:529: a reference inside a note is rewritten.
	t.Run("attachment remap", func(t *testing.T) {
		oldID, newID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
		fields := `{"status":"open","implementation_notes":[{"id":"n-r","summary":"see","details":"![img](pad-attachment:` + oldID + `)"}]}`
		it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "remap", Fields: fields})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RemapAttachmentReferencesInWorkspace(ws.ID, map[string]string{oldID: newID}); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, it.ID, 1)
		if got := storedItemEntries(t, s, it.ID); len(got) != 1 || !strings.Contains(got[0].Body, newID) {
			t.Errorf("remapped note not reindexed: %+v", got)
		}
	})
	// structured_entry_id_backfill.go:104.
	t.Run("entry id backfill", func(t *testing.T) {
		it := newItem(t, "backfill")
		s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","implementation_notes":[{"summary":"idless"}]}`, it.ID)
		if _, err := s.BackfillStructuredEntryIDs(); err != nil {
			t.Fatal(err)
		}
		assertEntriesMatchGo(t, s, it.ID, 1)
	})
	// collections.go:1130 (scalar option rename) and field_option_rename.go:80
	// (multi_select option rename), through UpdateCollection's migrations.
	t.Run("option renames", func(t *testing.T) {
		schema := func(opts string) string {
			return `{"fields":[{"key":"status","label":"Status","type":"select","options":` + opts + `},{"key":"labels","label":"Labels","type":"multi_select","options":` + opts + `}]}`
		}
		c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Renames", Schema: schema(`["open","done"]`)})
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Replace(entriesFixtureFields, `"status":"open"`, `"status":"open","labels":["open"]`, 1)
		it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: "rename", Fields: fields})
		if err != nil {
			t.Fatal(err)
		}
		before := storedItemEntryIDs(t, s, it.ID)
		next := schema(`["todo","done"]`)
		if _, err := s.UpdateCollection(c.ID, models.CollectionUpdate{Schema: &next, Migrations: []models.FieldMigration{
			{Field: "status", RenameOptions: map[string]string{"open": "todo"}},
			{Field: "labels", RenameOptions: map[string]string{"open": "todo"}},
		}}); err != nil {
			t.Fatal(err)
		}
		if f := storedFields(t, s, it.ID); !strings.Contains(f, `"todo"`) || strings.Contains(f, `"open"`) {
			t.Fatalf("fixture: renames did not land: %s", f)
		}
		assertEntriesMatchGo(t, s, it.ID, 3)
		if after := storedItemEntryIDs(t, s, it.ID); !reflect.DeepEqual(before, after) {
			t.Errorf("a rename that leaves the logs alone rewrote rows: %v -> %v", before, after)
		}
	})
}

func TestItemEntriesAppendTouchesOnlyNewRow(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries append")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Appends", "")
	for i := 0; i < 3; i++ {
		appendTestNote(t, s, it.ID, fmt.Sprintf("note %d", i), "")
	}
	before := storedItemEntryIDs(t, s, it.ID)
	if len(before) != 3 {
		t.Fatalf("rows after three appends: %v", before)
	}
	appendTestNote(t, s, it.ID, "note 3", "")
	after := storedItemEntryIDs(t, s, it.ID)
	if len(after) != 4 || !reflect.DeepEqual(after[:3], before) {
		t.Errorf("a fourth append rewrote earlier rows: %v -> %v", before, after)
	}
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
		t.Fatal(err)
	}
	if got := storedItemEntryIDs(t, s, it.ID); !reflect.DeepEqual(got, after) {
		t.Errorf("a status patch changed rows: %v -> %v", after, got)
	}
}

func TestItemEntriesRewriteAndRemoveKey(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries rewrite")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Rewrite", Fields: `{"status":"open","implementation_notes":[{"id":"n1","summary":"oldwordzeta"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	search := func(q string) int {
		res, err := s.Search(SearchParams{Query: q, Workspace: ws.Slug, Unrestricted: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.Total
	}
	if search("oldwordzeta") != 1 {
		t.Fatal("fixture: the original summary does not match")
	}
	next := `{"status":"open","implementation_notes":[{"id":"n1","summary":"newwordkappa"}]}`
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: &next}); err != nil {
		t.Fatal(err)
	}
	if n := search("oldwordzeta"); n != 0 {
		t.Errorf("an edited summary still matches its old text (%d)", n)
	}
	if n := search("newwordkappa"); n != 1 {
		t.Errorf("an edited summary does not match its new text (%d)", n)
	}
	gone := `{"status":"open"}`
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: &gone}); err != nil {
		t.Fatal(err)
	}
	if rows := storedItemEntries(t, s, it.ID); len(rows) != 0 {
		t.Errorf("removing the key left rows: %+v", rows)
	}
}

func TestItemEntriesMalformed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries malformed")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Malformed", "")
	cases := []struct {
		name, fields string
		want         []itemEntryRow
	}{
		{"not an array", `{"implementation_notes":"text","decision_log":{"a":1}}`, []itemEntryRow{}},
		{"non-object elements", `{"implementation_notes":["s",3,null,[1],{"id":"n1","summary":"kept"}]}`, []itemEntryRow{{"note", 4, "n1", "kept", ""}}},
		{"non-string members", `{"decision_log":[{"id":7,"decision":["x"],"rationale":"why"}]}`, []itemEntryRow{{"decision", 0, "", "", "why"}}},
		{"fields not an object", `[1,2]`, []itemEntryRow{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), c.fields, it.ID); err != nil {
				t.Fatalf("the write was refused: %v", err)
			}
			if got := storedItemEntries(t, s, it.ID); !reflect.DeepEqual(got, c.want) {
				t.Errorf("rows = %+v, want %+v", got, c.want)
			}
		})
	}
}

// An object entry replaced in place by a non-object reaches the update
// trigger's DELETE subquery, whose join evaluates json_type on je.value. On a
// bare string element that call would raise malformed JSON if the
// je.type = 'object' test did not run first, so each case must land.
func TestItemEntriesObjectReplacedByNonObject(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries replaced")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Replaced", "")
	const tail = `{"id":"n1","summary":"kept note"}`
	const dtail = `{"id":"d1","decision":"kept decision"}`
	objects := `{"implementation_notes":[{"id":"n0","summary":"first note"},` + tail + `],"decision_log":[{"id":"d0","decision":"first decision"},` + dtail + `]}`
	for _, c := range []struct{ name, elem string }{
		{"string", `"plain text"`},
		{"number", `3`},
		{"broken JSON string", `"{\"id\":"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), objects, it.ID); err != nil {
				t.Fatalf("fixture write: %v", err)
			}
			if got := storedItemEntries(t, s, it.ID); len(got) != 4 {
				t.Fatalf("fixture: want 4 rows before the replacement, got %+v", got)
			}
			replaced := `{"implementation_notes":[` + c.elem + `,` + tail + `],"decision_log":[` + c.elem + `,` + dtail + `]}`
			if _, err := s.db.Exec(s.q(`UPDATE items SET fields = ? WHERE id = ?`), replaced, it.ID); err != nil {
				t.Fatalf("the write was refused: %v", err)
			}
			want := []itemEntryRow{{"decision", 1, "d1", "kept decision", ""}, {"note", 1, "n1", "kept note", ""}}
			if got := storedItemEntries(t, s, it.ID); !reflect.DeepEqual(got, want) {
				t.Errorf("rows = %+v, want %+v", got, want)
			}
		})
	}
}

// A legacy row can hold a NUL that items.fields now refuses (084). The index
// cuts each value at its first NUL, so the NUL triggers on item_entries (185) never refuse that row's
// next write (condition 1). The items.fields triggers are dropped to write
// the legacy shape, as TestTimeline_NeverEmitsACursorItWouldRefuse does.
func TestItemEntriesLegacyNULDoesNotRefuseTheWrite(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Postgres jsonb cannot hold a NUL; the NUL layer is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "Entries NUL")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Legacy", "")
	for _, tr := range []string{"pad_nul_items_fields_ins", "pad_nul_items_fields_upd"} {
		s.mustExec(t, "DROP TRIGGER IF EXISTS "+tr)
	}
	s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","implementation_notes":[{"id":"nPLACEHOLDER1","summary":"lePLACEHOLDERgacy","details":"dPLACEHOLDERx"}]}`, it.ID)
	// The escape goes in as a bare six-character parameter, which the
	// guarded driver admits, as in the timeline test.
	if _, err := s.db.Exec(`UPDATE items SET fields = REPLACE(fields, 'PLACEHOLDER', ?) WHERE id = ?`, `\u0000`, it.ID); err != nil {
		t.Fatalf("the index refused a legacy write: %v", err)
	}
	if f := storedFields(t, s, it.ID); !strings.Contains(f, `\u0000`) {
		t.Fatalf("fixture: no NUL escape stored: %s", f)
	}
	// Each value is cut at its first NUL.
	want := []itemEntryRow{{"note", 0, "n", "le", "d"}}
	if got := storedItemEntries(t, s, it.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
}

func TestItemEntriesLifecycle(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries lifecycle")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Lifecycle", "")
	appendTestNote(t, s, it.ID, "lifecycle note", "")
	if err := s.DeleteItem(it.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(storedItemEntries(t, s, it.ID)); n != 1 {
		t.Errorf("soft delete changed rows: %d", n)
	}
	if _, err := s.RestoreItem(it.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(storedItemEntries(t, s, it.ID)); n != 1 {
		t.Errorf("restore changed rows: %d", n)
	}
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeWorkspaceData(ws.ID); err != nil {
		t.Fatal(err)
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM item_entries WHERE item_id = ?`, it.ID); n != 0 {
		t.Errorf("purge left %d rows", n)
	}
	if s.dialect.Driver() == DriverSQLite {
		s.mustExec(t, `INSERT INTO item_entries_fts(item_entries_fts) VALUES('integrity-check')`)
	}
}

func TestItemEntriesMove(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries move")
	from := createTestCollection(t, s, ws.ID, "Tasks")
	to := createTestCollection(t, s, ws.ID, "Ideas")
	it := createTestItem(t, s, ws.ID, from.ID, "Mover", "")
	appendTestNote(t, s, it.ID, "movingwordomega", "")
	if _, err := s.MoveItem(it.ID, to.ID, storedFields(t, s, it.ID)); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]int{from.Slug: 0, to.Slug: 1} {
		res, err := s.Search(SearchParams{Query: "movingwordomega", Workspace: ws.Slug, Unrestricted: true, Collection: slug})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != want {
			t.Errorf("collection %s: total %d, want %d", slug, res.Total, want)
		}
	}
}

func TestItemEntriesFollowIDBackfill(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries backfill")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Idless", "")
	s.mustExec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","implementation_notes":[{"summary":"idlesswordsigma"}]}`, it.ID)
	if rows := storedItemEntries(t, s, it.ID); len(rows) != 1 || rows[0].EntryID != "" {
		t.Fatalf("an idless note: %+v", rows)
	}
	if _, err := s.BackfillStructuredEntryIDs(); err != nil {
		t.Fatal(err)
	}
	minted := storedNoteIDs(t, s, it.ID)
	if len(minted) != 1 || minted[0] == "" {
		t.Fatalf("backfill minted %v", minted)
	}
	res, err := s.Search(SearchParams{Query: "idlesswordsigma", Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].EntryID != minted[0] {
		t.Errorf("search entry_id after backfill: %+v, want %s", res.Results, minted[0])
	}
}

// Condition 1 (TASK-4491): the index never refuses an item write. The largest
// fields an item write accepts is the 2 MiB JSON body cap
// (defaultJSONBodyLimit, internal/server/server.go:3212), so one note of
// almost that size, all distinct words, must land, be indexed, and be
// searchable. On Postgres an uncapped to_tsvector of that input exceeds the
// 1 MB tsvector limit and raises; the trigger caps its input (pgmigrations
// 144).
func TestItemEntriesLargeNoteIsIndexed(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Entries large")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	it := createTestItem(t, s, ws.ID, col.ID, "Large", "")
	details := largeDistinctWords(2<<20 - 4096)
	if _, err := s.UpdateItem(it.ID, models.ItemUpdate{ImplementationNoteToAppend: &models.ItemImplementationNote{ID: "note-large", Summary: "hugewordlambda summary", Details: details}}); err != nil {
		t.Fatalf("a note at the body cap was refused: %v", err)
	}
	rows := storedItemEntries(t, s, it.ID)
	if len(rows) != 1 || len(rows[0].Body) != len(details) {
		t.Fatalf("large note not indexed whole: %d rows", len(rows))
	}
	for _, q := range []string{"hugewordlambda", "aaab"} {
		res, err := s.Search(SearchParams{Query: q, Workspace: ws.Slug, Unrestricted: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 1 || res.Results[0].EntryID != "note-large" {
			t.Errorf("%s: large note not searchable: %+v", q, res)
		}
	}
}

// largeDistinctWords returns about n bytes of space-separated distinct
// lowercase words, the input that makes the largest tsvector per byte.
func largeDistinctWords(n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for i := 0; b.Len() < n; i++ {
		w := []byte{'a', 'a', 'a', 'a'}
		for j, k := 3, i; j >= 0; j, k = j-1, k/26 {
			w[j] = byte('a' + k%26)
		}
		b.Write(w)
		b.WriteByte(' ')
		if i >= 26*26*26*26-1 {
			i = -1
		}
	}
	return b.String()
}
