package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2696: github_pr is written only through the typed, validated update
// member. A fields_patch entry (every `--field` / MCP `field` setter lowers
// into one) is refused like the other three reserved keys.

func githubPRItem(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Linkable"})
	return srv, "/api/v1/workspaces/" + slug + "/items/" + item.Slug
}

func getCodeContext(t *testing.T, srv *Server, path string) *models.ItemCodeContext {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return item.CodeContext
}

func TestGitHubPRFieldPatchIsRefusedAndNamesTheWriter(t *testing.T) {
	t.Parallel()
	srv, path := githubPRItem(t)
	for _, v := range []any{
		`{"number":7,"url":"https://github.com/o/r/pull/7"}`, // what --field sends: a string
		map[string]any{"number": 7, "url": "https://github.com/o/r/pull/7"},
		nil,
	} {
		rr := doRequest(srv, "PATCH", path, map[string]any{"fields_patch": map[string]any{"github_pr": v}})
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "pad github link") {
			t.Fatalf("fields_patch github_pr=%v: want 400 naming `pad github link`, got %d %s", v, rr.Code, rr.Body.String())
		}
	}
	if cc := getCodeContext(t, srv, path); cc != nil {
		t.Fatalf("a refused write left a code context: %+v", cc)
	}
}

func TestGitHubPRTypedMemberWritesAReadableLinkAndClears(t *testing.T) {
	t.Parallel()
	srv, path := githubPRItem(t)

	rr := doRequest(srv, "PATCH", path, map[string]any{"github_pr": map[string]any{
		"number": 7, "url": "https://github.com/o/r/pull/7", "state": "OPEN", "branch": "b",
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("typed write: %d %s", rr.Code, rr.Body.String())
	}
	cc := getCodeContext(t, srv, path)
	if cc == nil || cc.PullRequest == nil || cc.PullRequest.Number != 7 || cc.Branch != "b" {
		t.Fatalf("the typed write did not render a link: %+v", cc)
	}

	rr = doRequest(srv, "PATCH", path, map[string]any{"clear_github_pr": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	if cc := getCodeContext(t, srv, path); cc != nil {
		t.Fatalf("clear_github_pr left a link: %+v", cc)
	}
}

func TestGitHubPRTypedMemberIsValidated(t *testing.T) {
	t.Parallel()
	srv, path := githubPRItem(t)
	cases := map[string]map[string]any{
		"no number":        {"github_pr": map[string]any{"url": "https://github.com/o/r/pull/7"}},
		"relative url":     {"github_pr": map[string]any{"number": 7, "url": "/o/r/pull/7"}},
		"with its clear":   {"github_pr": map[string]any{"number": 7, "url": "https://x/p/7"}, "clear_github_pr": true},
		"with full fields": {"github_pr": map[string]any{"number": 7, "url": "https://x/p/7"}, "fields": `{"status":"open"}`},
	}
	for name, body := range cases {
		if rr := doRequest(srv, "PATCH", path, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if cc := getCodeContext(t, srv, path); cc != nil {
		t.Fatalf("a refused typed write left a link: %+v", cc)
	}
}
