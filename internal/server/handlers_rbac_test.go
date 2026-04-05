package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// rbacTestEnv holds everything needed for RBAC tests:
// a server with an admin, workspace, and users with different roles.
type rbacTestEnv struct {
	srv        *Server
	wsSlug     string
	ownerToken string
	editorToken string
	viewerToken string
}

func setupRBACEnv(t *testing.T) *rbacTestEnv {
	t.Helper()
	srv := testServer(t)

	// Bootstrap admin user
	ownerToken := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")

	// Create workspace
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "RBAC Test",
	}, ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	// Register editor user
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "editor@test.com",
		"name":     "Editor",
		"password": "password123",
	}, ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register editor: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Register viewer user
	rr = doRequestWithCookie(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "viewer@test.com",
		"name":     "Viewer",
		"password": "password123",
	}, ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register viewer: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Look up users and add to workspace with roles
	editorUser, err := srv.store.GetUserByEmail("editor@test.com")
	if err != nil || editorUser == nil {
		t.Fatal("failed to find editor user")
	}
	viewerUser, err := srv.store.GetUserByEmail("viewer@test.com")
	if err != nil || viewerUser == nil {
		t.Fatal("failed to find viewer user")
	}

	if err := srv.store.AddWorkspaceMember(ws.ID, editorUser.ID, "editor"); err != nil {
		t.Fatalf("add editor member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, viewerUser.ID, "viewer"); err != nil {
		t.Fatalf("add viewer member: %v", err)
	}

	// Log in as editor
	editorToken := loginUser(t, srv, "editor@test.com", "password123")
	// Log in as viewer
	viewerToken := loginUser(t, srv, "viewer@test.com", "password123")

	return &rbacTestEnv{
		srv:         srv,
		wsSlug:      ws.Slug,
		ownerToken:  ownerToken,
		editorToken: editorToken,
		viewerToken: viewerToken,
	}
}

func loginUser(t *testing.T, srv *Server, email, password string) string {
	t.Helper()
	var bodyReader io.Reader
	data, _ := json.Marshal(map[string]string{"email": email, "password": password})
	bodyReader = bytes.NewReader(data)
	req := httptest.NewRequest("POST", "/api/v1/auth/login", bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login %s: expected 200, got %d: %s", email, rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	parseJSON(t, rr, &resp)
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("login %s: no token in response", email)
	}
	return token
}

