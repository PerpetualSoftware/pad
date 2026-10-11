package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3585: idempotency keys on item create.
func TestTASK3585_CreateReplaysTheItemItMade(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	path := "/api/v1/workspaces/" + slug + "/collections/tasks/items"
	post := func(key string, body map[string]any) (int, models.Item, http.Header) {
		t.Helper()
		rr := doRequestWithHeaders(srv, "POST", path, body, map[string]string{"Idempotency-Key": key})
		var it models.Item
		_ = json.Unmarshal(rr.Body.Bytes(), &it)
		return rr.Code, it, rr.Header()
	}
	body := map[string]any{"title": "Keyed create", "fields": `{"status":"open"}`}
	code, first, _ := post("c-1", body)
	if code != http.StatusCreated || first.ID == "" {
		t.Fatalf("first create: %d", code)
	}
	code, again, hdr := post("c-1", body)
	if code != http.StatusCreated || again.ID != first.ID || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s vs %s %v", code, again.ID, first.ID, hdr)
	}
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	items, err := srv.store.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.Title == "Keyed create" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d items titled 'Keyed create'; the retry created a twin", n)
	}
	if code, _, _ := post("c-1", map[string]any{"title": "Something else"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("a reused key: %d", code)
	}
}

func TestTASK3585_RefusedCreateReleasesItsKey(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	path := "/api/v1/workspaces/" + slug + "/collections/tasks/items"
	bad := doRequestWithHeaders(srv, "POST", path, map[string]any{"title": "x", "fields": `{"status":"no-such-status"}`}, map[string]string{"Idempotency-Key": "c-2"})
	if bad.Code < 400 {
		t.Fatalf("premise: the bad create must be refused; got %d", bad.Code)
	}
	ok := doRequestWithHeaders(srv, "POST", path, map[string]any{"title": "x", "fields": `{"status":"open"}`}, map[string]string{"Idempotency-Key": "c-2"})
	if ok.Code != http.StatusCreated || ok.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected retry with the same key: %d %s", ok.Code, ok.Body.String())
	}
}

// The CLI and MCP send the key as op_key in the body, not the header.
func TestTASK3585_CreateBodyKeyReplays(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	path := "/api/v1/workspaces/" + slug + "/collections/tasks/items"
	body := map[string]any{"title": "Body keyed", "fields": `{"status":"open"}`, "op_key": "b-1"}
	first := doRequest(srv, "POST", path, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", first.Code, first.Body.String())
	}
	again := doRequest(srv, "POST", path, body)
	var a, b models.Item
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if again.Code != http.StatusCreated || a.ID == "" || a.ID != b.ID || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("body-key replay: %d %q vs %q", again.Code, b.ID, a.ID)
	}
}
