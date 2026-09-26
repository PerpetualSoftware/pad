package server

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2659: GET /search resolves its `collection` filter the way the item
// handlers resolve a collection (exact match first, then the singular/alias
// fallback, an archived name still claiming itself), once per workspace in
// scope. It used to match a literal slug, so shorthand found nothing and
// clients normalised it, which let `tasks` shadow a collection named `task`.

const bug2659Term = "zebracornquartz"

func bug2659Workspace(t *testing.T, srv *Server, slug string) *models.Workspace {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace %q: %v", slug, err)
	}
	return ws
}

func bug2659Collection(t *testing.T, srv *Server, wsID, slug string) *models.Collection {
	t.Helper()
	if c, err := srv.store.GetCollectionBySlug(wsID, slug); err == nil && c != nil {
		return c
	}
	c, err := srv.store.CreateCollection(wsID, models.CollectionCreate{Name: slug, Slug: slug, Prefix: "ZQ" + strings.ToUpper(slug), Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("create collection %q: %v", slug, err)
	}
	if c.Slug != slug {
		t.Fatalf("premise: wanted collection slug %q, got %q", slug, c.Slug)
	}
	return c
}

func bug2659Item(t *testing.T, srv *Server, wsID string, coll *models.Collection, title string) {
	t.Helper()
	if _, err := srv.store.CreateItem(wsID, coll.ID, models.ItemCreate{Title: title + " " + bug2659Term, Content: bug2659Term, Fields: `{}`}); err != nil {
		t.Fatalf("create item %q: %v", title, err)
	}
}

func bug2659Titles(t *testing.T, srv *Server, query, token string) []string {
	t.Helper()
	path := "/api/v1/search?q=" + bug2659Term + query
	var rr *httptest.ResponseRecorder
	if token == "" {
		rr = doRequest(srv, "GET", path, nil)
	} else {
		rr = doRequestWithCookie(srv, "GET", path, nil, token)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("search%s: %d %s", query, rr.Code, rr.Body.String())
	}
	var resp store.SearchResponse
	parseJSON(t, rr, &resp)
	titles := []string{}
	for _, r := range resp.Results {
		titles = append(titles, r.Item.Title)
	}
	sort.Strings(titles)
	return titles
}

func bug2659Want(t *testing.T, got []string, want ...string) {
	t.Helper()
	for i := range want {
		want[i] += " " + bug2659Term
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("results: got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("results: got %q, want %q", got, want)
		}
	}
}

// Shorthand reaches the collection it names when nothing is named that
// exactly. Red on main: the literal filter matched no `task` and answered
// nothing, which is why clients normalised.
func TestSearchCollectionShorthandResolves(t *testing.T) {
	srv := testServer(t)
	ws := bug2659Workspace(t, srv, createWSForTest(t, srv))
	bug2659Item(t, srv, ws.ID, bug2659Collection(t, srv, ws.ID, "tasks"), "in tasks")

	bug2659Want(t, bug2659Titles(t, srv, "&workspace="+ws.Slug+"&collection=task", ""), "in tasks")
}

// Exact beats alias: with both `task` and `tasks`, `task` is `task`.
func TestSearchCollectionExactBeatsAlias(t *testing.T) {
	srv := testServer(t)
	ws := bug2659Workspace(t, srv, createWSForTest(t, srv))
	bug2659Item(t, srv, ws.ID, bug2659Collection(t, srv, ws.ID, "tasks"), "in tasks")
	bug2659Item(t, srv, ws.ID, bug2659Collection(t, srv, ws.ID, "task"), "in task")

	bug2659Want(t, bug2659Titles(t, srv, "&workspace="+ws.Slug+"&collection=task", ""), "in task")
	bug2659Want(t, bug2659Titles(t, srv, "&workspace="+ws.Slug+"&collection=tasks", ""), "in tasks")
}

// Per workspace: the same input names `task` in one workspace and resolves to
// `tasks` in another, in one unscoped search. No single slug can say that.
func TestSearchCollectionResolvesPerWorkspace(t *testing.T) {
	srv := testServer(t)
	a := bug2659Workspace(t, srv, createWSForTest(t, srv))
	b := bug2659Workspace(t, srv, createWSForTest(t, srv))
	bug2659Item(t, srv, a.ID, bug2659Collection(t, srv, a.ID, "tasks"), "a tasks")
	bug2659Item(t, srv, a.ID, bug2659Collection(t, srv, a.ID, "task"), "a task")
	bug2659Item(t, srv, b.ID, bug2659Collection(t, srv, b.ID, "tasks"), "b tasks")

	bug2659Want(t, bug2659Titles(t, srv, "&collection=task", ""), "a task", "b tasks")
}

// The membership fan-out (a signed-in caller, no workspace named) resolves
// the same way.
func TestSearchCollectionResolvesAcrossMemberships(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner-2659@test.com", "Owner")
	var slugs []string
	for _, name := range []string{"Alpha", "Beta"} {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
		}
		var w models.Workspace
		parseJSON(t, rr, &w)
		slugs = append(slugs, w.Slug)
	}
	a, b := bug2659Workspace(t, srv, slugs[0]), bug2659Workspace(t, srv, slugs[1])
	bug2659Item(t, srv, a.ID, bug2659Collection(t, srv, a.ID, "tasks"), "a tasks")
	bug2659Item(t, srv, a.ID, bug2659Collection(t, srv, a.ID, "task"), "a task")
	bug2659Item(t, srv, b.ID, bug2659Collection(t, srv, b.ID, "tasks"), "b tasks")

	bug2659Want(t, bug2659Titles(t, srv, "&collection=task", token), "a task", "b tasks")
}

// An archived `task` still claims its name: the filter does not fall through
// to `tasks` in that workspace.
func TestSearchCollectionArchivedNameClaimsItself(t *testing.T) {
	srv := testServer(t)
	ws := bug2659Workspace(t, srv, createWSForTest(t, srv))
	bug2659Item(t, srv, ws.ID, bug2659Collection(t, srv, ws.ID, "tasks"), "in tasks")
	archived := bug2659Collection(t, srv, ws.ID, "task")
	if err := srv.store.DeleteCollection(archived.ID, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}

	bug2659Want(t, bug2659Titles(t, srv, "&workspace="+ws.Slug+"&collection=task", ""))
}

// A name that resolves nowhere answers an empty 200, as before.
func TestSearchCollectionUnknownIsEmpty(t *testing.T) {
	srv := testServer(t)
	ws := bug2659Workspace(t, srv, createWSForTest(t, srv))
	bug2659Item(t, srv, ws.ID, bug2659Collection(t, srv, ws.ID, "tasks"), "in tasks")

	bug2659Want(t, bug2659Titles(t, srv, "&workspace="+ws.Slug+"&collection=nosuchthing", ""))
}

func TestCapabilitiesAdvertiseSearchCollectionResolution(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/server/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities: %d", rr.Code)
	}
	var caps struct {
		SearchCollectionResolution bool `json:"search_collection_resolution"`
	}
	parseJSON(t, rr, &caps)
	if !caps.SearchCollectionResolution {
		t.Fatal("search_collection_resolution not advertised")
	}
}
