package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/go-chi/chi/v5"
)

// These tests cover BUG-2102: the OAuth consent allow-list was enforced only
// by RequireWorkspaceAccess, which fires solely for /{slug} path-param routes.
// Every MCP-reachable read that is workspace-global (list, deleted, search
// fan-out, audit-log) or takes the workspace as a query/body param (search
// by workspace, restore by slug) bypassed the gate. Each handler now honors
// the allow-list; these tests drive the handlers directly with a context
// carrying WithCurrentUser + WithTokenAllowedWorkspaces (the shape MCPBearerAuth
// produces for a consent-scoped OAuth token — the cookie/PAT test path never
// sets an allow-list).

// consentScopedRequest builds an in-context request as an OAuth-scoped MCP
// call would arrive at a handler: the user on context, an optional allow-list,
// and optional chi URL params. Passing allow == nil models PAT / web-session
// auth (no allow-list → no gate).
func consentScopedRequest(method, path string, user *models.User, allow []string, urlParams map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := WithCurrentUser(req.Context(), user)
	if allow != nil {
		ctx = WithTokenAllowedWorkspaces(ctx, allow)
	}
	if len(urlParams) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range urlParams {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

func mustCreateUser(t *testing.T, srv *Server, email, name, role string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: name, Password: "correct-horse-battery-staple", Role: role,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func mustCreateOwnedWorkspace(t *testing.T, srv *Server, name string, owner *models.User) *models.Workspace {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace %s: %v", name, err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner to %s: %v", name, err)
	}
	return ws
}

func TestHandleListWorkspaces_FiltersByConsentAllowList(t *testing.T) {
	srv := testServer(t)
	user := mustCreateUser(t, srv, "u@test.com", "U", "member")
	alpha := mustCreateOwnedWorkspace(t, srv, "Alpha", user)
	beta := mustCreateOwnedWorkspace(t, srv, "Beta", user)

	list := func(allow []string) []models.Workspace {
		t.Helper()
		req := consentScopedRequest("GET", "/api/v1/workspaces", user, allow, nil)
		rec := httptest.NewRecorder()
		srv.handleListWorkspaces(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out []models.Workspace
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// Consent scoped to alpha: beta must be dropped (the leak BUG-2102 fixes).
	scoped := list([]string{alpha.Slug})
	if containsWorkspace(scoped, beta.ID) {
		t.Error("beta leaked despite consent scoped to alpha")
	}
	if !containsWorkspace(scoped, alpha.ID) {
		t.Error("alpha (consented) should be present")
	}

	// No allow-list (PAT / web session): unchanged — both present.
	all := list(nil)
	if !containsWorkspace(all, alpha.ID) || !containsWorkspace(all, beta.ID) {
		t.Error("without an allow-list both workspaces should be present")
	}

	// Wildcard consent: no per-slug filter — both present.
	wild := list([]string{"*"})
	if !containsWorkspace(wild, alpha.ID) || !containsWorkspace(wild, beta.ID) {
		t.Error("wildcard allow-list must not filter")
	}
}

func TestHandleListDeletedWorkspaces_FiltersByConsentAllowList(t *testing.T) {
	srv := testServer(t)
	user := mustCreateUser(t, srv, "u@test.com", "U", "member")
	ws := mustCreateOwnedWorkspace(t, srv, "Gone", user)
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	deleted := func(allow []string) []deletedWorkspaceEntry {
		t.Helper()
		req := consentScopedRequest("GET", "/api/v1/workspaces/deleted", user, allow, nil)
		rec := httptest.NewRecorder()
		srv.handleListDeletedWorkspaces(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out []deletedWorkspaceEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// No allow-list: the owner sees their soft-deleted workspace.
	var sawIt bool
	for _, e := range deleted(nil) {
		if e.ID == ws.ID {
			sawIt = true
		}
	}
	if !sawIt {
		t.Fatal("owner should see their soft-deleted workspace without a consent gate")
	}

	// Consent scoped to a different slug: the deleted workspace is hidden.
	for _, e := range deleted([]string{"some-other-slug"}) {
		if e.ID == ws.ID {
			t.Error("deleted workspace leaked despite consent scope excluding it")
		}
	}

	// Consent INCLUDING the slug still shows it — guards against a filter
	// that drops everything on any non-nil allow-list (which would pass the
	// nil-shows + other-slug-hides cases above vacuously).
	var sawScoped bool
	for _, e := range deleted([]string{ws.Slug}) {
		if e.ID == ws.ID {
			sawScoped = true
		}
	}
	if !sawScoped {
		t.Error("consent including the workspace slug should still show the soft-deleted workspace")
	}
}

func TestHandleRestoreWorkspace_GatedByConsentAllowList(t *testing.T) {
	srv := testServer(t)
	user := mustCreateUser(t, srv, "owner@test.com", "Owner", "member")
	ws := mustCreateOwnedWorkspace(t, srv, "Gone", user)
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	// Consent scoped to a different workspace: restore is denied (404, so the
	// token can't even confirm the slug exists) and the workspace stays deleted.
	req := consentScopedRequest("POST", "/api/v1/workspaces/"+ws.Slug+"/restore",
		user, []string{"some-other-slug"}, map[string]string{"slug": ws.Slug})
	rec := httptest.NewRecorder()
	srv.handleRestoreWorkspace(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("out-of-consent restore should 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if d, _ := srv.store.GetDeletedWorkspaceBySlug(ws.Slug); d == nil {
		t.Fatal("workspace must remain soft-deleted after a denied restore")
	}

	// Consent including the slug (owner): restore succeeds.
	req2 := consentScopedRequest("POST", "/api/v1/workspaces/"+ws.Slug+"/restore",
		user, []string{ws.Slug}, map[string]string{"slug": ws.Slug})
	rec2 := httptest.NewRecorder()
	srv.handleRestoreWorkspace(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("in-consent owner restore should 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestHandleSearch_ConsentScopesResults(t *testing.T) {
	srv := testServer(t)
	user := mustCreateUser(t, srv, "u@test.com", "U", "member")
	alpha := mustCreateOwnedWorkspace(t, srv, "Alpha", user)
	beta := mustCreateOwnedWorkspace(t, srv, "Beta", user)

	collA, err := srv.store.CreateCollection(alpha.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatalf("collA: %v", err)
	}
	collB, err := srv.store.CreateCollection(beta.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatalf("collB: %v", err)
	}
	if _, err := srv.store.CreateItem(alpha.ID, collA.ID, models.ItemCreate{Title: "zebrafish alpha", CreatedBy: "user", Source: "test"}); err != nil {
		t.Fatalf("item alpha: %v", err)
	}
	if _, err := srv.store.CreateItem(beta.ID, collB.ID, models.ItemCreate{Title: "zebrafish beta", CreatedBy: "user", Source: "test"}); err != nil {
		t.Fatalf("item beta: %v", err)
	}

	search := func(workspace string, allow []string) store.SearchResponse {
		t.Helper()
		path := "/api/v1/search?q=zebrafish"
		if workspace != "" {
			path += "&workspace=" + workspace
		}
		req := consentScopedRequest("GET", path, user, allow, nil)
		rec := httptest.NewRecorder()
		srv.handleSearch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("search status %d: %s", rec.Code, rec.Body.String())
		}
		var resp store.SearchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	hasWorkspace := func(resp store.SearchResponse, wsID string) bool {
		for _, r := range resp.Results {
			if r.Item.WorkspaceID == wsID {
				return true
			}
		}
		return false
	}

	// Baseline (no allow-list): fan-out finds items in BOTH workspaces. If this
	// fails the FTS seed is broken, not the consent gate.
	base := search("", nil)
	if !hasWorkspace(base, alpha.ID) || !hasWorkspace(base, beta.ID) {
		t.Fatalf("baseline fan-out search should find both workspaces' items; got %d results", len(base.Results))
	}

	// Fan-out consent-scoped to alpha: beta's item CONTENT must not appear.
	scoped := search("", []string{alpha.Slug})
	if hasWorkspace(scoped, beta.ID) {
		t.Error("beta item leaked in a consent-scoped fan-out search (HIGH leak)")
	}
	if !hasWorkspace(scoped, alpha.ID) {
		t.Error("alpha's own item should still be found under consent scoped to alpha")
	}

	// Explicit workspace=beta while consent-scoped to alpha: empty, no leak.
	denied := search(beta.Slug, []string{alpha.Slug})
	if len(denied.Results) != 0 {
		t.Errorf("searching a non-consented workspace by name should return empty; got %d results", len(denied.Results))
	}

	// Explicit workspace=beta with matching consent: results returned.
	allowed := search(beta.Slug, []string{beta.Slug})
	if !hasWorkspace(allowed, beta.ID) {
		t.Error("searching a consented workspace by name should return its items")
	}
}

func TestHandleAuditLog_DeniedForConsentScopedToken(t *testing.T) {
	srv := testServer(t)
	admin := mustCreateUser(t, srv, "admin@test.com", "Admin", "admin")

	code := func(allow []string) int {
		t.Helper()
		req := consentScopedRequest("GET", "/api/v1/audit-log", admin, allow, nil)
		rec := httptest.NewRecorder()
		srv.handleAuditLog(rec, req)
		return rec.Code
	}

	// Consent-scoped (specific allow-list): the platform-wide log is denied.
	if got := code([]string{"alpha"}); got != http.StatusForbidden {
		t.Errorf("consent-scoped admin token should be denied the platform audit log; got %d", got)
	}
	// No allow-list (PAT / web session admin): unchanged — allowed.
	if got := code(nil); got != http.StatusOK {
		t.Errorf("non-scoped admin should get the audit log; got %d", got)
	}
	// Wildcard consent: allowed.
	if got := code([]string{"*"}); got != http.StatusOK {
		t.Errorf("wildcard-consent admin should get the audit log; got %d", got)
	}
}
