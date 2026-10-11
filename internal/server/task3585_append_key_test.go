package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3585: idempotency keys on the note/decision appends.
func appendKeyFixture(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{"title": "Noted", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("item: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &it)
	return srv, "/api/v1/workspaces/" + slug + "/items/" + it.Slug
}

func notesOn(t *testing.T, srv *Server, path string) []models.ItemImplementationNote {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	var it models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &it)
	return models.ExtractItemImplementationNotes(it.Fields)
}

func TestTASK3585_AppendReplaysTheEntryItMade(t *testing.T) {
	srv, path := appendKeyFixture(t)
	body := map[string]any{"append_implementation_note": map[string]any{"summary": "Keyed note"}, "source": "cli", "op_key": "n-1"}
	first := doRequest(srv, "PATCH", path, body)
	again := doRequest(srv, "PATCH", path, body)
	var a, b models.Item
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if first.Code != http.StatusOK || again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d/%d %s", first.Code, again.Code, again.Body.String())
	}
	if a.Appended == nil || a.Appended.ImplementationNote == nil || b.Appended == nil || b.Appended.ImplementationNote == nil ||
		a.Appended.ImplementationNote.ID != b.Appended.ImplementationNote.ID {
		t.Fatalf("the replay must echo the first entry: %+v vs %+v", a.Appended, b.Appended)
	}
	if n := len(notesOn(t, srv, path)); n != 1 {
		t.Fatalf("%d notes; the retry appended a twin", n)
	}
	other := map[string]any{"append_implementation_note": map[string]any{"summary": "Other"}, "op_key": "n-1"}
	if rr := doRequest(srv, "PATCH", path, other); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key: %d", rr.Code)
	}
}

func TestTASK3585_KeyOnANonAppendUpdateIsRefused(t *testing.T) {
	srv, path := appendKeyFixture(t)
	for _, body := range []map[string]any{
		{"title": "Renamed", "op_key": "u-1"},
		{"append_decision": map[string]any{"decision": "D"}, "fields_patch": map[string]any{"status": "done"}, "op_key": "u-2"},
	} {
		rr := doRequest(srv, "PATCH", path, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%v: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := doRequest(srv, "GET", path, nil)
	var it models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &it)
	if it.Title != "Noted" {
		t.Fatalf("a refused keyed update wrote: title %q", it.Title)
	}
}

func TestTASK3585_AppendWithoutAKeyIsUnchanged(t *testing.T) {
	srv, path := appendKeyFixture(t)
	body := map[string]any{"append_decision": map[string]any{"decision": "Unkeyed"}}
	for i := 0; i < 2; i++ {
		if rr := doRequest(srv, "PATCH", path, body); rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "" {
			t.Fatalf("unkeyed append %d: %d", i, rr.Code)
		}
	}
	rr := doRequest(srv, "GET", path, nil)
	var it models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &it)
	if n := len(models.ExtractItemDecisionLog(it.Fields)); n != 2 {
		t.Fatalf("two unkeyed appends must append twice: %d", n)
	}
}
