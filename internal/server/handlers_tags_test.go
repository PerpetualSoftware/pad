package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// seedTaggedItem POSTs an item with the given JSON tags array into a collection.
func seedTaggedItem(t *testing.T, srv *Server, ws, coll, title, tags string) {
	t.Helper()
	// Omit fields so each collection's schema default fills status (tasks and
	// ideas have different status enums) — the test only cares about tags.
	rr := doRequest(srv, "POST",
		"/api/v1/workspaces/"+ws+"/collections/"+coll+"/items",
		map[string]interface{}{
			"title": title,
			"tags":  tags,
		})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed %q: expected 201, got %d: %s", title, rr.Code, rr.Body.String())
	}
}

// A tag spans collections, so a single tag must group an Idea and a Task
// together — both the cross-collection `?tag=` filter and the `/tags`
// enumeration endpoint are exercised here.
func TestTagsAcrossCollections(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	seedTaggedItem(t, srv, ws, "tasks", "Task A", `["User feedback","ux"]`)
	seedTaggedItem(t, srv, ws, "ideas", "Idea A", `["User feedback"]`)
	seedTaggedItem(t, srv, ws, "ideas", "Idea B", `["perf"]`)

	t.Run("cross-collection ?tag= filter returns items from every collection", func(t *testing.T) {
		rr := doRequest(srv, "GET",
			"/api/v1/workspaces/"+ws+"/items?tag="+url.QueryEscape("User feedback"), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("list items by tag: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var items []models.Item
		parseJSON(t, rr, &items)
		if len(items) != 2 {
			t.Fatalf("expected 2 items tagged 'User feedback', got %d: %+v", len(items), items)
		}
		colls := map[string]bool{}
		for _, it := range items {
			colls[it.CollectionSlug] = true
		}
		if !colls["tasks"] || !colls["ideas"] {
			t.Errorf("expected the tag to span tasks AND ideas, got collections %v", colls)
		}
	})

	t.Run("GET /tags enumerates distinct tags with counts, ordered", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/tags", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("list tags: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var tags []models.TagCount
		parseJSON(t, rr, &tags)
		want := []models.TagCount{
			{Tag: "User feedback", Count: 2},
			{Tag: "perf", Count: 1},
			{Tag: "ux", Count: 1},
		}
		if len(tags) != len(want) {
			t.Fatalf("got %d tags, want %d: %+v", len(tags), len(want), tags)
		}
		for i, w := range want {
			if tags[i] != w {
				t.Errorf("tag[%d] = %+v, want %+v", i, tags[i], w)
			}
		}
	})
}
