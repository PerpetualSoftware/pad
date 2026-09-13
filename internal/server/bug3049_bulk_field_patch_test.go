package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// BUG-3049 — the REST bulk field ops must not revert a field they do not name.
//
// `bulkFieldUpdate` reads the item's stored blob (in `handleBulkItems`, before
// this function is called), merges the op's changes onto it, and writes. While
// that write sent the whole blob, everything written to the row between the
// read and the write was reverted — a bulk status move over ten items could
// undo ten unrelated single-field edits, one per row.
//
// THE INTERLEAVING IS REAL HERE, not simulated: the test hands the door a STALE
// item snapshot (the state as of the handler's read) after having written a new
// key to the row through the ordinary PATCH path. That is exactly the shape of
// the race, made deterministic — no goroutines, no timing.
//
// The control that makes this test worth having: revert the door to
// `Fields: &fieldsStr` and it fails, because the stale snapshot's blob does not
// contain the concurrent key. See TestFieldsPatchFromMerge for the diff
// helper's own legs.
func TestBulkFieldUpdate_DoesNotRevertAConcurrentWriteToAnotherField(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, wsSlug, "Bulk target", `{"status":"open","priority":"high"}`)

	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("load workspace %q: %v", wsSlug, err)
	}

	// The snapshot the bulk handler would be holding: read BEFORE the
	// concurrent write below.
	stale, err := srv.store.GetItem(item.ID)
	if err != nil || stale == nil {
		t.Fatalf("load item snapshot: %v", err)
	}

	// The concurrent writer: an ordinary single-key PATCH landing after the
	// bulk handler's read and before its write.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"category": "billing"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("concurrent PATCH: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// PREMISE, asserted before the thing under test: the concurrent write
	// actually landed. Without this leg the test passes when nothing happened.
	if got := decodeItemFields(t, mustGetItemFields(t, srv, item.ID))["category"]; got != "billing" {
		t.Fatalf("premise failed: the concurrent write did not land, category=%v", got)
	}

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+wsSlug+"/items/bulk", nil)
	var dropped []string
	updated, opErr := srv.bulkFieldUpdate(req, ws.ID, stale, map[string]any{"status": "done"},
		true, /* force: skip the open-children precheck, which is not what this test is about */
		nil, "user", "web", "", &dropped)
	if opErr != nil {
		t.Fatalf("bulkFieldUpdate: %v", opErr.message)
	}

	fields := decodeItemFields(t, updated.Fields)

	// The door's own key changed — the second half of the premise. A door that
	// wrote nothing at all would satisfy the survival assertion below.
	if fields["status"] != "done" {
		t.Fatalf("the bulk op did not apply its own change: status=%v want done", fields["status"])
	}
	// The defect: the concurrent key must survive a write that never named it.
	if fields["category"] != "billing" {
		t.Errorf("BUG-3049: the bulk op reverted a field it did not name: category=%v want billing", fields["category"])
	}
	// And a field neither write touched is still there.
	if fields["priority"] != "high" {
		t.Errorf("priority lost: got %v want high", fields["priority"])
	}
}

func mustGetItemFields(t *testing.T, srv *Server, itemID string) string {
	t.Helper()
	it, err := srv.store.GetItem(itemID)
	if err != nil || it == nil {
		t.Fatalf("re-read item %s: %v", itemID, err)
	}
	return it.Fields
}

