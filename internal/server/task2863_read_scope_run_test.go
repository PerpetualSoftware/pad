package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2863: a read-scoped token may POST exactly one side-effect-free
// path, /api/v1/workspaces/{ws}/playbooks/{ref}/run. Every other POST stays
// refused (lead ruling: option A, one entry, not /match, /claim or
// /release).

func TestTASK2863_ReadScopePOSTPathTable(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/v1/workspaces/docapp/playbooks/ship/run", true},
		{"/api/v1/workspaces/docapp/playbooks/PLAYB-1405/run", true},
		{"/api/v1/workspaces/0b7c9e1a-3f00-4c1e-9d2a-5a6b7c8d9e0f/playbooks/my_playbook.v2/run", true},

		{"/api/v1/workspaces/docapp/playbooks/match", false},
		{"/api/v1/workspaces/docapp/playbooks/ship/run/", false},
		{"/api/v1/workspaces/docapp/playbooks/ship/run/x", false},
		{"/api/v1/workspaces/docapp/playbooks/ship", false},
		{"/api/v1/workspaces/docapp/items/TASK-1/claim", false},
		{"/api/v1/workspaces/docapp/collections/tasks/items", false},
		{"/api/v1/workspaces//playbooks/ship/run", false},
		{"/api/v1/workspaces/docapp/playbooks//run", false},
		{"/api/v1/workspaces/docapp/playbooks/../run", false},
		{"/api/v1/workspaces/./playbooks/ship/run", false},
		{"api/v1/workspaces/docapp/playbooks/ship/run", false},
		{"/api/v2/workspaces/docapp/playbooks/ship/run", false},
		{"/api/v1/workspaces/docapp/playbooks/a/b/run", false},
		{"", false},
		// The MCP dispatcher passes the string it built from caller input,
		// before http.NewRequest parses it: each of these would route
		// somewhere other than run.
		{"/api/v1/workspaces/docapp/playbooks/match?/run", false},
		{"/api/v1/workspaces/docapp?/playbooks/ship/run", false},
		{"/api/v1/workspaces/docapp/playbooks/match#/run", false},
		{"/api/v1/workspaces/docapp/playbooks/match%3F/run", false},
		{"/api/v1/workspaces/docapp/playbooks/sh ip/run", false},
		{"/api/v1/workspaces/docapp/playbooks/shïp/run", false},
	}
	for _, c := range cases {
		if got := isReadScopePOSTPath(c.path); got != c.want {
			t.Errorf("isReadScopePOSTPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}

	// The predicate widens only the read scopes, and only for POST.
	run := "/api/v1/workspaces/docapp/playbooks/ship/run"
	for _, c := range []struct {
		scopes, method string
		want           bool
	}{
		{`["read"]`, http.MethodPost, true},
		{`["pad:read"]`, http.MethodPost, true},
		{`["read"]`, http.MethodPut, false},
		{`["read"]`, http.MethodPatch, false},
		{`["read"]`, http.MethodDelete, false},
		{`["read-only"]`, http.MethodPost, false},
		{`null`, http.MethodPost, false},
	} {
		if got := tokenScopeAllows(c.scopes, c.method, run); got != c.want {
			t.Errorf("tokenScopeAllows(%s, %s, run) = %v, want %v", c.scopes, c.method, got, c.want)
		}
	}
	// A read token still cannot write anywhere else, and the empty path
	// (what callers asking "can this token write at all" pass) stays a no.
	if tokenScopeAllows(`["read"]`, http.MethodPost, "") {
		t.Error(`a read token may POST with an empty path`)
	}
}

// Through the router with real PATs: the read token's run answers the
// handler's 200; its other POSTs get TokenAuth's scope refusal, matched
// on its message; a write token's same POSTs are not refused for scope.
func TestTASK2863_ReadPATThroughTheRouter(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship",
		"content": "Step 1",
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "reader@example.com", Name: "Reader", Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	mint := func(scopes string) string {
		tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: scopes, Scopes: scopes}, 90, 365)
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	readTok, writeTok := mint(`["read"]`), mint(`["write"]`)
	as := func(tok, method, path string, body any) (int, string) {
		rr := doRequestWithHeaders(srv, method, path, body, map[string]string{"Authorization": "Bearer " + tok})
		var env struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		// TokenAuth's own refusal is the only 403 that names the scope, so
		// for a path no route serves (where a write token gets 404) this
		// is what says the read token was refused by the scope check
		// (codex round 1).
		if rr.Code == http.StatusForbidden && env.Error.Message == "Token scope does not permit this action" {
			return rr.Code, "scope"
		}
		return rr.Code, env.Error.Code
	}

	base := "/api/v1/workspaces/" + slug
	if code, _ := as(readTok, "POST", base+"/playbooks/ship/run", map[string]any{}); code != http.StatusOK {
		t.Fatalf("read PAT run: %d, want 200", code)
	}

	refused := []struct {
		name, path string
		body       any
	}{
		{"match", base + "/playbooks/match", map[string]any{"text": "ship it"}},
		{"item create", base + "/collections/tasks/items", map[string]any{"title": "x"}},
		{"run with a trailing slash", base + "/playbooks/ship/run/", map[string]any{}},
		{"run plus a segment", base + "/playbooks/ship/run/x", map[string]any{}},
		{"dot-dot ref", base + "/playbooks/../run", map[string]any{}},
	}
	for _, c := range refused {
		if code, ecode := as(readTok, "POST", c.path, c.body); code != http.StatusForbidden || ecode != "scope" {
			t.Errorf("read PAT %s: %d %q, want TokenAuth's scope refusal", c.name, code, ecode)
		}
		if _, ecode := as(writeTok, "POST", c.path, c.body); ecode == "scope" {
			t.Errorf("write PAT %s is refused for scope too, so the read leg measures the route, not the scope", c.name)
		}
	}
}
