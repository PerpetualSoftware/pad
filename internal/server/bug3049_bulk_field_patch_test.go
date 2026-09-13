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
