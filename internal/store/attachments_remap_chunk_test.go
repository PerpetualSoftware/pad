package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// A workspace small enough for one chunk must come out byte-identical to what
// the single-transaction remap produced. The golden file was captured from
// that code (see remapGoldenPath), so this is a comparison against the OLD
// function's output, not against the new one's own idea of correct.
func TestRemap_SingleChunkEquivalence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	wsID := remapEquivalenceFixture(t, s)
	before := dumpRemapState(t, s, wsID)
	if err := s.RemapAttachmentReferencesInWorkspace(wsID, remapFixtureMap()); err != nil {
		t.Fatalf("remap: %v", err)
	}
	got := dumpRemapState(t, s, wsID)
	if reflect.DeepEqual(before, got) {
		t.Fatal("the remap changed nothing; the fixture is vacuous")
	}
	if os.Getenv("PAD_UPDATE_REMAP_GOLDEN") == "1" {
		writeRemapGolden(t, got)
		t.Skip("golden rewritten")
	}
	raw, err := os.ReadFile(remapGoldenPath())
	if err != nil {
		t.Fatal(err)
	}
	var want remapDump
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		gj, _ := json.MarshalIndent(got, "", " ")
		wj, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("single-chunk remap differs from the single-transaction golden\n--- want\n%s\n--- got\n%s", wj, gj)
	}
}

// ---------------------------------------------------------------------------
// Chunked remap (TASK-4278). Every test builds its fixture through the
// store, counts effects with raw SQL, and on Postgres first proves the backend
// is a real PostgreSQL (requireRealPostgres: SHOW server_version).
// ---------------------------------------------------------------------------

// remapStore is testStore plus the backend proof when it is Postgres.
func remapStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if s.dialect.Driver() == DriverPostgres {
		requireRealPostgres(t, s)
	}
	return s
}

// fakeRemapClock advances by step on EVERY reading, so a chunk's boundary
// depends on how many times the code looked at the clock, not on the box.
func fakeRemapClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	cur := time.Unix(1_800_000_000, 0)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(step)
		return cur
	}
}

type remapWorldItem struct {
	id, title, oldID, newID, commentID string
}

type remapWorld struct {
	s     *Store
	wsID  string
	colID string
	items []remapWorldItem
	oldTo map[string]string
}

func createCloneAttachment(t *testing.T, s *Store, wsID string) string {
	t.Helper()
	variant := "original"
	a := &models.Attachment{
		ID: newID(), WorkspaceID: wsID, StorageKey: "fs:" + newID(), ContentHash: "hash-" + newID(),
		Filename: "clone.png", MimeType: "image/png", SizeBytes: 1, Variant: &variant,
	}
	if err := s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	return a.ID
}