func TestRBAC_ViewerBlockedFromItemMutations(t *testing.T) {
	env := setupRBACEnv(t)

	// Create an item as owner first
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title":   "Test Item",
		"content": "Content",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: create item failed: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]interface{}
	parseJSON(t, rr, &item)
	itemSlug := item["slug"].(string)

	tests := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"create item", "POST", "/api/v1/workspaces/" + env.wsSlug + "/collections/docs/items",
			map[string]interface{}{"title": "New Item"}},
		{"update item", "PATCH", "/api/v1/workspaces/" + env.wsSlug + "/items/" + itemSlug,
			map[string]interface{}{"title": "Updated"}},
		{"delete item", "DELETE", "/api/v1/workspaces/" + env.wsSlug + "/items/" + itemSlug, nil},
		{"restore item", "POST", "/api/v1/workspaces/" + env.wsSlug + "/items/" + itemSlug + "/restore", nil},
		{"move item", "POST", "/api/v1/workspaces/" + env.wsSlug + "/items/" + itemSlug + "/move",
			map[string]interface{}{"collection_slug": "ideas"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := doRequestWithCookie(env.srv, tt.method, tt.path, tt.body, env.viewerToken)
			if rr.Code != http.StatusForbidden {
				t.Errorf("expected 403 for viewer %s, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRBAC_EditorAllowedItemMutations(t *testing.T) {
	env := setupRBACEnv(t)

	// Editor can create items
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title":   "Editor Item",
		"content": "Content",
	}, env.editorToken)
	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201 for editor create item, got %d: %s", rr.Code, rr.Body.String())
	}

	var item map[string]interface{}
	parseJSON(t, rr, &item)
	itemSlug := item["slug"].(string)

	// Editor can update items (update content, not title, to preserve slug)
	rr = doRequestWithCookie(env.srv, "PATCH", "/api/v1/workspaces/"+env.wsSlug+"/items/"+itemSlug, map[string]interface{}{
		"content": "Updated by editor",
	}, env.editorToken)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for editor update item, got %d: %s", rr.Code, rr.Body.String())
	}

	// Editor can delete items
	rr = doRequestWithCookie(env.srv, "DELETE", "/api/v1/workspaces/"+env.wsSlug+"/items/"+itemSlug, nil, env.editorToken)
	if rr.Code != http.StatusNoContent {
		t.Errorf("expected 204 for editor delete item, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRBAC_EditorBlockedFromOwnerOperations(t *testing.T) {
	env := setupRBACEnv(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"create collection", "POST", "/api/v1/workspaces/" + env.wsSlug + "/collections",
			map[string]interface{}{"name": "Custom", "schema": `{"fields":[]}`}},
		{"update workspace", "PATCH", "/api/v1/workspaces/" + env.wsSlug,
			map[string]interface{}{"name": "Updated"}},
		{"delete workspace", "DELETE", "/api/v1/workspaces/" + env.wsSlug, nil},
		{"export workspace", "GET", "/api/v1/workspaces/" + env.wsSlug + "/export", nil},
		{"create webhook", "POST", "/api/v1/workspaces/" + env.wsSlug + "/webhooks",
			map[string]interface{}{"url": "http://example.com", "events": []string{"item.created"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := doRequestWithCookie(env.srv, tt.method, tt.path, tt.body, env.editorToken)
			if rr.Code != http.StatusForbidden {
				t.Errorf("expected 403 for editor %s, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRBAC_OwnerAllowedEverything(t *testing.T) {
	env := setupRBACEnv(t)

	// Owner can create items
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title":   "Owner Item",
		"content": "Content",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201 for owner create item, got %d: %s", rr.Code, rr.Body.String())
	}

	// Owner can create collections
	rr = doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections", map[string]interface{}{
		"name":   "Custom",
		"schema": `{"fields":[]}`,
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201 for owner create collection, got %d: %s", rr.Code, rr.Body.String())
	}

	// Owner can update workspace
	rr = doRequestWithCookie(env.srv, "PATCH", "/api/v1/workspaces/"+env.wsSlug, map[string]interface{}{
		"name": "Updated by Owner",
	}, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for owner update workspace, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRBAC_ViewerBlockedFromDocumentMutations(t *testing.T) {
	env := setupRBACEnv(t)

	// Create a doc as owner
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/documents", map[string]interface{}{
		"title":   "Test Doc",
		"content": "Content",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: create doc failed: %d %s", rr.Code, rr.Body.String())
	}
	var doc map[string]interface{}
	parseJSON(t, rr, &doc)
	docID := doc["id"].(string)

	tests := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"create document", "POST", "/api/v1/workspaces/" + env.wsSlug + "/documents",
			map[string]interface{}{"title": "New Doc"}},
		{"update document", "PATCH", "/api/v1/workspaces/" + env.wsSlug + "/documents/" + docID,
			map[string]interface{}{"content": "Updated"}},
		{"delete document", "DELETE", "/api/v1/workspaces/" + env.wsSlug + "/documents/" + docID, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := doRequestWithCookie(env.srv, tt.method, tt.path, tt.body, env.viewerToken)
			if rr.Code != http.StatusForbidden {
				t.Errorf("expected 403 for viewer %s, got %d: %s", tt.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRBAC_ViewerBlockedFromCommentCreation(t *testing.T) {
	env := setupRBACEnv(t)

	// Create item as owner
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title": "Commented Item",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: create item failed: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]interface{}
	parseJSON(t, rr, &item)
	itemSlug := item["slug"].(string)

	rr = doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/items/"+itemSlug+"/comments", map[string]interface{}{
		"body": "Hello from viewer",
	}, env.viewerToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for viewer create comment, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRBAC_ViewerCanReadEverything(t *testing.T) {
	env := setupRBACEnv(t)

	// Create items/data as owner
	doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title": "Readable",
	}, env.ownerToken)

	// Viewer can list collections
	rr := doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/collections", nil, env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for viewer list collections, got %d", rr.Code)
	}

	// Viewer can list items
	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", nil, env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for viewer list items, got %d", rr.Code)
	}

	// Viewer can get workspace
	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug, nil, env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for viewer get workspace, got %d", rr.Code)
	}
}

func TestRBAC_EditorBlockedFromAgentRoleMutations(t *testing.T) {
	env := setupRBACEnv(t)

	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/agent-roles", map[string]interface{}{
		"name": "Test Role",
	}, env.editorToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for editor create agent role, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRBAC_ViewerBlockedFromItemLinkMutations(t *testing.T) {
	env := setupRBACEnv(t)

	// Create two items as owner
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title": "Item A",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: create item A failed: %d %s", rr.Code, rr.Body.String())
	}
	var itemA map[string]interface{}
	parseJSON(t, rr, &itemA)
	itemASlug := itemA["slug"].(string)

	rr = doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title": "Item B",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: create item B failed: %d %s", rr.Code, rr.Body.String())
	}
	var itemB map[string]interface{}
	parseJSON(t, rr, &itemB)
	itemBID := itemB["id"].(string)

	rr = doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/items/"+itemASlug+"/links", map[string]interface{}{
		"target_id": itemBID,
		"link_type": "blocks",
	}, env.viewerToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for viewer create item link, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRBAC_SearchScopedToUserWorkspaces(t *testing.T) {
	env := setupRBACEnv(t)

	// Create an item in the workspace the editor belongs to
	doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items", map[string]interface{}{
		"title":   "Visible Secret",
		"content": "This should be found by the editor",
	}, env.ownerToken)

	// Create a second workspace that the editor does NOT belong to
	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Private Workspace",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create private workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var privateWS map[string]interface{}
	parseJSON(t, rr, &privateWS)
	privateSlug := privateWS["slug"].(string)

	// Create an item in the private workspace
	doRequestWithCookie(env.srv, "POST", "/api/v1/workspaces/"+privateSlug+"/collections/docs/items", map[string]interface{}{
		"title":   "Hidden Secret",
		"content": "This should NOT be found by the editor",
	}, env.ownerToken)

	// Editor searches without workspace param — should only see their workspace's items
	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/search?q=Secret", nil, env.editorToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Results []map[string]interface{} `json:"results"`
		Total   int                      `json:"total"`
	}
	parseJSON(t, rr, &resp)

	// Should find only the visible item, not the one in the private workspace
	if resp.Total != 1 {
		t.Errorf("expected 1 result (only from editor's workspace), got %d", resp.Total)
		for _, r := range resp.Results {
			if item, ok := r["item"].(map[string]interface{}); ok {
				t.Logf("  found: %v", item["title"])
			}
		}
	}

	// Owner searches without workspace param — should see items from both workspaces
	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/search?q=Secret", nil, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner search: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &resp)
	if resp.Total != 2 {
		t.Errorf("expected 2 results for owner (both workspaces), got %d", resp.Total)
	}
}
