package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3339: a share link must stop resolving the moment its workspace is
// soft-deleted. DeleteWorkspace only sets deleted_at (the purge runs 30 days
// later), and the token lookup never checked the workspace, so a link kept
// serving the deleted workspace's content for the whole restore window.
func TestBUG3339_ShareLinkStopsResolvingWhenWorkspaceDeleted(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Shared WS"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/docs/items",
		map[string]any{"title": "Shared doc", "content": "public body"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	link, err := srv.store.CreateShareLink(ws.ID, "item", item.ID, "view", owner.ID, nil)
	if err != nil || link.Token == "" {
		t.Fatalf("create share link: %v (token %q)", err, link.Token)
	}

	resolve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/s/"+link.Token, nil)
		req.RemoteAddr = "198.51.100.7:1234"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	unknown := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/s/not-a-real-token", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	rec := resolve()
	if rec.Code != http.StatusOK {
		t.Fatalf("precondition: the link should resolve, got %d %s", rec.Code, rec.Body.String())
	}
	// No answer may be cached, or a cache would outlive the deletion.
	for name, r := range map[string]*httptest.ResponseRecorder{"200": rec, "404": unknown()} {
		if cc := r.Header().Get("Cache-Control"); cc != "private, no-store" {
			t.Errorf("%s answer Cache-Control = %q, want private, no-store", name, cc)
		}
	}

	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	gone := unknown()
	if rec := resolve(); rec.Code != gone.Code || rec.Body.String() != gone.Body.String() {
		t.Errorf("after the workspace was deleted the link answers %d %s, want the unknown-token answer %d %s",
			rec.Code, rec.Body.String(), gone.Code, gone.Body.String())
	}
	// Both public share routes (/s/{token} and /s/{token}/attachments/{id})
	// resolve the token through this one lookup, so asserting it covers the
	// attachment route too (a made-up attachment ID would 404 regardless).
	if got, err := srv.store.GetShareLinkByToken(link.Token); err != nil || got != nil {
		t.Errorf("GetShareLinkByToken after the workspace was deleted = %v, %v; want nil, nil", got, err)
	}

	// Restoring the workspace inside the window brings the link back: it was
	// never destroyed, only unreachable.
	if err := srv.store.RestoreWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if rec := resolve(); rec.Code != http.StatusOK {
		t.Errorf("after the workspace was restored the link answers %d %s, want 200", rec.Code, rec.Body.String())
	}
}
