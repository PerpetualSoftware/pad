package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestMe_OwnerAdmin pins the admin/owner shape: collection_access "all",
// no visibility list, no grants. Admins are normalized by the auth
// middleware to role="owner" regardless of formal membership.
func TestMe_OwnerAdmin(t *testing.T) {
	env := setupRBACEnv(t)

	rr := doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, env.ownerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp meResponse
	parseJSON(t, rr, &resp)

	if resp.Role != "owner" {
		t.Errorf("role: expected owner, got %q", resp.Role)
	}
	if resp.CollectionAccess != "all" {
		t.Errorf("collection_access: expected all, got %q", resp.CollectionAccess)
	}
	if len(resp.VisibleCollectionIDs) != 0 {
		t.Errorf("visible_collection_ids: expected empty for all-access, got %v", resp.VisibleCollectionIDs)
	}
	if resp.CollectionGrants == nil {
		t.Error("collection_grants: expected empty slice, got nil (must be present in JSON)")
	}
	if resp.ItemGrants == nil {
		t.Error("item_grants: expected empty slice, got nil (must be present in JSON)")
	}
}

// TestMe_EditorMember pins the editor-with-all-access shape.
func TestMe_EditorMember(t *testing.T) {
	env := setupRBACEnv(t)

	rr := doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, env.editorToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp meResponse
	parseJSON(t, rr, &resp)

	if resp.Role != "editor" {
		t.Errorf("role: expected editor, got %q", resp.Role)
	}
	if resp.CollectionAccess != "all" {
		t.Errorf("collection_access: expected all (default), got %q", resp.CollectionAccess)
	}
	if len(resp.VisibleCollectionIDs) != 0 {
		t.Errorf("visible_collection_ids: expected empty for all-access, got %v", resp.VisibleCollectionIDs)
	}
}

// TestMe_ViewerWithCollectionGrant pins the precedence-override case:
// a viewer with CollectionGrant.edit on one collection. The /me response
// must include the grant so the frontend canEditCollection helper can
// override the role-based deny.
func TestMe_ViewerWithCollectionGrant(t *testing.T) {
	env := setupRBACEnv(t)

	// Find an existing collection in the workspace to grant against.
	ws, err := env.srv.store.GetWorkspaceBySlug(env.wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("locate workspace: %v", err)
	}
	colls, err := env.srv.store.ListCollections(ws.ID)
	if err != nil || len(colls) == 0 {
		t.Fatalf("list collections: %v (got %d)", err, len(colls))
	}
	target := colls[0]

	viewer, err := env.srv.store.GetUserByEmail("viewer@test.com")
	if err != nil || viewer == nil {
		t.Fatal("locate viewer user")
	}
	owner, err := env.srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatal("locate owner user")
	}

	if _, err := env.srv.store.CreateCollectionGrant(ws.ID, target.ID, viewer.ID, "edit", owner.ID); err != nil {
		t.Fatalf("create collection grant: %v", err)
	}

	rr := doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, env.viewerToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp meResponse
	parseJSON(t, rr, &resp)

	if resp.Role != "viewer" {
		t.Errorf("role: expected viewer, got %q", resp.Role)
	}
	if len(resp.CollectionGrants) != 1 {
		t.Fatalf("expected 1 collection grant, got %d", len(resp.CollectionGrants))
	}
	if resp.CollectionGrants[0].CollectionID != target.ID {
		t.Errorf("grant collection_id: expected %s, got %s", target.ID, resp.CollectionGrants[0].CollectionID)
	}
	if resp.CollectionGrants[0].Permission != "edit" {
		t.Errorf("grant permission: expected edit, got %s", resp.CollectionGrants[0].Permission)
	}
}