// buildRemapWorld makes n items, each with its OWN source id and its own
// destination clone (an attachments row with no stamp), referenced from the
// item's content, its fields, a comment and a two-row version chain (an older
// full row and a newer reverse patch). One extra soft-deleted item carries only
// a comment reference. So every surface the remap rewrites is present per item,
// and which attachment a text points at says which item it belongs to.
func buildRemapWorld(t *testing.T, s *Store, n int) *remapWorld {
	t.Helper()
	ws := createTestWorkspace(t, s, "Remap World")
	col := createTestCollection(t, s, ws.ID, "Docs")
	w := &remapWorld{s: s, wsID: ws.ID, colID: col.ID, oldTo: map[string]string{}}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(s.q(q), args...); err != nil {
			t.Fatalf("fixture exec %q: %v", q, err)
		}
	}
	for i := 0; i < n; i++ {
		oldID, newID := newID(), createCloneAttachment(t, s, ws.ID)
		w.oldTo[oldID] = newID
		content := fmt.Sprintf("current body %d %s", i, remapRef(oldID))
		it := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("item-%03d", i), content)
		exec(`UPDATE items SET fields = ? WHERE id = ?`, `{"status":"open","cover":"pad-attachment:`+oldID+`"}`, it.ID)
		exec(`DELETE FROM item_versions WHERE item_id = ?`, it.ID)
		prev := fmt.Sprintf("earlier body %d %s", i, remapRef(oldID))
		exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("ver-%03d-1", i), it.ID, "first body "+remapRef(oldID), s.dialect.BoolToInt(false), "2026-01-01T00:00:00Z", 1)
		exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("ver-%03d-2", i), it.ID, diff.CreateReversePatch(prev, content), s.dialect.BoolToInt(true), "2026-01-02T00:00:00Z", 2)
		cid := fmt.Sprintf("cm-%03d", i)
		exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			cid, it.ID, ws.ID, "comment "+remapRef(oldID), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		w.items = append(w.items, remapWorldItem{id: it.ID, title: it.Title, oldID: oldID, newID: newID, commentID: cid})
	}
	// A soft-deleted item: its comment is rewritten, its body is not.
	oldID, newID := newID(), createCloneAttachment(t, s, ws.ID)
	w.oldTo[oldID] = newID
	dead := createTestItem(t, s, ws.ID, col.ID, "item-dead", "dead body "+remapRef(oldID))
	exec(`UPDATE items SET deleted_at = ? WHERE id = ?`, "2026-01-05T00:00:00Z", dead.ID)
	exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"cm-dead", dead.ID, ws.ID, "dead comment "+remapRef(oldID), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	w.items = append(w.items, remapWorldItem{id: dead.ID, title: dead.Title, oldID: oldID, newID: newID, commentID: "cm-dead"})
	return w
}

// snapshot is every remap-relevant column of the workspace, raw: items
// (content, fields, seq), comments, version rows, and each attachment's stamp.
func (w *remapWorld) snapshot(t *testing.T) map[string]string {
	t.Helper()
	s := w.s
	out := map[string]string{}
	rows, err := s.db.Query(s.q(`SELECT id, content, fields, seq FROM items WHERE workspace_id = ?`), w.wsID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, content, fields string
		var seq int64
		if err := rows.Scan(&id, &content, &fields, &seq); err != nil {
			t.Fatal(err)
		}
		var fv any
		_ = json.Unmarshal([]byte(fields), &fv)
		fb, _ := json.Marshal(fv)
		out["item:"+id] = fmt.Sprintf("%s\x00%s\x00%d", content, fb, seq)
	}
	rows.Close()
	crows, err := s.db.Query(s.q(`SELECT id, body FROM comments WHERE workspace_id = ?`), w.wsID)
	if err != nil {
		t.Fatal(err)
	}
	for crows.Next() {
		var id, body string
		if err := crows.Scan(&id, &body); err != nil {
			t.Fatal(err)
		}
		out["comment:"+id] = body
	}
	crows.Close()
	vrows, err := s.db.Query(s.q(`SELECT v.id, v.content, v.is_diff, COALESCE(v.body_sha256, '') FROM item_versions v JOIN items i ON i.id = v.item_id WHERE i.workspace_id = ?`), w.wsID)
	if err != nil {
		t.Fatal(err)
	}
	for vrows.Next() {
		var id, content, hash string
		var isDiff bool
		if err := vrows.Scan(&id, &content, &isDiff, &hash); err != nil {
			t.Fatal(err)
		}
		out["version:"+id] = fmt.Sprintf("%t\x00%s\x00%s", isDiff, hash, content)
	}
	vrows.Close()
	for _, it := range w.items {
		var stamp *string
		if err := s.db.QueryRow(s.q(`SELECT last_referenced_at FROM attachments WHERE id = ?`), it.newID).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		if stamp != nil {
			out["stamp:"+it.newID] = *stamp
		} else {
			out["stamp:"+it.newID] = "<nil>"
		}
	}
	return out
}

