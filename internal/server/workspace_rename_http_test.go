package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestWorkspaceRenameHTTP(t *testing.T) {
	f := newCrossWSFixture(t)
	ownerToken, err := f.srv.store.CreateAPIToken(f.ownerBoth.ID, models.APITokenCreate{Name: "owner"}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	editor := f.member("editor-rename@example.com", "editorrename", "editor", f.wsA)
	editorToken, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{Name: "editor"}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + f.wsA.Slug + "/slug"
	request := func(token, path string, body any, status int, code string) {
		t.Helper()
		rr := doRequestWithBearer(f.srv, http.MethodPatch, path, token, body)
		if rr.Code != status || (code != "" && !strings.Contains(rr.Body.String(), code)) {
			t.Fatalf("PATCH %s: got %d %s, want %d %s", path, rr.Code, rr.Body.String(), status, code)
		}
	}
	request(editorToken.Token, path, map[string]any{"expected_slug": f.wsA.Slug, "slug": "editor-attempt"}, http.StatusForbidden, "")
	for _, body := range []map[string]any{
		{"slug": "new-alpha"}, {"expected_slug": f.wsA.Slug}, {"expected_slug": f.wsA.Slug, "slug": "Bad Slug"},
		{"expected_slug": f.wsA.Slug, "slug": "deleted"}, {"expected_slug": f.wsA.Slug, "slug": "new-alpha", "name": "  "},
	} {
		request(ownerToken.Token, path, body, http.StatusBadRequest, "")
	}
	request(ownerToken.Token, path, map[string]any{"expected_slug": f.wsA.Slug, "slug": f.wsB.Slug}, http.StatusConflict, "workspace_slug_taken")
	rename := map[string]any{"expected_slug": f.wsA.Slug, "slug": "new-alpha", "name": "New Alpha"}
	request(ownerToken.Token, path, rename, http.StatusOK, "")
	request(ownerToken.Token, path, rename, http.StatusOK, "")
	request(ownerToken.Token, path, map[string]any{"expected_slug": f.wsA.Slug, "slug": "third-alpha"}, http.StatusConflict, "workspace_rename_stale")
	for _, slug := range []string{f.wsA.Slug, "new-alpha"} {
		rr := doRequestWithBearer(f.srv, http.MethodGet, "/api/v1/workspaces/"+slug, ownerToken.Token, nil)
		var ws models.Workspace
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &ws) != nil || ws.ID != f.wsA.ID || ws.Slug != "new-alpha" || ws.Name != "New Alpha" {
			t.Fatalf("GET %s: %d %s", slug, rr.Code, rr.Body.String())
		}
		rr = doRequestWithBearer(f.srv, http.MethodGet, "/api/v1/workspaces/"+slug+"/items/"+f.itemA.Slug, ownerToken.Token, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), f.itemA.ID) {
			t.Fatalf("item via %s: %d %s", slug, rr.Code, rr.Body.String())
		}
	}
}

func TestWorkspaceRenamePreservesScopedAuthorization(t *testing.T) {
	f := newCrossWSFixture(t)
	tok, err := f.srv.store.CreateAPIToken(f.ownerBoth.ID, models.APITokenCreate{Name: "owner"}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithBearer(f.srv, http.MethodPatch, "/api/v1/workspaces/"+f.wsA.Slug+"/slug", tok.Token, map[string]any{"expected_slug": f.wsA.Slug, "slug": "renamed-alpha"})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rr.Code, rr.Body.String())
	}
	for _, scoped := range []struct {
		name    string
		options reqOpts
	}{
		{"legacy token", reqOpts{bearer: true, noUser: true, tokenWorkspaceID: f.wsA.ID}},
		{"OAuth allowlist", reqOpts{bearer: true, setAllowed: true, allowed: []string{"renamed-alpha"}}},
	} {
		t.Run(scoped.name, func(t *testing.T) {
			r := f.request(f.ownerBoth, scoped.options)
			for _, slug := range []string{f.wsA.Slug, "renamed-alpha"} {
				verdict := f.srv.AuthorizeCrossWorkspaceEdit(r, slug, CrossWorkspaceCollectionScope(f.collA.ID))
				if !verdict.Allowed {
					t.Errorf("lost access via %s: %+v", slug, verdict)
				}
			}
			verdict := f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID))
			if verdict.Allowed {
				t.Errorf("scope broadened to other workspace: %+v", verdict)
			}
		})
	}
}

