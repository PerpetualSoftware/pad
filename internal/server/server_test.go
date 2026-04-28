package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	srv := New(s)
	// Drain background goroutines BEFORE closing the store. Without this,
	// fire-and-forget writes spawned by request middleware (e.g.
	// TouchUserActivity in middleware_auth) can race the SQLite WAL/SHM
	// file removal in t.TempDir's cleanup, causing
	// "TempDir RemoveAll cleanup: directory not empty" — see BUG-842.
	t.Cleanup(func() {
		srv.Stop()
		s.Close()
	})
	return srv
}

func doRequest(srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	return doRequestFromRemoteAddr(srv, method, path, body, "192.0.2.1:1234")
}

func doLoopbackRequest(srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	return doRequestFromRemoteAddr(srv, method, path, body, "127.0.0.1:1234")
}

func doRequestFromRemoteAddr(srv *Server, method, path string, body interface{}, remoteAddr string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = remoteAddr
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func parseJSON(t *testing.T, rr *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("failed to parse JSON response: %v\nBody: %s", err, rr.Body.String())
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/health", nil)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", resp["status"])
	}
	// cloud_mode should default to false when not configured
	if resp["cloud_mode"] != false {
		t.Errorf("expected cloud_mode false, got %v", resp["cloud_mode"])
	}
}

func TestWorkspaceEndpoints(t *testing.T) {
	srv := testServer(t)

	// Create
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "My Project",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if ws.Slug != "my-project" {
		t.Errorf("expected slug 'my-project', got %q", ws.Slug)
	}
	if ws.Context != nil {
		t.Fatalf("did not expect structured context on a default workspace, got %#v", ws.Context)
	}

	// List
	rr = doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list workspaces: expected 200, got %d", rr.Code)
	}

	var wsList []models.Workspace
	parseJSON(t, rr, &wsList)
	if len(wsList) != 1 {
		t.Errorf("expected 1 workspace, got %d", len(wsList))
	}

	// Get
	rr = doRequest(srv, "GET", "/api/v1/workspaces/my-project", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get workspace: expected 200, got %d", rr.Code)
	}

	// Update
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/my-project", map[string]string{
		"name": "Updated Project",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update workspace: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Delete
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/my-project", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete workspace: expected 204, got %d", rr.Code)
	}

	// Should be gone
	rr = doRequest(srv, "GET", "/api/v1/workspaces/my-project", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for deleted workspace, got %d", rr.Code)
	}
}

func TestWorkspaceValidation(t *testing.T) {
	srv := testServer(t)

	// Missing name
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rr.Code)
	}

	// Not found
	rr = doRequest(srv, "GET", "/api/v1/workspaces/nonexistent", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces", map[string]interface{}{
		"name":     "Bad Settings",
		"settings": `{"context":`,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid settings JSON, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceContextAPI(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]interface{}{
		"name": "Contextual",
		"context": map[string]interface{}{
			"repositories": []map[string]string{
				{"name": "docapp", "role": "primary", "path": ".", "repo": "PerpetualSoftware/pad"},
			},
			"commands": map[string]string{
				"build": "make install",
				"test":  "go test ./...",
			},
			"deployment": map[string]string{
				"mode":     "local",
				"base_url": "http://127.0.0.1:7777",
			},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace with context: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if ws.Context == nil || ws.Context.Commands == nil {
		t.Fatalf("expected workspace context in create response, got %#v", ws.Context)
	}
	if ws.Context.Commands.Build != "make install" {
		t.Fatalf("expected build command in context, got %#v", ws.Context.Commands)
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/contextual", map[string]interface{}{
		"context": map[string]interface{}{
			"assumptions": []string{"pad-web lives at ../pad-web"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update workspace context: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var updated models.Workspace
	parseJSON(t, rr, &updated)
	if updated.Context == nil {
		t.Fatal("expected context after update")
	}
	if len(updated.Context.Assumptions) != 1 {
		t.Fatalf("expected assumptions after update, got %#v", updated.Context.Assumptions)
	}
	if updated.Context.Commands != nil {
		t.Fatalf("expected context replacement semantics on update, got %#v", updated.Context.Commands)
	}
}

func createWSForTest(t *testing.T, srv *Server) string {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Test"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("failed to create test workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	return ws.Slug
}

func TestDocumentEndpoints(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Create
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":    "My Doc",
		"content":  "Hello world",
		"doc_type": "notes",
		"status":   "active",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create doc: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var doc models.Document
	parseJSON(t, rr, &doc)
	if doc.Title != "My Doc" {
		t.Errorf("expected title 'My Doc', got %q", doc.Title)
	}

	// Get
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get doc: expected 200, got %d", rr.Code)
	}

	// List
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/documents", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list docs: expected 200, got %d", rr.Code)
	}

	var docs []models.Document
	parseJSON(t, rr, &docs)
	if len(docs) != 1 {
		t.Errorf("expected 1 doc, got %d", len(docs))
	}

	// Update
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID, map[string]interface{}{
		"content": "Updated content",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update doc: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Delete
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete doc: expected 204, got %d", rr.Code)
	}

	// Restore
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID+"/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore doc: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentValidation(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Missing title
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"content": "No title",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", rr.Code)
	}

	// Invalid doc_type
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":    "Doc",
		"doc_type": "invalid",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid doc_type, got %d", rr.Code)
	}

	// Invalid status
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":  "Doc",
		"status": "invalid",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid status, got %d", rr.Code)
	}

	// Not found
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/documents/nonexistent-id", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestSearchEndpoint(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items", map[string]interface{}{
		"title":   "Auth Architecture",
		"content": "OAuth2 authentication flow",
	})
	doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items", map[string]interface{}{
		"title":   "Data Model",
		"content": "Database schema",
	})

	rr := doRequest(srv, "GET", "/api/v1/search?q=authentication&workspace="+slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Results []store.SearchResult `json:"results"`
		Total   int                  `json:"total"`
	}
	parseJSON(t, rr, &resp)
	if resp.Total != 1 {
		t.Errorf("expected 1 result, got %d", resp.Total)
	}

	// Missing query
	rr = doRequest(srv, "GET", "/api/v1/search", nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing query, got %d", rr.Code)
	}
}