// resolvedChain is the item's version bodies as a reader is shown them.
func (w *remapWorld) resolvedChain(t *testing.T, it remapWorldItem) map[string]string {
	t.Helper()
	var content string
	if err := w.s.db.QueryRow(w.s.q(`SELECT content FROM items WHERE id = ?`), it.id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	vs, err := w.s.ListItemVersionsResolved(it.id, content)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, v := range vs {
		out[v.ID] = v.Content
	}
	return out
}

func (w *remapWorld) swapOldForNew(in map[string]string, it remapWorldItem) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = strings.ReplaceAll(v, it.oldID, it.newID)
	}
	return out
}

// itemKeys are the snapshot keys that belong to one item.
func (w *remapWorld) itemKeys(it remapWorldItem) []string {
	keys := []string{"item:" + it.id, "comment:" + it.commentID}
	for _, n := range []string{"1", "2"} {
		if strings.HasPrefix(it.title, "item-") && it.title != "item-dead" {
			keys = append(keys, "version:ver-"+strings.TrimPrefix(it.title, "item-")+"-"+n)
		}
	}
	return keys
}

// Stopped after the first chunk (the fault seam), a part-done remap must (1)
// resolve every item it did process FULLY, content, fields, its comments, its
// version chain, and stamp what they now reference; (2) leave every other item
// byte-for-byte as it was, references dangling exactly as before; and (3) a
// rerun must complete the rest, and a further run must change nothing.
func TestRemap_StopAfterFirstChunk(t *testing.T) {
	t.Parallel()
	s := remapStore(t)
	w := buildRemapWorld(t, s, 20)

	bystander := createTestItem(t, s, w.wsID, w.colID, "bystander", "no refs")
	before := w.snapshot(t)
	chainsBefore := map[string]map[string]string{}
	for _, it := range w.items {
		chainsBefore[it.id] = w.resolvedChain(t, it)
	}

	gapWriteOK := false
	s.remapTune = &remapTuning{maxItems: 5, afterChunk: func(info RemapChunkInfo) error {
		// Between chunks the workspace seq lock is free: another session's item
		// write goes straight through.
		title := "bystander renamed in the gap"
		if _, err := s.UpdateItem(bystander.ID, models.ItemUpdate{Title: &title}); err != nil {
			t.Errorf("an item write between chunks failed: %v", err)
		} else {
			gapWriteOK = true
		}
		if info.Chunk == 1 {
			return errors.New("injected stop after chunk 1")
		}
		return nil
	}}
	err := s.RemapAttachmentReferencesInWorkspace(w.wsID, w.oldTo)
	var partial *RemapPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("a remap stopped between chunks must return *RemapPartialError, got %v", err)
	}
	if partial.Chunks != 1 || partial.Items != 5 {
		t.Fatalf("partial = %d chunks / %d items, want 1 / 5", partial.Chunks, partial.Items)
	}
	if strings.Contains(err.Error(), "injected") || !strings.Contains(err.Error(), "delete this partial workspace and import the bundle again") {
		t.Errorf("partial error text must be plain and name the remedy, got %q", err.Error())
	}
	if !gapWriteOK {
		t.Fatal("the between-chunks write never ran")
	}

	after := w.snapshot(t)
	var done, untouched []remapWorldItem
	nDone := 0
	for _, it := range w.items {
		// The soft-deleted item is processed iff its comment was rewritten (its
		// own body never is).
		probe := "item:" + it.id
		if it.title == "item-dead" {
			probe = "comment:" + it.commentID
		}
		if strings.Contains(after[probe], it.newID) {
			done = append(done, it)
			if it.title != "item-dead" {
				nDone++
			}
		} else {
			untouched = append(untouched, it)
		}
	}
	if nDone == 0 || nDone >= 20 {
		t.Fatalf("instrument: %d of 20 items processed after chunk 1, want a strict part", nDone)
	}

	// (1) Every processed item resolves fully.
	for _, it := range done {
		if it.title == "item-dead" {
			continue
		}
		for _, key := range w.itemKeys(it)[:2] {
			if strings.Contains(after[key], it.oldID) || !strings.Contains(after[key], it.newID) {
				t.Errorf("processed %s: %s not fully remapped: %q", it.title, key, after[key])
			}
		}
		if !strings.Contains(after["item:"+it.id], `"cover":"pad-attachment:`+it.newID) {
			t.Errorf("processed %s: fields not remapped: %q", it.title, after["item:"+it.id])
		}
		got, want := w.resolvedChain(t, it), w.swapOldForNew(chainsBefore[it.id], it)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("processed %s: version chain resolves to %q, want %q", it.title, got, want)
		}
		if after["stamp:"+it.newID] == "<nil>" {
			t.Errorf("processed %s: the clone it now references carries no stamp", it.title)
		}
	}
	// (2) Every other item is byte-for-byte as before, stamp included.
	for _, it := range untouched {
		for _, key := range append(w.itemKeys(it), "stamp:"+it.newID) {
			if it.title == "item-dead" && strings.HasPrefix(key, "version:") {
				continue
			}
			if after[key] != before[key] {
				t.Errorf("unprocessed %s: %s changed\n before %q\n after  %q", it.title, key, before[key], after[key])
			}
		}
		if it.title != "item-dead" {
			if got := w.resolvedChain(t, it); !reflect.DeepEqual(got, chainsBefore[it.id]) {
				t.Errorf("unprocessed %s: version chain changed: %q", it.title, got)
			}
		}
	}
	// Whole workspace: the rows that changed are exactly the processed items'.
	changed := map[string]bool{}
	for k, v := range after {
		if before[k] != v {
			changed[k] = true
		}
	}
	allowed := map[string]bool{}
	for _, it := range done {
		for _, k := range append(w.itemKeys(it), "stamp:"+it.newID) {
			allowed[k] = true
		}
	}
	for k := range changed {
		if !allowed[k] && !strings.HasPrefix(k, "item:"+bystander.ID) {
			t.Errorf("row %s changed but belongs to no processed item", k)
		}
	}

	// (3) A rerun completes the rest.
	s.remapTune = nil
	if err := s.RemapAttachmentReferencesInWorkspace(w.wsID, w.oldTo); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	full := w.snapshot(t)
	for _, it := range w.items {
		keys := w.itemKeys(it)
		if it.title == "item-dead" {
			keys = []string{"comment:" + it.commentID}
			if !strings.Contains(full["item:"+it.id], it.oldID) {
				t.Errorf("a soft-deleted item's body must stay untouched: %q", full["item:"+it.id])
			}
		}
		for _, key := range keys {
			// Content/fields are checked below; a reverse patch row holds
			// characters, not whole ids, so it is judged by the resolved
			// chain below instead.
			if key == "item:"+it.id || strings.HasSuffix(key, "-2") {
				continue
			}
			if strings.Contains(full[key], it.oldID) || !strings.Contains(full[key], it.newID) {
				t.Errorf("after the rerun %s still points at the source id: %q", key, full[key])
			}
		}
		if it.title != "item-dead" {
			if strings.Contains(full["item:"+it.id], it.oldID) {
				t.Errorf("after the rerun %s content/fields still hold the source id", it.title)
			}
			want := w.swapOldForNew(chainsBefore[it.id], it)
			if got := w.resolvedChain(t, it); !reflect.DeepEqual(got, want) {
				t.Errorf("after the rerun %s chain = %q, want %q", it.title, got, want)
			}
		}
		if full["stamp:"+it.newID] == "<nil>" {
			t.Errorf("after the rerun the clone of %s carries no stamp", it.title)
		}
	}
	for _, it := range done {
		if it.title != "item-dead" && full["item:"+it.id] != after["item:"+it.id] {
			t.Errorf("a rerun rewrote the already-processed %s again", it.title)
		}
		if full["stamp:"+it.newID] != after["stamp:"+it.newID] {
			t.Errorf("a rerun re-stamped the already-processed %s", it.title)
		}
	}

	// Idempotent: a third run changes nothing at all, and stamps nothing.
	var infos []RemapChunkInfo
	s.remapTune = &remapTuning{afterChunk: func(i RemapChunkInfo) error { infos = append(infos, i); return nil }}
	if err := s.RemapAttachmentReferencesInWorkspace(w.wsID, w.oldTo); err != nil {
		t.Fatal(err)
	}
	s.remapTune = nil
	if third := w.snapshot(t); !reflect.DeepEqual(third, full) {
		t.Error("a third run changed rows: the remap is not idempotent")
	}
	if len(infos) != 1 || infos[0].Items != 0 || !infos[0].Final {
		t.Errorf("a run over a finished workspace should be one empty chunk, got %+v", infos)
	}
}

