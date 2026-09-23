package server

import (
	"net/http"
	"strings"
	"testing"
)

// BUG-3154 — a bulk op whose field KEY the server chooses must not write that
// key onto an item whose collection does not declare it.
//
// Both callers of bulkFieldUpdate pass a fixed key: `set-priority` writes
// `priority`, a status-only `move` writes `status`. Validation walks only the
// DECLARED fields, so on a collection with neither field the value used to be
// stored as an orphan key no schema-driven surface renders, and the item was
// listed under `updated` although its one requested change had no meaning
// there. The item is now refused into `failed[]`, naming the collection and
// the field, and nothing is written; the other items in the request still
// apply, which is bulk's partial-success contract.
//
// Each case sends ONE request carrying an item from each kind of collection,
// so the refusal is shown to be per item rather than per batch — and so the
// declared leg is the control: a check that refused everything, or that
// consulted the wrong collection, fails it.
func TestBulkItems_RefusesAFieldTheItemsCollectionDoesNotDeclare(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		field string
		value string
	}{
		{"set-priority", map[string]any{"op": "set-priority", "priority": "high"}, "priority", "high"},
		{"status-only move", map[string]any{"op": "move", "status": "done"}, "status", "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			slug := createWSWithCollections(t, srv)
			ws, err := srv.store.GetWorkspaceBySlug(slug)
			if err != nil || ws == nil {
				t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
			}
			// Declares neither `priority` nor `status`.
			bare := mustSchemaCollection(t, srv, ws.ID, "Bare Notes", `{"fields":[
				{"key":"note","label":"Note","type":"text"}
			]}`)

			orphanCandidate := createItem(t, srv, slug, bare.Slug, map[string]interface{}{
				"title": "No such field here", "fields": `{"note":"hi"}`,
			})
			// Tasks declare both fields: the control leg.
			declared := createBulkTestItem(t, srv, slug, "Declared", `{"status":"open","priority":"low"}`)

			body := map[string]any{"ids": []string{orphanCandidate.Ref, declared.Ref}}
			for k, v := range tc.body {
				body[k] = v
			}
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/bulk", body)
			if rr.Code != http.StatusOK {
				t.Fatalf("bulk %s: expected 200 with a per-item failure, got %d: %s", tc.name, rr.Code, rr.Body.String())
			}
			var resp bulkItemsResponse
			parseJSON(t, rr, &resp)

			// Control: the item whose collection declares the field applied.
			if len(resp.Updated) != 1 || resp.Updated[0].Ref != declared.Ref {
				t.Fatalf("expected exactly %s under updated, got %+v", declared.Ref, resp.Updated)
			}
			if got := itemFields(t, srv, slug, declared.Slug)[tc.field]; got != tc.value {
				t.Errorf("control: %s %s = %v, want %s", declared.Ref, tc.field, got, tc.value)
			}

			// The refusal: listed under failed, with a reason naming the
			// collection and the field — not listed under updated.
			if len(resp.Failed) != 1 || resp.Failed[0].Ref != orphanCandidate.Ref {
				t.Fatalf("expected exactly %s under failed, got %+v", orphanCandidate.Ref, resp.Failed)
			}
			f := resp.Failed[0]
			if f.Code != "validation_error" {
				t.Errorf("failed code = %q, want validation_error", f.Code)
			}
			if !strings.Contains(f.Error, bare.Slug) || !strings.Contains(f.Error, `"`+tc.field+`"`) {
				t.Errorf("failed reason should name collection %q and field %q, got %q", bare.Slug, tc.field, f.Error)
			}

			// The defect: nothing was written. The stored blob is compared
			// whole, so a write that added the key — or touched anything
			// else — fails here.
			stored := itemFields(t, srv, slug, orphanCandidate.Slug)
			if _, present := stored[tc.field]; present || len(stored) != 1 || stored["note"] != "hi" {
				t.Errorf("BUG-3154: refused item's stored fields changed: %v, want only note=hi", stored)
			}
			after, err := srv.store.GetItem(orphanCandidate.ID)
			if err != nil || after == nil {
				t.Fatalf("reload %s: %v", orphanCandidate.Ref, err)
			}
			if after.Seq != orphanCandidate.Seq {
				t.Errorf("refused item was written: seq %d -> %d", orphanCandidate.Seq, after.Seq)
			}
		})
	}
}
