package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Handler tests for /api/v1/me/workspace-tabs (TASK-3256). They go through the
// real router with a session cookie, so the route binding, auth and CSRF are
// exercised along with the handlers. testServer is SQLite-only; the store
// tests carry the Postgres coverage.

type tabsFixture struct {
	srv   *Server
	user  *models.User
	token string
}

func newTabsFixture(t *testing.T) tabsFixture {
	t.Helper()
	srv := testServer(t)
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "tabs@test.com", Name: "Tabs", Username: "tabber", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := loginUser(t, srv, "tabs@test.com", "correct-horse-battery-staple")
	return tabsFixture{srv, user, token}
}

func (f tabsFixture) do(t *testing.T, method, path string, body any) (int, workspaceTabsResponse, map[string]any) {
	t.Helper()
	rec := doRequestWithCookie(f.srv, method, "/api/v1/me/workspace-tabs"+path, body, f.token)
	var tabs workspaceTabsResponse
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tabs)
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	return rec.Code, tabs, raw
}

func errorDetails(raw map[string]any) map[string]any {
	e, _ := raw["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

func tabSlugs(r workspaceTabsResponse) string {
	var s []string
	for _, tab := range r.Tabs {
		s = append(s, tab.Slug)
	}
	return strings.Join(s, ",")
}

func TestWorkspaceTabsAPI_OpenReorderPatchClose(t *testing.T) {
	f := newTabsFixture(t)
	a := mustCreateOwnedWorkspace(t, f.srv, "Alpha", f.user)
	b := mustCreateOwnedWorkspace(t, f.srv, "Beta", f.user)
	c := mustCreateOwnedWorkspace(t, f.srv, "Gamma", f.user)

	code, got, _ := f.do(t, "GET", "/", nil)
	if code != http.StatusOK || len(got.Tabs) != 0 {
		t.Fatalf("initial GET: %d %+v", code, got)
	}

	if code, got, _ = f.do(t, "POST", "/", map[string]any{"slug": a.Slug}); code != http.StatusOK || tabSlugs(got) != a.Slug {
		t.Fatalf("open a: %d %s", code, tabSlugs(got))
	}
	f.do(t, "POST", "/", map[string]any{"slug": b.Slug, "ephemeral": true})
	code, got, _ = f.do(t, "POST", "/", map[string]any{"slug": c.Slug})
	if want := strings.Join([]string{a.Slug, b.Slug, c.Slug}, ","); code != http.StatusOK || tabSlugs(got) != want {
		t.Fatalf("after opens: %d %s, want %s", code, tabSlugs(got), want)
	}
	if !got.Tabs[1].Ephemeral || got.Tabs[0].Ephemeral {
		t.Fatalf("ephemeral flags wrong: %+v", got.Tabs)
	}

	code, got, _ = f.do(t, "PUT", "/", []string{c.Slug, a.Slug, "no-such-workspace"})
	if want := strings.Join([]string{c.Slug, a.Slug, b.Slug}, ","); code != http.StatusOK || tabSlugs(got) != want {
		t.Fatalf("reorder: %d %s, want %s", code, tabSlugs(got), want)
	}

	owner := got.Tabs[0].OwnerUsername
	if owner == "" {
		t.Fatal("tabs carry no owner_username; the route prefix cannot be built")
	}
	route := "/" + owner + "/" + b.Slug + "/tasks?view=board"
	code, got, _ = f.do(t, "PATCH", "/"+b.Slug, map[string]any{"pin": true, "last_route": route})
	if code != http.StatusOK {
		t.Fatalf("patch: %d", code)
	}
	if tab := got.Tabs[2]; tab.Slug != b.Slug || tab.Ephemeral || tab.LastRoute != route {
		t.Fatalf("patch result: %+v", tab)
	}
	code, got, _ = f.do(t, "PATCH", "/"+b.Slug, map[string]any{"last_route": nil})
	if code != http.StatusOK || got.Tabs[2].LastRoute != "" {
		t.Fatalf("clear route: %d %+v", code, got.Tabs[2])
	}

	code, got, _ = f.do(t, "DELETE", "/"+a.Slug, nil)
	if want := strings.Join([]string{c.Slug, b.Slug}, ","); code != http.StatusOK || tabSlugs(got) != want {
		t.Fatalf("close: %d %s, want %s", code, tabSlugs(got), want)
	}
	if code, _, raw := f.do(t, "PATCH", "/"+a.Slug, map[string]any{"pin": true}); code != http.StatusNotFound || errorDetails(raw) != nil {
		t.Fatalf("patch of a closed tab in a visible workspace: %d %v, want a 404 with no workspace marker", code, raw)
	}
}

func TestWorkspaceTabsAPI_LastRouteIsSameWorkspaceOnly(t *testing.T) {
	f := newTabsFixture(t)
	b := mustCreateOwnedWorkspace(t, f.srv, "B", f.user)
	// A sibling whose slug extends b's, so a prefix check without the
	// trailing slash would accept b's routes pointing into it.
	bx := mustCreateOwnedWorkspace(t, f.srv, b.Slug+"x", f.user)
	_, got, _ := f.do(t, "POST", "/", map[string]any{"slug": b.Slug})
	owner := got.Tabs[0].OwnerUsername
	base := "/" + owner + "/" + b.Slug

	for _, ok := range []string{base, base + "/", base + "/tasks", base + "/tasks/TASK-1?peek=1#c", base + "/settings", base + "/tasks?q=a%20b", base + "/caf%C3%A9", base + "/tasks?q=50%25"} {
		if code, _, raw := f.do(t, "PATCH", "/"+b.Slug, map[string]any{"last_route": ok}); code != http.StatusOK {
			t.Errorf("route %q refused: %d %v", ok, code, raw)
		}
	}
	for _, bad := range []string{
		"/" + owner + "/" + bx.Slug + "/tasks",
		base + "x/tasks",
		"https://evil.example/" + owner + "/" + b.Slug,
		"//evil.example" + base,
		base + "/../other/tasks",
		base + "/./tasks",
		base + "//evil.example",
		base + "\\tasks",
		base + "/tasks\n",
		base + "/t asks",
		"/someoneelse/" + b.Slug + "/tasks",
		"tasks",
		base + "/" + strings.Repeat("a", maxWorkspaceTabLastRouteLen),
		// Percent-encoded forms a URL parser decodes (codex round 1).
		base + "/%2e%2e/other/tasks",
		base + "/%2E%2E/other/tasks",
		base + "/.%2e/other/tasks",
		base + "/%2e./other/tasks",
		base + "/%2e/tasks",
		base + "/%2F%2Fevil.example",
		base + "/%5ctasks",
		base + "/tasks%00",
		base + "/tasks%0a",
		base + "/t%20asks",
		base + "/%zz",
		base + "/%252e%252e/other/tasks",
		base + "/%25",
	} {
		code, _, raw := f.do(t, "PATCH", "/"+b.Slug, map[string]any{"last_route": bad})
		if code != http.StatusBadRequest || raw["error"] == nil {
			t.Errorf("route %q: %d %v, want 400", bad, code, raw)
		}
	}

	for _, body := range []map[string]any{{}, {"pin": false}, {"unknown": 1}} {
		if code, _, _ := f.do(t, "PATCH", "/"+b.Slug, body); code != http.StatusBadRequest {
			t.Errorf("PATCH %v: %d, want 400", body, code)
		}
	}
}

// A workspace the caller cannot see answers exactly what an unknown slug
// answers, on every door, and never lands in the open set.
func TestWorkspaceTabsAPI_InvisibleWorkspaceIsNotAnOracle(t *testing.T) {
	f := newTabsFixture(t)
	other := mustCreateUser(t, f.srv, "other@test.com", "Other", "member")
	theirs := mustCreateOwnedWorkspace(t, f.srv, "Theirs", other)
	mine := mustCreateOwnedWorkspace(t, f.srv, "Mine", f.user)
	f.do(t, "POST", "/", map[string]any{"slug": mine.Slug})

	for _, slug := range []string{theirs.Slug, "no-such-workspace"} {
		code, _, raw := f.do(t, "POST", "/", map[string]any{"slug": slug})
		details := errorDetails(raw)
		if code != http.StatusNotFound || details["scope"] != "workspace" {
			t.Fatalf("open %s: %d %v, want the workspace 404", slug, code, raw)
		}
		code, _, raw = f.do(t, "PATCH", "/"+slug, map[string]any{"pin": true})
		details = errorDetails(raw)
		if code != http.StatusNotFound || details["scope"] != "workspace" {
			t.Fatalf("patch %s: %d %v, want the workspace 404", slug, code, raw)
		}
		code, got, _ := f.do(t, "DELETE", "/"+slug, nil)
		if code != http.StatusOK || tabSlugs(got) != mine.Slug {
			t.Fatalf("close %s: %d %s", slug, code, tabSlugs(got))
		}
	}
	if code, got, _ := f.do(t, "PUT", "/", []string{theirs.Slug, mine.Slug}); code != http.StatusOK || tabSlugs(got) != mine.Slug {
		t.Fatalf("reorder naming theirs: %d %s", code, tabSlugs(got))
	}
}

// The invariant is enforced on READ: a row whose workspace the caller lost by
// a path that never pruned it is not served.
func TestWorkspaceTabsAPI_ReadFilterHidesALostWorkspace(t *testing.T) {
	f := newTabsFixture(t)
	owner := mustCreateUser(t, f.srv, "owner@test.com", "Owner", "member")
	ws := mustCreateOwnedWorkspace(t, f.srv, "Shared", owner)
	if err := f.srv.store.AddWorkspaceMember(ws.ID, f.user.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if code, got, _ := f.do(t, "POST", "/", map[string]any{"slug": ws.Slug}); code != http.StatusOK || tabSlugs(got) != ws.Slug {
		t.Fatalf("open: %d %s", code, tabSlugs(got))
	}
	// Remove the membership underneath the store's loss path, so the row
	// stays behind.
	if _, err := f.srv.store.DB().Exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, ws.ID, f.user.ID); err != nil {
		t.Fatal(err)
	}
	list, err := f.srv.store.ListWorkspaceTabs(f.user.ID)
	rows := list.Rows
	if err != nil || len(rows) != 1 {
		t.Fatalf("precondition: the stale row must still be stored: %v %v", rows, err)
	}
	if code, got, _ := f.do(t, "GET", "/", nil); code != http.StatusOK || len(got.Tabs) != 0 {
		t.Fatalf("GET served a lost workspace: %d %+v", code, got)
	}
}

// An owner with no username (legal: usernames arrived in migration 029 with
// an empty default) has no /{owner}/{ws} prefix, and the path the web app
// would build, //{ws}, is protocol-relative. No last_route is stored for it.
func TestWorkspaceTabsAPI_NoLastRouteWithoutAnOwnerUsername(t *testing.T) {
	f := newTabsFixture(t)
	legacy := mustCreateUser(t, f.srv, "legacy@test.com", "Legacy", "member")
	ws := mustCreateOwnedWorkspace(t, f.srv, "Old", legacy)
	if err := f.srv.store.AddWorkspaceMember(ws.ID, f.user.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	_, got, _ := f.do(t, "POST", "/", map[string]any{"slug": ws.Slug})
	if len(got.Tabs) != 1 || got.Tabs[0].OwnerUsername != "" {
		t.Fatalf("precondition: an ownerless-username tab: %+v", got.Tabs)
	}
	for _, route := range []string{"//" + ws.Slug + "/tasks", "/" + ws.Slug + "/tasks"} {
		if code, _, _ := f.do(t, "PATCH", "/"+ws.Slug, map[string]any{"last_route": route}); code != http.StatusBadRequest {
			t.Errorf("route %q: %d, want 400", route, code)
		}
	}
}

func TestWorkspaceTabsAPI_RequiresAUser(t *testing.T) {
	f := newTabsFixture(t)
	rec := doRequest(f.srv, "GET", "/api/v1/me/workspace-tabs/", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET: %d, want 401", rec.Code)
	}
}

// Every answer carries the revision (BUG-3285): each door's write answers one
// above the last, a GET answers the current one without moving it, and a
// close of a slug outside the visible set writes nothing and answers like a
// GET.
func TestWorkspaceTabsAPI_EveryAnswerCarriesTheRevision(t *testing.T) {
	f := newTabsFixture(t)
	a := mustCreateOwnedWorkspace(t, f.srv, "RevAlpha", f.user)
	b := mustCreateOwnedWorkspace(t, f.srv, "RevBeta", f.user)

	_, got, raw := f.do(t, "GET", "/", nil)
	if _, ok := raw["revision"]; !ok {
		t.Fatalf("GET answer has no revision member: %v", raw)
	}
	last := got.Revision
	step := func(name, method, path string, body any, bump int64) {
		t.Helper()
		code, got, _ := f.do(t, method, path, body)
		if code != http.StatusOK || got.Revision != last+bump {
			t.Fatalf("%s: %d revision %d, want %d", name, code, got.Revision, last+bump)
		}
		last = got.Revision
	}
	step("open a", "POST", "/", map[string]any{"slug": a.Slug}, 1)
	step("open b", "POST", "/", map[string]any{"slug": b.Slug, "ephemeral": true}, 1)
	step("reorder", "PUT", "/", []string{b.Slug, a.Slug}, 1)
	step("patch", "PATCH", "/"+a.Slug, map[string]any{"last_route": "/tabber/" + a.Slug + "/tasks"}, 1)
	step("close", "DELETE", "/"+b.Slug, nil, 1)
	step("GET", "GET", "/", nil, 0)
	step("close of an unknown slug", "DELETE", "/no-such-workspace", nil, 0)
}