func TestActivityEndpoints(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Create a doc — should log activity
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/documents", map[string]interface{}{
		"title":   "Doc",
		"content": "Content",
	})
	var doc models.Document
	parseJSON(t, rr, &doc)

	// Update — should log activity
	doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID, map[string]interface{}{
		"content": "Updated",
	})

	// Workspace activity
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/activity", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("workspace activity: expected 200, got %d", rr.Code)
	}

	var activities []models.Activity
	parseJSON(t, rr, &activities)
	if len(activities) < 2 {
		t.Errorf("expected at least 2 activities, got %d", len(activities))
	}

	// Document activity
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/documents/"+doc.ID+"/activity", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("doc activity: expected 200, got %d", rr.Code)
	}
}

// TestServer_Stop_DrainsRateLimiterCleanup pins BUG-851: NewRateLimiters
// spawns 9 cleanup goroutines (one per ipRateLimiter), each in a
// 5-minute Sleep loop. Without explicit drain on Stop(), these leak
// across every testServer lifecycle and (under -race) push the
// internal/server suite past the 10-minute test timeout — see the
// goroutine dump in BUG-851.
//
// This test exercises N construct-then-Stop cycles and asserts that the
// number of live goroutines settles back to baseline. We allow a small
// tolerance for the test runtime's own bookkeeping goroutines.
func TestServer_Stop_DrainsRateLimiterCleanup(t *testing.T) {
	// Let any prior test scheduler activity quiesce so the baseline
	// reading is meaningful. Two short waits + a GC pass — anything
	// stronger and the test becomes its own flake.
	settle := func() {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
	}
	settle()
	baseline := runtime.NumGoroutine()

	const cycles = 5
	for i := 0; i < cycles; i++ {
		// Use the bare New(s) constructor (not testServer) so we control
		// when Stop runs and don't entangle with t.Cleanup ordering.
		dir := t.TempDir()
		s, err := store.New(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		srv := New(s)

		// Each NewRateLimiters spawns 9 cleanup goroutines.
		if got := runtime.NumGoroutine(); got < baseline+9 {
			t.Fatalf("cycle %d: expected at least baseline+9 goroutines after New(), got %d (baseline %d)",
				i, got, baseline)
		}

		srv.Stop()
		s.Close()
	}

	settle()
	final := runtime.NumGoroutine()

	// Allow ±3 for Go runtime/test-framework noise (timer wheels,
	// finalizers, etc.). A leak of 9 per cycle would put us at
	// baseline + 9*5 = baseline + 45 — comfortably outside the tolerance.
	if final > baseline+3 {
		t.Errorf("goroutine leak: baseline=%d, after %d Stop cycles=%d (delta %d > 3)",
			baseline, cycles, final, final-baseline)
	}
}

// TestServer_Stop_DrainsBackgroundGoroutines pins BUG-842 part 2: any
// fire-and-forget goroutine spawned via Server.goAsync must be drained by
// Server.Stop() before the call returns, so test cleanup (and graceful
// shutdown) can close the SQLite store without racing in-flight WAL writes
// against t.TempDir's RemoveAll.
func TestServer_Stop_DrainsBackgroundGoroutines(t *testing.T) {
	srv := testServer(t)

	// Block the goroutine on a release channel so we can prove Stop() is
	// actually waiting (not just racing past a goroutine that already
	// finished). A naive `go func(){...}()` would let Stop return
	// immediately while the goroutine is still running — that's the bug.
	release := make(chan struct{})
	var done atomic.Bool
	srv.goAsync(func() {
		<-release
		done.Store(true)
	})

	// Stop() must block until the goroutine sees release + sets done.
	// Run Stop in a goroutine so the test can release after a brief wait.
	stopReturned := make(chan struct{})
	go func() {
		srv.Stop()
		close(stopReturned)
	}()

	// Stop should NOT have returned yet — the goAsync goroutine is blocked.
	select {
	case <-stopReturned:
		t.Fatal("Server.Stop() returned before background goroutine finished")
	case <-time.After(50 * time.Millisecond):
		// Expected: still waiting.
	}

	close(release)

	select {
	case <-stopReturned:
		// Expected: Stop drained the goroutine and returned.
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Stop() did not return within 2s of releasing the goroutine")
	}

	if !done.Load() {
		t.Fatal("background goroutine did not run to completion before Stop returned")
	}
}
