package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2348 checkpoint 2, defect 3. The update handler never gave the version
// row a source, so UpdateItem's "web" default labelled every body edit as a
// web edit: an agent's PATCH over a bearer token read "Web" in the Versions
// panel. Each leg writes a body through the real route and reads the version
// row that write produced, identified by the body it holds (a version row
// stores the body from BEFORE its edit).
func TestUpdateItem_VersionSourceIsTheRequestSource(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	path := "/api/v1/workspaces/" + ws + "/items/" + slug

	// Bearer + X-Pad-Agent: the CLI/agent door.
	var item models.Item
	rr := authedAgentRequest(t, srv, token, "wren", "PATCH", path, map[string]any{"content": "agent body\n"})
	decodeAttributionBody(t, rr, &item)

	// Cookie session: the web door, on the same account.
	data, _ := json.Marshal(map[string]any{"content": "web body\n"})
	req := httptest.NewRequest("PATCH", path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	web := httptest.NewRecorder()
	srv.ServeHTTP(web, req)
	if web.Code != http.StatusOK {
		t.Fatalf("cookie PATCH = %d: %s", web.Code, web.Body.String())
	}

	versions, err := srv.store.ListItemVersionsResolved(item.ID, "web body\n")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	// The body each write REPLACED names the version row that write made.
	want := map[string]struct{ source, createdBy string }{
		"":             {"cli", "agent"}, // the agent's edit replaced the empty create body
		"agent body\n": {"web", "user"},
	}
	seen := 0
	for _, v := range versions {
		w, ok := want[v.Content]
		if !ok {
			continue
		}
		seen++
		if v.Source != w.source {
			t.Errorf("version holding %q: source = %q, want %q", v.Content, v.Source, w.source)
		}
		if v.CreatedBy != w.createdBy {
			t.Errorf("version holding %q: created_by = %q, want %q", v.Content, v.CreatedBy, w.createdBy)
		}
	}
	if seen != len(want) {
		t.Fatalf("found %d of %d expected version rows; versions: %+v", seen, len(want), versions)
	}
}