// The chunk boundary is the clock's, and the stop is an item boundary: with a
// clock that makes every reading cost 100 ms the time budget (not the item
// cap) ends each chunk after a handful of items, every chunk's lock hold stays
// under the 1 s target, and the walk still reaches every item.
func TestRemap_TimeBudgetEndsChunksAndNothingIsSkipped(t *testing.T) {
	s := remapStore(t)
	w := buildRemapWorld(t, s, 120)
	var infos []RemapChunkInfo
	s.remapTune = &remapTuning{now: fakeRemapClock(100 * time.Millisecond), afterChunk: func(i RemapChunkInfo) error {
		infos = append(infos, i)
		return nil
	}}
	if err := s.RemapAttachmentReferencesInWorkspace(w.wsID, w.oldTo); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, i := range infos {
		total += i.Items
		if i.Hold >= time.Second {
			t.Errorf("chunk %d held the lock %s on the fake clock, want < 1s", i.Chunk, i.Hold)
		}
		if i.Items > remapChunkMaxItems/2 {
			t.Errorf("chunk %d wrote %d items; the 100ms clock should have ended it by time long before the item cap", i.Chunk, i.Items)
		}
	}
	if len(infos) < 10 {
		t.Errorf("only %d chunks: the time budget never ended a chunk", len(infos))
	}
	if total != 121 {
		t.Errorf("chunks rewrote %d items in total, want 121 (120 + the soft-deleted item's comment)", total)
	}
	snap := w.snapshot(t)
	for _, it := range w.items {
		if strings.Contains(snap["comment:"+it.commentID], it.oldID) {
			t.Errorf("%s: comment still on the source id: an item was skipped", it.title)
		}
		if it.title != "item-dead" && strings.Contains(snap["item:"+it.id], it.oldID) {
			t.Errorf("%s: body still on the source id: an item was skipped", it.title)
		}
	}
}

