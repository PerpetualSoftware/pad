package mcp

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

func searchQueryAction(t *testing.T) ActionFn {
	t.Helper()
	for _, d := range Catalog {
		if d.Name == "pad_search" {
			fn, ok := d.Actions["query"]
			if !ok {
				t.Fatal("pad_search has no action \"query\"")
			}
			return fn
		}
	}
	t.Fatal("pad_search is not in the catalog")
	return nil
}

func searchStdioCall(t *testing.T, in map[string]any) (*CallToolResult, *fakeDispatcher) {
	t.Helper()
	disp := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := searchQueryAction(t)(context.Background(), in, env)
	if err != nil {
		t.Fatalf("pad_search.query: %v", err)
	}
	return res, disp
}

func searchRemoteCall(t *testing.T, in map[string]any, lister WorkspaceLister) (*CallToolResult, []recordedRequest) {
	t.Helper()
	var seen []recordedRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		seen = append(seen, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"total":0,"limit":50,"offset":0}`))
	})
	disp := &HTTPHandlerDispatcher{
		Handler:      h,
		UserResolver: func(context.Context) *models.User { return &models.User{ID: "u1"} },
		Lister:       lister,
	}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: disp}
	res, err := searchQueryAction(t)(context.Background(), in, env)
	if err != nil {
		t.Fatalf("pad_search.query: %v", err)
	}
	return res, seen
}

func TestMCPSearchAllWorkspacesRoute(t *testing.T) {
	in := map[string]any{
		"query":          "oauth",
		"all_workspaces": true,
	}

	sres, disp := searchStdioCall(t, copyInput(in))
	rres, seen := searchRemoteCall(t, copyInput(in), nil)
	if sres.IsError || rres.IsError {
		t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
	}
	if got := strings.Join(disp.gotPath, " "); got != "item search" {
		t.Fatalf("stdio cmdPath = %q, want item search", got)
	}
	if !contains(disp.gotArgs, "--all-workspaces") {
		t.Errorf("stdio args = %q, want --all-workspaces", disp.gotArgs)
	}
	if contains(disp.gotArgs, "--workspace") {
		t.Errorf("stdio args leaked session --workspace: %q", disp.gotArgs)
	}
	if len(seen) != 1 {
		t.Fatalf("remote requests = %+v, want 1", seen)
	}
	if seen[0].Path != "/api/v1/search" {
		t.Fatalf("remote path = %q, want /api/v1/search", seen[0].Path)
	}
	q, err := url.ParseQuery(seen[0].Query)
	if err != nil {
		t.Fatalf("parse remote query %q: %v", seen[0].Query, err)
	}
	if q.Get("q") != "oauth" {
		t.Errorf("remote query q = %q, want oauth (query=%v)", q.Get("q"), q)
	}
	if q.Has("workspace") {
		t.Errorf("remote query leaked session workspace: %v", q)
	}
	if q.Has("all_workspaces") {
		t.Errorf("remote query forwarded all_workspaces: %v", q)
	}

	// Even when the dispatcher has a single-workspace Lister (remote OAuth
	// auto-default), all_workspaces=true must not inject workspace=.
	singleLister := &oauthWorkspaceLister{store: &fakeStore{workspaces: []WorkspaceHint{{Slug: "only-ws"}}}}
	rres, seen = searchRemoteCall(t, copyInput(in), singleLister)
	if rres.IsError {
		t.Fatalf("remote with single-workspace lister refused: %s", textOf(rres))
	}
	if len(seen) != 1 || seen[0].Path != "/api/v1/search" {
		t.Fatalf("remote requests with lister = %+v", seen)
	}
	q, _ = url.ParseQuery(seen[0].Query)
	if q.Has("workspace") {
		t.Errorf("remote query with single-workspace lister leaked workspace: %v", q)
	}
}

func TestMCPSearchDefaultKeepsWorkspace(t *testing.T) {
	cases := map[string]map[string]any{
		"omitted": {"query": "oauth"},
		"false":   {"query": "oauth", "all_workspaces": false},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			sres, disp := searchStdioCall(t, copyInput(in))
			rres, seen := searchRemoteCall(t, copyInput(in), nil)
			if sres.IsError || rres.IsError {
				t.Fatalf("refused: stdio=%s remote=%s", textOf(sres), textOf(rres))
			}
			if !hasFlagPair(disp.gotArgs, "--workspace", "docapp") {
				t.Errorf("stdio args = %q, want --workspace docapp", disp.gotArgs)
			}
			if contains(disp.gotArgs, "--all-workspaces") {
				t.Errorf("stdio args unexpectedly had --all-workspaces: %q", disp.gotArgs)
			}
			if len(seen) != 1 {
				t.Fatalf("remote requests = %+v, want 1", seen)
			}
			if seen[0].Path != "/api/v1/search" {
				t.Fatalf("remote path = %q, want /api/v1/search", seen[0].Path)
			}
			q, err := url.ParseQuery(seen[0].Query)
			if err != nil {
				t.Fatalf("parse remote query %q: %v", seen[0].Query, err)
			}
			if q.Get("q") != "oauth" || q.Get("workspace") != "docapp" {
				t.Errorf("remote query = %v, want q=oauth and workspace=docapp", q)
			}
			if q.Has("all_workspaces") {
				t.Errorf("remote query forwarded all_workspaces: %v", q)
			}
		})
	}
}

func TestMCPSearchAllWorkspacesRefusals(t *testing.T) {
	cases := map[string]map[string]any{
		"explicit workspace":            {"all_workspaces": true, "workspace": "docapp"},
		"explicit workspace with query": {"query": "oauth", "all_workspaces": true, "workspace": "docapp"},
		"non-boolean":                   {"all_workspaces": "yes"},
		"non-boolean with query":        {"query": "oauth", "all_workspaces": "yes"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			sres, disp := searchStdioCall(t, copyInput(in))
			rres, seen := searchRemoteCall(t, copyInput(in), nil)
			if !sres.IsError || !rres.IsError || len(disp.gotPath) != 0 || len(seen) != 0 {
				t.Fatalf("both must refuse before dispatch; stdio=%v/%v remote=%v/%v", sres.IsError, disp.gotPath, rres.IsError, seen)
			}
			sc, rc := errorCodeOf(t, sres), errorCodeOf(t, rres)
			if sc != ErrValidationFailed || sc != rc {
				t.Fatalf("error code mismatch: stdio=%q (%s) remote=%q (%s)", sc, textOf(sres), rc, textOf(rres))
			}
			if !strings.Contains(textOf(sres), "all_workspaces") || textOf(sres) != textOf(rres) {
				t.Fatalf("unexpected refusal text: stdio=%s remote=%s", textOf(sres), textOf(rres))
			}
		})
	}
}

func TestMCPHTTPSearchAllWorkspacesHonoursTokenAllowList(t *testing.T) {
	srv, st := newPadServer(t)

	owner, err := st.CreateUser(models.UserCreate{
		Email:    "search-allowlist@example.com",
		Name:     "Search Owner",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	makeWorkspaceWithItem := func(name, slug, title string) *models.Workspace {
		t.Helper()
		ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: name, Slug: slug, Template: "blank"})
		if err != nil {
			t.Fatalf("create workspace %s: %v", slug, err)
		}
		if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("add owner to %s: %v", slug, err)
		}
		coll, err := st.CreateCollection(ws.ID, models.CollectionCreate{
			Name:   "Tasks",
			Slug:   "tasks",
			Prefix: "TASK",
			Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
		})
		if err != nil {
			t.Fatalf("create collection in %s: %v", slug, err)
		}
		if _, err := st.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title:  title,
			Fields: `{"status":"open"}`,
		}); err != nil {
			t.Fatalf("create item in %s: %v", slug, err)
		}
		return ws
	}

	const (
		titleA = "Alpha quasar4808 finding"
		titleB = "Beta quasar4808 finding"
	)
	wsA := makeWorkspaceWithItem("Search Workspace A", "search-ws-a", titleA)
	_ = makeWorkspaceWithItem("Search Workspace B", "search-ws-b", titleB)

	disp := &HTTPHandlerDispatcher{
		Handler:      http.HandlerFunc(srv.ServeHTTP),
		UserResolver: fixedUserResolver(owner),
	}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState(wsA.Slug),
		Dispatcher: disp,
	}
	search := func(ctx context.Context) string {
		t.Helper()
		res, err := searchQueryAction(t)(ctx, map[string]any{
			"query":          "quasar4808",
			"all_workspaces": true,
		}, env)
		if err != nil {
			t.Fatalf("pad_search.query: %v", err)
		}
		if res.IsError {
			t.Fatalf("pad_search.query refused: %s", textOf(res))
		}
		return textOf(res)
	}

	// Control: no allow-list sees both workspaces.
	gotAll := search(context.Background())
	if !strings.Contains(gotAll, titleA) || !strings.Contains(gotAll, titleB) {
		t.Fatalf("no allow-list: want both %q and %q, got:\n%s", titleA, titleB, gotAll)
	}

	// Allow-list with only A sees A and not B.
	gotOnlyA := search(server.WithTokenAllowedWorkspaces(context.Background(), []string{wsA.Slug}))
	if !strings.Contains(gotOnlyA, titleA) {
		t.Errorf("allow-list [A]: missing %q in:\n%s", titleA, gotOnlyA)
	}
	if strings.Contains(gotOnlyA, titleB) {
		t.Errorf("allow-list [A]: leaked %q in:\n%s", titleB, gotOnlyA)
	}

	// Empty allow-list sees neither.
	gotNone := search(server.WithTokenAllowedWorkspaces(context.Background(), []string{}))
	if strings.Contains(gotNone, titleA) || strings.Contains(gotNone, titleB) {
		t.Errorf("allow-list []: leaked item in:\n%s", gotNone)
	}
}
