package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4767: the activity list doors passed `limit` to the store with no
// maximum, and every returned row costs one item lookup for enrichment. They
// now clamp it to activityMaxLimit, keeping the lenient parse: an absent,
// non-numeric or non-positive limit still reads the store default of 20.

func bug4767Count(t *testing.T, srv *Server, path string) int {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
	}
	var page []models.Activity
	parseJSON(t, rr, &page)
	return len(page)
}

func TestBUG4767_ActivityLimitIsClamped(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	item := createItem(t, srv, slug, "tasks", map[string]any{"title": "Busy task"})
	doc, err := srv.store.CreateDocument(ws.ID, models.DocumentCreate{Title: "Busy doc"})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	const seeded = activityMaxLimit + 5
	for _, docID := range []string{item.ID, doc.ID} {
		for range seeded {
			if _, err := srv.store.CreateActivity(models.Activity{
				WorkspaceID: ws.ID, DocumentID: docID, Action: "updated", Actor: "user", Source: "web",
			}); err != nil {
				t.Fatalf("CreateActivity: %v", err)
			}
		}
	}

	base := "/api/v1/workspaces/" + slug
	cases := []struct {
		name, path string
		want       int
	}{
		{"workspace over max", base + "/activity?limit=500", activityMaxLimit},
		{"workspace small", base + "/activity?limit=5", 5},
		{"workspace absent", base + "/activity", 20},
		{"workspace zero", base + "/activity?limit=0", 20},
		{"workspace negative", base + "/activity?limit=-3", 20},
		{"workspace non-numeric", base + "/activity?limit=abc", 20},
		{"item over max", base + "/items/" + item.Slug + "/activity?limit=500", activityMaxLimit},
		{"item small", base + "/items/" + item.Slug + "/activity?limit=5", 5},
		{"item absent", base + "/items/" + item.Slug + "/activity", 20},
		{"document over max", base + "/documents/" + doc.ID + "/activity?limit=500", activityMaxLimit},
		{"document small", base + "/documents/" + doc.ID + "/activity?limit=5", 5},
		{"document absent", base + "/documents/" + doc.ID + "/activity", 20},
	}
	for _, c := range cases {
		if got := bug4767Count(t, srv, c.path); got != c.want {
			t.Errorf("%s: got %d rows, want %d", c.name, got, c.want)
		}
	}
}