// TestMe_RestrictedMember pins the collection_access="specific" path:
// VisibleCollectionIDs is materialized into the response so the frontend
// can answer canViewCollection without re-deriving the cascade.
func TestMe_RestrictedMember(t *testing.T) {
	env := setupRBACEnv(t)

	ws, err := env.srv.store.GetWorkspaceBySlug(env.wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("locate workspace: %v", err)
	}
	colls, err := env.srv.store.ListCollections(ws.ID)
	if err != nil || len(colls) < 2 {
		t.Fatalf("list collections: need >= 2, got %d", len(colls))
	}

	editor, err := env.srv.store.GetUserByEmail("editor@test.com")
	if err != nil || editor == nil {
		t.Fatal("locate editor user")
	}

	// Restrict the editor to the first collection only.
	if err := env.srv.store.SetMemberCollectionAccess(ws.ID, editor.ID, "specific", []string{colls[0].ID}); err != nil {
		t.Fatalf("set member collection access: %v", err)
	}

	rr := doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, env.editorToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp meResponse
	parseJSON(t, rr, &resp)

	if resp.CollectionAccess != "specific" {
		t.Errorf("collection_access: expected specific, got %q", resp.CollectionAccess)
	}
	// Must include at least the granted collection. The list also includes
	// system collections (always visible) and any item-grant collections —
	// see workspace_members.go:139. The exact size depends on system seeds;
	// we only assert the granted ID is present.
	found := false
	for _, id := range resp.VisibleCollectionIDs {
		if id == colls[0].ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("visible_collection_ids missing granted collection %s, got %v", colls[0].ID, resp.VisibleCollectionIDs)
	}
}

// TestMe_GuestWithItemGrant pins the guest path: a non-member with only
// an item_grant. /me uses GuestVisibleCollectionIDs which materializes
// the item-grant's collection into visible_collection_ids so the
// collection appears in nav.
func TestMe_GuestWithItemGrant(t *testing.T) {
	env := setupRBACEnv(t)

	ws, err := env.srv.store.GetWorkspaceBySlug(env.wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("locate workspace: %v", err)
	}

	// Create an item to grant.
	rr := doRequestWithCookie(env.srv, "POST",
		"/api/v1/workspaces/"+env.wsSlug+"/collections/docs/items",
		map[string]interface{}{"title": "Guest Target", "content": "x"},
		env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// Register a non-member user.
	rr = doRequestWithCookie(env.srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "guest@test.com",
		"name":     "Guest",
		"password": "correct-horse-battery-staple",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register guest: %d %s", rr.Code, rr.Body.String())
	}
	guestUser, err := env.srv.store.GetUserByEmail("guest@test.com")
	if err != nil || guestUser == nil {
		t.Fatal("locate guest user")
	}
	owner, err := env.srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatal("locate owner user")
	}

	// Grant the guest item-edit access.
	if _, err := env.srv.store.CreateItemGrant(ws.ID, item.ID, guestUser.ID, "edit", owner.ID); err != nil {
		t.Fatalf("create item grant: %v", err)
	}

	guestToken := loginUser(t, env.srv, "guest@test.com", "correct-horse-battery-staple")

	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, guestToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp meResponse
	parseJSON(t, rr, &resp)

	if resp.Role != "guest" {
		t.Errorf("role: expected guest, got %q", resp.Role)
	}
	if resp.CollectionAccess != "specific" {
		t.Errorf("collection_access: expected specific (guest), got %q", resp.CollectionAccess)
	}
	if len(resp.ItemGrants) != 1 {
		t.Fatalf("expected 1 item grant, got %d", len(resp.ItemGrants))
	}
	if resp.ItemGrants[0].ItemID != item.ID {
		t.Errorf("item grant item_id: expected %s, got %s", item.ID, resp.ItemGrants[0].ItemID)
	}
	// The granted item's collection must appear in visible_collection_ids
	// so it shows up in workspace navigation.
	if len(resp.VisibleCollectionIDs) == 0 {
		t.Errorf("guest with item grant: expected at least one visible collection, got 0")
	}
}

// TestMe_NonMemberNoGrants_NoAccess confirms non-members with no grants
// are rejected upstream and never reach /me. The workspace resolver returns
// 404 (workspace not found from this user's perspective) before
// RequireWorkspaceAccess gets a chance to 403 — both signal "no access" and
// the handler itself never runs. Either status is acceptable; what matters
// is that /me does not leak data to a stranger.
func TestMe_NonMemberNoGrants_NoAccess(t *testing.T) {
	env := setupRBACEnv(t)

	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "stranger@test.com",
		"name":     "Stranger",
		"password": "correct-horse-battery-staple",
	}, env.ownerToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register stranger: %d %s", rr.Code, rr.Body.String())
	}
	strangerToken := loginUser(t, env.srv, "stranger@test.com", "correct-horse-battery-staple")

	rr = doRequestWithCookie(env.srv, "GET", "/api/v1/workspaces/"+env.wsSlug+"/me", nil, strangerToken)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Errorf("non-member with no grants: expected 403 or 404, got %d: %s", rr.Code, rr.Body.String())
	}
}