// The item cap alone bounds a chunk when the clock never advances.
func TestRemap_ItemCapBoundsAChunk(t *testing.T) {
	t.Parallel()
	s := remapStore(t)
	w := buildRemapWorld(t, s, 40)
	// Items that need no rewrite, interleaved by id with the ones that do, so a
	// planning page can come back partly empty and the next one overshoot.
	for i := 0; i < 60; i++ {
		createTestItem(t, s, w.wsID, w.colID, fmt.Sprintf("plain-%02d", i), "nothing to rewrite")
	}
	var infos []RemapChunkInfo
	frozen := func() time.Time { return time.Unix(1_800_000_000, 0) }
	s.remapTune = &remapTuning{maxItems: 7, now: frozen, afterChunk: func(i RemapChunkInfo) error {
		infos = append(infos, i)
		return nil
	}}
	if err := s.RemapAttachmentReferencesInWorkspace(w.wsID, w.oldTo); err != nil {
		t.Fatal(err)
	}
	for _, i := range infos {
		if i.Items > 7 {
			t.Errorf("chunk %d rewrote %d items, hard cap is 7", i.Chunk, i.Items)
		}
	}
	total := 0
	for _, i := range infos {
		total += i.Items
	}
	if total != 41 {
		t.Errorf("chunks rewrote %d items, want 41", total)
	}
	if want := (41 + 6) / 7; len(infos) < want {
		t.Errorf("%d chunks, want at least %d", len(infos), want)
	}
}

