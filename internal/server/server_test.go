package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s)
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

	var resp map[string]string
	parseJSON(t, rr, &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
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
