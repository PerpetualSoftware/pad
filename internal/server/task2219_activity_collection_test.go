package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2219 (audit C47): the web activity page filtered its loaded page by
// collection CLIENT-side, so a collection whose rows were not in the newest
// page read as empty, and Load more was hidden under the filter. The filter
// is now a `collection` query parameter applied in the store query, before
// LIMIT, so every page is full and the keyset cursor walks it.

func createActivityItem(t *testing.T, srv *Server, ws, coll, title string) {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll+"/items", map[string]any{"title": title})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %s/%s: %d %s", coll, title, rr.Code, rr.Body.String())
	}
}

func activityPage(t *testing.T, srv *Server, path string) []models.Activity {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
	}
	var page []models.Activity
	parseJSON(t, rr, &page)
	return page
}

func TestTASK2219_CollectionFilterIsAppliedBeforeTheLimit(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	// Five tasks among many more ideas.
	for i := range 5 {
		createActivityItem(t, srv, ws, "tasks", fmt.Sprintf("Task %d", i))
	}
	for i := range 12 {
		createActivityItem(t, srv, ws, "ideas", fmt.Sprintf("Idea %d", i))
	}
	base := "/api/v1/workspaces/" + ws + "/activity"

	// Walk the filtered feed two rows at a time: every row is a task, and
	// all five creates are reached. (Rows written in one second order by id,
	// which is random, so the test does not assume where the tasks fall in
	// the unfiltered feed; an ignored parameter fails both checks anyway.)
	seen := map[string]bool{}
	path := base + "?collection=tasks&limit=2"
	for range 10 {
		page := activityPage(t, srv, path)
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			if a.CollectionSlug != "tasks" {
				t.Fatalf("collection=tasks returned a %q row: %+v", a.CollectionSlug, a)
			}
			if seen[a.ID] {
				t.Fatalf("row %s returned twice", a.ID)
			}
			seen[a.ID] = true
		}
		last := page[len(page)-1]
		raw, _ := json.Marshal(last.CreatedAt)
		var ts string
		_ = json.Unmarshal(raw, &ts)
		path = base + "?collection=tasks&limit=2&before=" + url.QueryEscape(ts) + "&before_id=" + url.QueryEscape(last.ID)
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d task rows, want 5", len(seen))
	}
}

func TestTASK2219_CollectionFilterExcludesWorkspaceRows(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	createActivityItem(t, srv, ws, "tasks", "Only task")
	seedWorkspaceActivities(t, srv, ws, "", 3)

	page := activityPage(t, srv, "/api/v1/workspaces/"+ws+"/activity?collection=tasks&limit=50")
	if len(page) != 1 || page[0].CollectionSlug != "tasks" {
		t.Fatalf("collection=tasks: want the one task row, got %d rows: %+v", len(page), page)
	}
}

func TestTASK2219_UnknownCollectionAnswersEmpty(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	createActivityItem(t, srv, ws, "tasks", "A task")

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/activity?collection=no-such-collection", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown collection: %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("unknown collection: body %q, want []", got)
	}
}