func TestWorkspaceRenameOAuthScope(t *testing.T) {
	srv := twoResourceOAuthServer(t)
	user, sessionToken := loginTestUser(t, srv)
	sess := oauthSession{sessionToken: sessionToken, csrfTok: readCSRFFromCookie(t, srv, sessionToken), clientID: registerTestClient(t, srv, "https://app.test/cb")}
	ws := mustCreateOwnedWorkspace(t, srv, "OAuth Rename", user)
	other := mustCreateOwnedWorkspace(t, srv, "OAuth Other", user)
	token, rid := legacyGrant(t, srv, sess, []string{ws.Slug})
	renamed, _, err := srv.store.RenameWorkspace(ws.ID, ws.Slug, "oauth-renamed", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"persisted connection", "legacy session"} {
		if mode == "legacy session" {
			if err := srv.store.DeleteOAuthConnection(rid); err != nil {
				t.Fatal(err)
			}
		}
		allowed := allowListAt(t, srv, token)
		if len(allowed) != 1 || allowed[0] != renamed.Slug {
			t.Fatalf("%s allowlist = %v, want only %s (not %s)", mode, allowed, renamed.Slug, other.Slug)
		}
	}
}

func TestWorkspaceRenameNeedsAlias(t *testing.T) {
	f := newNfx(t, testServer(t))
	alice, stranger := f.user("renamealice"), f.user("renamestranger")
	ws := f.workspace("Needs Rename", alice)
	c := f.collection(ws, "Questions", true)
	f.item(ws, c, "Needs an answer", nil, time.Now().Add(-time.Hour))
	if _, _, err := f.srv.store.RenameWorkspace(ws.ID, ws.Slug, "needs-renamed", nil); err != nil {
		t.Fatal(err)
	}
	old := f.need(alice, "?workspace="+ws.Slug)
	current := f.need(alice, "?workspace=needs-renamed")
	if old.Total != 1 || current.Total != 1 || len(old.Workspaces) != 1 || old.Workspaces[0].Slug != "needs-renamed" {
		t.Fatalf("alias=%+v current=%+v", old, current)
	}
	if got := f.need(stranger, "?workspace="+ws.Slug); got.Total != 0 || len(got.Workspaces) != 0 {
		t.Fatalf("alias exposed workspace to stranger: %+v", got)
	}
}

func TestWorkspaceRenameTabsAlias(t *testing.T) {
	f := newTabsFixture(t)
	ws := mustCreateOwnedWorkspace(t, f.srv, "Tabs Rename", f.user)
	other := mustCreateOwnedWorkspace(t, f.srv, "Tabs Other", f.user)
	if _, _, err := f.srv.store.RenameWorkspace(ws.ID, ws.Slug, "tabs-renamed", nil); err != nil {
		t.Fatal(err)
	}
	code, tabs, _ := f.do(t, "POST", "/", map[string]any{"slug": ws.Slug})
	if code != http.StatusOK || tabSlugs(tabs) != "tabs-renamed" {
		t.Fatalf("open alias: %d %+v", code, tabs)
	}
	f.do(t, "POST", "/", map[string]any{"slug": other.Slug})
	code, tabs, _ = f.do(t, "PUT", "/", []string{other.Slug, ws.Slug})
	if code != http.StatusOK || tabSlugs(tabs) != other.Slug+",tabs-renamed" {
		t.Fatalf("reorder alias: %d %+v", code, tabs)
	}
	owner := tabs.Tabs[1].OwnerUsername
	route := "/" + owner + "/" + ws.Slug + "/tasks?view=board#detail"
	code, tabs, _ = f.do(t, "PATCH", "/"+ws.Slug, map[string]any{"last_route": route})
	if code != http.StatusOK || tabs.Tabs[1].LastRoute != route {
		t.Fatalf("patch alias route: %d %+v", code, tabs)
	}
	bad := "/" + owner + "/" + ws.Slug + "/../" + other.Slug
	if code, _, _ := f.do(t, "PATCH", "/"+ws.Slug, map[string]any{"last_route": bad}); code != http.StatusBadRequest {
		t.Fatalf("unsafe route accepted: %d", code)
	}
	code, tabs, _ = f.do(t, "DELETE", "/"+ws.Slug, nil)
	if code != http.StatusOK || tabSlugs(tabs) != other.Slug {
		t.Fatalf("close alias: %d %+v", code, tabs)
	}
}