func TestRemapPartialError_IsPlainAndKeepsItsCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("update item x: pq: SELECT secret FROM users")
	err := error(&RemapPartialError{Chunks: 3, Items: 190, Cause: cause})
	if strings.Contains(err.Error(), "SELECT") || strings.Contains(err.Error(), "pq:") {
		t.Errorf("the error text must not carry the cause: %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("the cause must stay reachable through Unwrap, for the log")
	}
}

// buildBulkRemapWorkspace makes n items that all reference one source id from
// content, fields, two comments and a full-row version, so every item needs a
// rewrite on every surface. Comments and versions go in as one statement each.
func buildBulkRemapWorkspace(t *testing.T, s *Store, n int) (wsID, colID, oldID, newID string) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Remap Bulk")
	col := createTestCollection(t, s, ws.ID, "Docs")
	oldID, newID = "11111111-1111-4111-8111-111111111111", createCloneAttachment(t, s, ws.ID)
	for i := 0; i < n; i++ {
		createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Bulk %d", i), fmt.Sprintf("body of item %d, some prose to make the row non-trivial. %s", i, remapRef(oldID)))
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(s.q(q), args...); err != nil {
			t.Fatalf("bulk exec: %v", err)
		}
	}
	exec(`UPDATE items SET fields = ? WHERE workspace_id = ?`, `{"status":"open","cover":"pad-attachment:`+oldID+`"}`, ws.ID)
	exec(`DELETE FROM item_versions WHERE item_id IN (SELECT id FROM items WHERE workspace_id = ?)`, ws.ID)
	exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
		SELECT id || '-c1', id, workspace_id, 'ref comment `+remapRef(oldID)+`', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z' FROM items WHERE workspace_id = ?`, ws.ID)
	exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
		SELECT id || '-c2', id, workspace_id, 'plain comment', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z' FROM items WHERE workspace_id = ?`, ws.ID)
	exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq)
		SELECT id || '-v1', id, 'older body `+remapRef(oldID)+`', ?, '2026-01-01T00:00:00Z', 1 FROM items WHERE workspace_id = ?`, s.dialect.BoolToInt(false), ws.ID)
	return ws.ID, col.ID, oldID, newID
}

// At 1,000 rewrites the remap must (a) hold the workspace seq lock for under
// 1 s per chunk, measured on the injected clock so the figure is deterministic,
// and (b) let an item write by ANOTHER session succeed while it runs: before
// chunking the one transaction held the lock for ~12 ms per rewrite, so at
// 1,000 rewrites a writer waited ~12 s and a bounded pool (lock_timeout 10 s)
// answered it 503.
func TestRemap_ThousandRewritesLockHoldAndInterleavedWrites(t *testing.T) {
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})
	const n = 1000
	wsID, colID, oldID, newID := buildBulkRemapWorkspace(t, s, n)
	bystander := createTestItem(t, s, wsID, colID, "bystander", "no refs")
	if got := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1 AND content LIKE '%' || $2 || '%'`, wsID, oldID); got != n {
		t.Fatalf("fixture: %d items reference the source id, want %d", got, n)
	}

	// A second session writes items for the whole remap.
	var (
		mu        sync.Mutex
		writes    int
		maxWait   time.Duration
		writeErrs []error
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			title := fmt.Sprintf("bystander %d", i)
			begin := time.Now()
			_, err := s.UpdateItem(bystander.ID, models.ItemUpdate{Title: &title})
			d := time.Since(begin)
			mu.Lock()
			if err != nil {
				writeErrs = append(writeErrs, err)
			} else {
				writes++
			}
			if d > maxWait {
				maxWait = d
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var infos []RemapChunkInfo
	var wall time.Time
	var maxWall time.Duration
	s.remapTune = &remapTuning{afterChunk: func(i RemapChunkInfo) error {
		infos = append(infos, i)
		return nil
	}}
	wall = time.Now()
	remapErr := s.RemapAttachmentReferencesInWorkspace(wsID, map[string]string{oldID: newID})
	maxWall = time.Since(wall)
	close(stop)
	wg.Wait()
	if remapErr != nil {
		t.Fatalf("remap: %v", remapErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(writeErrs) > 0 {
		t.Errorf("%d item writes by another session failed during the remap, first: %v", len(writeErrs), writeErrs[0])
	}
	if writes < 3 {
		t.Errorf("only %d interleaved writes completed; the writer never got through", writes)
	}
	if maxWait > 5*time.Second {
		t.Errorf("an interleaved write waited %s", maxWait)
	}
	for _, i := range infos {
		// Real clock here; the 1 s figure is asserted on the fake clock below.
		if i.Hold > 3*time.Second {
			t.Errorf("chunk %d held the lock %s", i.Chunk, i.Hold)
		}
	}
	if len(infos) < 10 {
		t.Errorf("%d chunks for %d rewrites, want the work split", len(infos), n)
	}
	t.Logf("1,000 rewrites: %d chunks in %s, %d interleaved writes (max wait %s)", len(infos), maxWall, writes, maxWait)

	// Raw row counts on the real Postgres: nothing left on the source id, every
	// surface on the clone.
	for _, c := range []struct{ name, table, col string }{
		{"items", "items", "content"}, {"fields", "items", "fields"}, {"comments", "comments", "body"}, {"versions", "item_versions", "content"},
	} {
		join := `workspace_id = $1`
		if c.table == "item_versions" {
			join = `item_id IN (SELECT id FROM items WHERE workspace_id = $1)`
		}
		if got := pgCount(t, s, `SELECT count(*) FROM `+c.table+` WHERE `+join+` AND CAST(`+c.col+` AS TEXT) LIKE '%' || $2 || '%'`, wsID, oldID); got != 0 {
			t.Errorf("%s: %d rows still hold the source id", c.name, got)
		}
		if got := pgCount(t, s, `SELECT count(*) FROM `+c.table+` WHERE `+join+` AND CAST(`+c.col+` AS TEXT) LIKE '%' || $2 || '%'`, wsID, newID); got != n {
			t.Errorf("%s: %d rows hold the clone id, want %d", c.name, got, n)
		}
	}
}

// The same 1,000 rewrites on the injected clock: every chunk's lock hold is
// under 1 s whatever the box does, and the work is split.
func TestRemap_ThousandRewritesEveryChunkUnderOneSecond(t *testing.T) {
	s := remapStore(t)
	const n = 1000
	for _, step := range []time.Duration{time.Millisecond, 12 * time.Millisecond} {
		wsID, _, oldID, newID := buildBulkRemapWorkspace(t, s, n)
		// 12 ms per reading is the measured cost of one rewritten item, so a
		// chunk's hold on this clock tracks its item count the way the real
		// one does.
		var infos []RemapChunkInfo
		s.remapTune = &remapTuning{now: fakeRemapClock(step), afterChunk: func(i RemapChunkInfo) error {
			infos = append(infos, i)
			return nil
		}}
		if err := s.RemapAttachmentReferencesInWorkspace(wsID, map[string]string{oldID: newID}); err != nil {
			t.Fatal(err)
		}
		total, maxHold := 0, time.Duration(0)
		for _, i := range infos {
			total += i.Items
			if i.Hold > maxHold {
				maxHold = i.Hold
			}
			if i.Hold >= time.Second {
				t.Errorf("step %s: chunk %d held the lock %s on the injected clock", step, i.Chunk, i.Hold)
			}
		}
		if len(infos) < 10 || total == 0 {
			t.Errorf("step %s: %d chunks / %d items; the 1,000 rewrites were not split", step, len(infos), total)
		}
		t.Logf("step %s: %d chunks, %d items, max hold %s on the injected clock", step, len(infos), total, maxHold)
	}
}
