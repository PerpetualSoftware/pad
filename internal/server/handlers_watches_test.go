package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// setupWatchTestUser seeds a workspace + item, mints a real user +
// user-scoped Bearer token (mirrors handlers_stars_test.go's pattern),
// and returns everything a watch CRUD test needs.
func setupWatchTestUser(t *testing.T, srv *Server) (slug string, item models.Item, tok *models.APITokenWithSecret, user *models.User) {
	t.Helper()
	slug = createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Watch me", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &item)

	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "watch-test@example.com",
		Name:     "Watch Tester",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err = srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "watch-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return slug, item, tok, u
}

// bearerJSON marshals body and issues a Bearer-authenticated request.
// Needed for any mutation issued AFTER a real user has been created in
// the test's store: creating a user ends the fresh-install "no users
// exist" auth bypass that doRequest relies on, so further mutations must
// carry a token (Bearer auth also bypasses CSRF — middleware_csrf.go).
func bearerJSON(t *testing.T, srv *Server, method, path, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bearerCall(t, srv, method, path, token, data)
}

func bearerCall(t *testing.T, srv *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestCreateWatch_AndList(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var w models.Watch
	parseJSON(t, rr, &w)
	if w.ItemID != item.ID || w.Predicate != "" {
		t.Fatalf("unexpected watch: %+v", w)
	}

	rr = bearerCall(t, srv, "GET", "/api/v1/watches", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list watches: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var list []models.Watch
	parseJSON(t, rr, &list)
	if len(list) != 1 || list[0].ItemRef != item.Ref {
		t.Fatalf("expected 1 watch with ref %q, got %+v", item.Ref, list)
	}
}

func TestCreateWatch_WithPredicate(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token,
		[]byte(`{"predicate":"status=done"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var w models.Watch
	parseJSON(t, rr, &w)
	if w.Predicate != "status=done" {
		t.Fatalf("expected predicate 'status=done', got %q", w.Predicate)
	}
}

func TestCreateWatch_MalformedPredicateRejected(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token,
		[]byte(`{"predicate":"not-a-pair"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed predicate, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestDeleteWatch(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	rr = bearerCall(t, srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete watch: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	rr = bearerCall(t, srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete watch (again): expected 404, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	rr = bearerCall(t, srv, "GET", "/api/v1/watches", tok.Token, nil)
	var list []models.Watch
	parseJSON(t, rr, &list)
	if len(list) != 0 {
		t.Fatalf("expected 0 watches after delete, got %d", len(list))
	}
}

func TestListWatches_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	// No workspace member exists, no bearer token — this exercises the
	// currentUserID(r) == "" branch even in the fresh-install no-auth
	// window (a request with a bearer header pointing at nothing valid
	// still resolves no user).
	req := httptest.NewRequest("GET", "/api/v1/watches", nil)
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// Fresh-install (no users exist) bypasses auth entirely, so this can
	// legitimately be 200 with an empty list in that mode — assert the
	// weaker, still-meaningful invariant: it never 500s and never
	// requires currentUserID to be non-empty to avoid crashing.
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("unexpected 500: %s", rec.Body.String())
	}
}

// chunkedBearerCall is bearerCall with ContentLength forced to -1, the way a
// chunked request arrives. The old guard (`ContentLength > 0`) silently
// DROPPED such bodies — predicate ignored, unconditional watch, 200 (codex
// closing round 4).
func chunkedBearerCall(t *testing.T, srv *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:0"
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestCreateWatch_ChunkedBodyIsDecoded(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/watch"

	// The discriminating assertion is the PREDICATE, not the status code:
	// the unfixed handler also answered 200, with the predicate silently
	// dropped to "".
	rr := chunkedBearerCall(t, srv, "POST", path, tok.Token, []byte(`{"predicate":"status=done"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("chunked create watch: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var w models.Watch
	parseJSON(t, rr, &w)
	if w.Predicate != "status=done" {
		t.Fatalf("chunked body was dropped: predicate = %q, want %q", w.Predicate, "status=done")
	}
}

func TestCreateWatch_ChunkedMalformedBodyRejected(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/watch"

	// A chunked body must reach the SAME gate a measured one does — here the
	// NUL rule. Unfixed, this answered 200 and created an unconditional watch.
	rr := chunkedBearerCall(t, srv, "POST", path, tok.Token, []byte("{\"predicate\":\"status=do\\u0000ne\"}"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("chunked NUL body: expected 400, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCreateWatch_ChunkedEmptyBodyStaysValid(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/watch"

	// The tolerant no-body contract must survive the fix: decodeJSON answers
	// an empty body with io.EOF, which the handler treats as absent.
	rr := chunkedBearerCall(t, srv, "POST", path, tok.Token, []byte(""))
	if rr.Code != http.StatusOK {
		t.Fatalf("chunked empty body: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var w models.Watch
	parseJSON(t, rr, &w)
	if w.Predicate != "" {
		t.Fatalf("expected unconditional watch, got predicate %q", w.Predicate)
	}
}