// TestFieldsPatchFromMerge covers the diff helper directly: what it carries,
// what it omits, and what it deletes. The omission is the whole point — every
// key it leaves out is a key this write can no longer revert.
func TestFieldsPatchFromMerge(t *testing.T) {
	cases := []struct {
		name           string
		stored, merged map[string]any
		want           map[string]any
	}{
		{
			name:   "unchanged keys are omitted",
			stored: map[string]any{"status": "open", "priority": "high"},
			merged: map[string]any{"status": "done", "priority": "high"},
			want:   map[string]any{"status": "done"},
		},
		{
			name:   "a key the pipeline added is carried",
			stored: map[string]any{"status": "open"},
			merged: map[string]any{"status": "done", "completed_at": "2026-09-13"},
			want:   map[string]any{"status": "done", "completed_at": "2026-09-13"},
		},
		{
			name:   "a key the pipeline dropped becomes an explicit nil delete",
			stored: map[string]any{"status": "open", "owner": "ghost"},
			merged: map[string]any{"status": "open"},
			want:   map[string]any{"owner": nil},
		},
		{
			name:   "nothing changed produces an empty patch",
			stored: map[string]any{"status": "open"},
			merged: map[string]any{"status": "open"},
			want:   map[string]any{},
		},
		{
			name:   "nested values compare by value, not by identity",
			stored: map[string]any{"github_pr": map[string]any{"number": float64(7)}},
			merged: map[string]any{"github_pr": map[string]any{"number": float64(7)}},
			want:   map[string]any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fieldsPatchFromMerge(tc.stored, tc.merged)
			if !reflect.DeepEqual(got, tc.want) {
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(tc.want)
				t.Errorf("patch = %s, want %s", gj, wj)
			}
		})
	}
}

// An unreadable stored blob is REFUSED, not silently discarded — and the state
// it guards is UNREACHABLE through any door, which is why this is a unit test of
// the arm rather than a fixture with a broken row in the database.
//
// Codex round 3 read the diff correctly: before this unit, the door ignored the
// unmarshal error, so the decoded map stayed empty, the caller's changes were
// written over the top, and the unreadable bytes were replaced with a valid
// blob. The patch conversion turned that silent repair into a store-level
// failure. Two things were then measured rather than assumed:
//
//  1. THE STATE CANNOT EXIST. Postgres stores items.fields as `jsonb`
//     (pgmigrations/035_items_jsonb_not_null.sql), so malformed text is refused
//     by the column type. SQLite stores it as TEXT, but the partial UNIQUE index
//     over json_extract(fields, '$.invocation_slug') errors on a row whose
//     fields fails json_valid — migration 056's own comment says so, and an
//     attempt to inject one in this test failed with `SQL logic error: malformed
//     JSON (1)`. Migration 056 also backfilled every pre-existing bad row to
//     '{}'. So the "repair path" codex identified as lost could never fire.
//  2. THE REPAIR WAS NOT WORTH KEEPING ANYWAY. It destroyed the unreadable
//     bytes, which is the opposite of the room's standing answer for unreadable
//     stored state (BUG-2627 part 3, BUG-2675's `stored_state_unreadable`): the
//     raw bytes are the only thing a human could repair from.
//
// So the arm stays as a defensive refusal with an honest, retry-hostile code,
// and this test drives it the only way it is reachable — by handing the door a
// snapshot whose Fields do not parse. It does NOT assert anything about a row,
// because no row can be in this state.
func TestBulkFieldUpdate_RefusesAnUnreadableFieldsSnapshot(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, wsSlug, "Unreadable snapshot", `{"status":"open"}`)

	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("load workspace %q: %v", wsSlug, err)
	}

	stale, err := srv.store.GetItem(item.ID)
	if err != nil || stale == nil {
		t.Fatalf("re-read the item: %v", err)
	}
	const brokenBlob = `{"status":"open",`
	// Premise: the fixture really is unreadable.
	if json.Valid([]byte(brokenBlob)) {
		t.Fatalf("premise failed: %q parses", brokenBlob)
	}
	stale.Fields = brokenBlob

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+wsSlug+"/items/bulk", nil)
	var dropped []string
	_, opErr := srv.bulkFieldUpdate(req, ws.ID, stale, map[string]any{"status": "done"},
		true, nil, "user", "web", "", &dropped)
	if opErr == nil {
		t.Fatal("expected a refusal for an unreadable fields snapshot, got success")
	}
	if opErr.code != storedStateUnreadableCode {
		t.Errorf("code = %q, want %q (a caller must be able to tell this is not retryable)", opErr.code, storedStateUnreadableCode)
	}

	// Nothing was written: the real row still holds the status it held. (Compared
	// by field rather than against a literal blob — create injects schema
	// defaults, so the stored blob carries more than the two keys seeded above.)
	// The pre-BUG-3049 door would have written status=done here.
	after := decodeItemFields(t, mustGetItemFields(t, srv, item.ID))
	if after["status"] != "open" {
		t.Errorf("the refused write touched the row: status=%v want open", after["status"])
	}
}
