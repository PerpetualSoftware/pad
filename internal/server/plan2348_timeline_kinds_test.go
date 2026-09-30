package server

import (
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2348 U3: a view that renders a subset of the timeline pages through its
// own kinds. The History tab of a comment-heavy item used to open on a page of
// comments it drops, rendering nothing but "Load more".

// kindsFixture seeds an item whose history (create, two updates, a body edit)
// is OLDER than 60 comments, so an unfiltered first page is all comments.
func kindsFixture(t *testing.T) (*Server, string, *models.Item) {
	t.Helper()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")
	seedUpdateActivity(t, srv, item.ID, "status: open → in-progress")
	seedUpdateActivity(t, srv, item.ID, "priority: low → high")
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{"content": "body\n"})
	if rr.Code != http.StatusOK {
		t.Fatalf("content PATCH = %d: %s", rr.Code, rr.Body.String())
	}

	// Strictly newer than every history row: created_at is whole-second text.
	time.Sleep(1100 * time.Millisecond)
	for i := 0; i < 60; i++ {
		if _, err := srv.store.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
			Body: "comment", Author: "tester", CreatedBy: "user", Source: "web",
		}); err != nil {
			t.Fatalf("seed comment %d: %v", i, err)
		}
	}
	return srv, ws, item
}

// walk pages a query to the end and returns every entry id by kind.
func walkTimeline(t *testing.T, srv *Server, ws, slug, query string) (map[string][]string, int) {
	t.Helper()
	byKind := map[string][]string{}
	before, beforeID := "", ""
	pages := 0
	for {
		pages++
		if pages > 20 {
			t.Fatalf("paging %q did not terminate", query)
		}
		q := query
		if before != "" {
			q += "&before=" + before + "&before_id=" + beforeID
		}
		page := fetchTimeline(t, srv, ws, slug, q)
		for _, e := range page.Entries {
			byKind[e.Kind] = append(byKind[e.Kind], e.ID)
		}
		if !page.HasMore {
			return byKind, pages
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
	}
}

func sortedTimelineIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestTimelineKinds_HistoryPageIsNotSpentOnComments(t *testing.T) {
	t.Parallel()
	srv, ws, item := kindsFixture(t)

	all, _ := walkTimeline(t, srv, ws, item.Slug, "limit=50")
	var history []string
	for kind, ids := range all {
		if kind != "comment" {
			history = append(history, ids...)
		}
	}
	if len(all["comment"]) != 60 || len(history) < 4 || len(all["version"]) == 0 {
		t.Fatalf("precondition: want 60 comments and >= 4 history rows including a version, got %v", map[string]int{
			"comment": len(all["comment"]), "history": len(history), "version": len(all["version"])})
	}

	// PRECONDITION, the defect: unfiltered, page 1 is all comments.
	first := fetchTimeline(t, srv, ws, item.Slug, "limit=50")
	if k := kindsOf(first.Entries); k["comment"] != 50 || !first.HasMore {
		t.Fatalf("precondition: unfiltered page 1 = %v, has_more %v; want 50 comments", k, first.HasMore)
	}

	page := fetchTimeline(t, srv, ws, item.Slug, "limit=50&kinds=activity,version,note,decision")
	var got []string
	for _, e := range page.Entries {
		if e.Kind == "comment" {
			t.Fatalf("a history page carried a comment: %s", e.ID)
		}
		got = append(got, e.ID)
	}
	if page.HasMore {
		t.Fatalf("history page 1 has_more = true with only %d history rows", len(history))
	}
	if g, w := sortedTimelineIDs(got), sortedTimelineIDs(history); len(g) != len(w) {
		t.Fatalf("history page 1 = %v, want every history row %v", g, w)
	} else {
		for i := range g {
			if g[i] != w[i] {
				t.Fatalf("history page 1 = %v, want %v", g, w)
			}
		}
	}
}

func TestTimelineKinds_CommentPaginationUnchanged(t *testing.T) {
	t.Parallel()
	srv, ws, item := kindsFixture(t)

	all, _ := walkTimeline(t, srv, ws, item.Slug, "limit=50")
	onlyComments, pages := walkTimeline(t, srv, ws, item.Slug, "limit=50&kinds=comment")
	if len(onlyComments) != 1 || len(onlyComments["comment"]) != 60 {
		t.Fatalf("kinds=comment walk = %v", map[string]int{"kinds": len(onlyComments), "comments": len(onlyComments["comment"])})
	}
	if pages != 2 {
		t.Fatalf("60 comments at limit 50 took %d pages, want 2", pages)
	}
	g, w := sortedTimelineIDs(onlyComments["comment"]), sortedTimelineIDs(all["comment"])
	for i := range w {
		if g[i] != w[i] {
			t.Fatalf("the comment walk differs from the unfiltered one at %d", i)
		}
	}
}

func TestTimelineKinds_UnknownKindIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")
	for _, q := range []string{"kinds=comments", "kinds=comment,", "kinds=,activity", "kinds=Activity"} {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/timeline?"+q, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", q, rr.Code)
		}
	}
	// CONTROL: absent and empty are both "every kind".
	for _, q := range []string{"", "kinds="} {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/timeline?"+q, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%q = %d, want 200", q, rr.Code)
		}
	}
}

// An empty filtered page is an ordinary answer and must be an empty array:
// clients call entries.filter(), and a JSON null threw in three e2e specs.
func TestTimelineKinds_EmptyPageIsAnArrayNotNull(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/timeline?kinds=comment", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"entries":[]`) {
		t.Fatalf("an empty page did not serialise entries as []: %s", rr.Body.String())
	}
}
