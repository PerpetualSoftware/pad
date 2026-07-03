package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// restrictedOwnerVisibilityFixture seeds a workspace-role "owner" member who
// is independently restricted via collection_access="specific" (BUG-1920:
// requireMinRole("owner") only checks workspaceRole, which handleSetMember-
// CollectionAccess can scope down regardless of role — there is no role
// exclusion). One collection/item pair sits inside the member's visible
// slice, one outside it. Provides both a bearer PAT and a session cookie
// for the same user so every auth class is covered, mirroring
// bearerGateItemFixture's shape (handlers_items_test.go, BUG-1918).
type restrictedOwnerVisibilityFixture struct {
	srv          *Server
	ws           *models.Workspace
	ownerID      string
	hiddenColl   *models.Collection
	visibleColl  *models.Collection
	hiddenItem   *models.Item
	visibleItem  *models.Item
	bearerToken  string
	sessionToken string
}

func newRestrictedOwnerVisibilityFixture(t *testing.T) *restrictedOwnerVisibilityFixture {
	t.Helper()
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "restricted-owner@example.com", Name: "Restricted Owner", Username: "restricted-owner",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "GrantVisibility", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: schema,
	})
	if err != nil {
		t.Fatalf("create visible collection: %v", err)
	}
	hidden, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: schema,
	})
	if err != nil {
		t.Fatalf("create hidden collection: %v", err)
	}
	visibleItem, err := srv.store.CreateItem(ws.ID, visible.ID, models.ItemCreate{
		Title: "Visible item", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create visible item: %v", err)
	}
	hiddenItem, err := srv.store.CreateItem(ws.ID, hidden.ID, models.ItemCreate{
		Title: "Hidden item", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create hidden item: %v", err)
	}

	// Scope the owner to "visible" only. member_collection_access has no
	// role exclusion, so a workspace-role "owner" is restricted exactly
	// like any other member.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, owner.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	tok, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{
		Name: "owner-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	return &restrictedOwnerVisibilityFixture{
		srv: srv, ws: ws, ownerID: owner.ID,
		hiddenColl: hidden, visibleColl: visible,
		hiddenItem: hiddenItem, visibleItem: visibleItem,
		bearerToken: tok.Token, sessionToken: sessTok,
	}
}

func (f *restrictedOwnerVisibilityFixture) bearerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.bearerToken}
}

// grantHiddenItemToOwner gives the fixture's restricted owner an item-level
// grant on the hidden item, independent of their collection_access
// restriction — the item-grant-only scenario from BUG-1920's codex R2
// follow-up. VisibleCollectionIDs (workspace_members.go) folds
// item-grant-derived collections into the nav-lenient visible set, but
// requireCollectionFullyVisible must NOT treat an item grant as full access
// to the collection it lives in.
func (f *restrictedOwnerVisibilityFixture) grantHiddenItemToOwner(t *testing.T) {
	t.Helper()
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.hiddenItem.ID, f.ownerID, "view", f.ownerID); err != nil {
		t.Fatalf("CreateItemGrant hidden item to owner: %v", err)
	}
}

// seedGrantee creates a throwaway user to be the recipient of a grant, for
// tests that need a real grant/link ID on a hidden or visible resource
// (BUG-1923's delete/view-history handlers operate on IDs directly, so the
// setup for these tests bypasses the HTTP layer via direct store calls —
// mirroring how the resource would have been minted before the caller lost
// visibility to it).
func (f *restrictedOwnerVisibilityFixture) seedGrantee(t *testing.T) *models.User {
	t.Helper()
	grantee, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "grantee2@example.com", Name: "Grantee2", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create grantee: %v", err)
	}
	return grantee
}

// ─── handleCreateItemShareLink ──────────────────────────────────────────

// TestCreateItemShareLink_RestrictedOwner_HiddenCollection404 pins BUG-1920:
// a restricted owner can no longer mint a public share-link token for an
// item in a collection hidden from them, over either auth class. An
// unrestricted (visible-collection) item still succeeds.
func TestCreateItemShareLink_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/share-links"
	rr := doRequestWithHeaders(f.srv, "POST", path, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner create share-link on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", path, map[string]interface{}{}, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner create share-link on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.visibleItem.Slug + "/share-links"
	rr = doRequestWithHeaders(f.srv, "POST", visPath, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer restricted owner create share-link on visible item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestListItemShareLinks_RestrictedOwner_HiddenCollection404 pins the
// list-side of BUG-1920.
func TestListItemShareLinks_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/share-links"
	rr := doRequestWithHeaders(f.srv, "GET", path, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner list share-links on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner list share-links on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.visibleItem.Slug + "/share-links"
	rr = doRequestWithHeaders(f.srv, "GET", visPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer restricted owner list share-links on visible item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleCreateCollectionShareLink / handleListCollectionShareLinks ──

// TestCollectionShareLinks_RestrictedOwner_HiddenCollection404 pins the
// collection-level twin of BUG-1920: minting or listing a share link for a
// collection hidden from the restricted owner must 404, over either auth
// class.
func TestCollectionShareLinks_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	createPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/share-links"
	rr := doRequestWithHeaders(f.srv, "POST", createPath, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner create collection share-link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", createPath, map[string]interface{}{}, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner create collection share-link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	listPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/share-links"
	rr = doRequestWithHeaders(f.srv, "GET", listPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner list collection share-links on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visCreatePath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.visibleColl.Slug + "/share-links"
	rr = doRequestWithHeaders(f.srv, "POST", visCreatePath, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer restricted owner create collection share-link on visible collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleCreateItemGrant / handleListItemGrants ──────────────────────

// TestCreateItemGrant_RestrictedOwner_HiddenCollection404 pins BUG-1920 for
// item grants.
func TestCreateItemGrant_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	grantee, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "grantee@example.com", Name: "Grantee", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create grantee: %v", err)
	}

	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants"
	body := map[string]interface{}{"user_id": grantee.ID, "permission": "view"}
	rr := doRequestWithHeaders(f.srv, "POST", path, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner create item grant on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", path, body, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner create item grant on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.visibleItem.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "POST", visPath, body, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer restricted owner create item grant on visible item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestListItemGrants_RestrictedOwner_HiddenCollection404 pins the list-side
// of BUG-1920 for item grants.
func TestListItemGrants_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants"
	rr := doRequestWithHeaders(f.srv, "GET", path, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner list item grants on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner list item grants on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.visibleItem.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "GET", visPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer restricted owner list item grants on visible item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleCreateCollectionGrant / handleListCollectionGrants ──────────

// TestCollectionGrants_RestrictedOwner_HiddenCollection404 pins the
// collection-level twin of BUG-1920 for grants.
func TestCollectionGrants_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	grantee, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "grantee@example.com", Name: "Grantee", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create grantee: %v", err)
	}

	createPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants"
	body := map[string]interface{}{"user_id": grantee.ID, "permission": "view"}
	rr := doRequestWithHeaders(f.srv, "POST", createPath, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner create collection grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", createPath, body, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner create collection grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	listPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "GET", listPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner list collection grants on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visCreatePath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.visibleColl.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "POST", visCreatePath, body, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer restricted owner create collection grant on visible collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── Item-grant-only must NOT promote to collection-wide access ───────

// TestCollectionShareLinksAndGrants_ItemGrantOnly_HiddenCollection404 pins
// codex R2's follow-up finding: VisibleCollectionIDs folds in collections
// that are visible ONLY via an item-level grant ("so the collection appears
// in navigation" — workspace_members.go), which made the original
// requireCollectionVisible pass for a restricted owner holding nothing more
// than an item grant on ONE item inside the hidden collection — letting
// them mint/list a share link or grant for the ENTIRE hidden collection.
// requireCollectionFullyVisible must narrow to full-collection-access
// semantics and 404 here, over both auth classes, while the SAME owner's
// item-level operations on the granted item itself keep working (an item
// grant legitimately entitles the holder to act on that item).
func TestCollectionShareLinksAndGrants_ItemGrantOnly_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	f.grantHiddenItemToOwner(t)

	grantee, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "grantee@example.com", Name: "Grantee", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create grantee: %v", err)
	}
	grantBody := map[string]interface{}{"user_id": grantee.ID, "permission": "view"}

	// Collection-level share link create + list on the hidden collection:
	// item grant on hiddenItem must NOT qualify as full collection access.
	collSharePath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/share-links"
	rr := doRequestWithHeaders(f.srv, "POST", collSharePath, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner create collection share-link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", collSharePath, map[string]interface{}{}, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner create collection share-link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, "GET", collSharePath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner list collection share-links on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", collSharePath, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner list collection share-links on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// Collection-level grant create + list on the hidden collection: same.
	collGrantPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "POST", collGrantPath, grantBody, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner create collection grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "POST", collGrantPath, grantBody, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner create collection grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, "GET", collGrantPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner list collection grants on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", collGrantPath, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner list collection grants on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// Item-level operations on the granted item itself must still succeed
	// (both auth classes) — an item grant legitimately entitles the holder
	// to mint/list a share link or grant for that specific item.
	itemSharePath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/share-links"
	rr = doRequestWithHeaders(f.srv, "POST", itemSharePath, map[string]interface{}{}, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer item-grant-only owner create item share-link on granted item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", itemSharePath, nil, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("session item-grant-only owner list item share-links on granted item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	itemGrantPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants"
	rr = doRequestWithHeaders(f.srv, "POST", itemGrantPath, grantBody, f.bearerHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("bearer item-grant-only owner create item grant on granted item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", itemGrantPath, nil, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("session item-grant-only owner list item grants on granted item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleDeleteCollectionGrant / handleDeleteItemGrant (BUG-1923) ────

// TestDeleteCollectionGrant_RestrictedOwner_HiddenCollection404 pins
// BUG-1923: unlike its create/list siblings, handleDeleteCollectionGrant
// operated on the grant ID directly with no visibility check on the
// underlying collection — a restricted owner who knew a grant ID could
// revoke a grant on a collection hidden from them. The grant is seeded
// directly via the store (as if minted before the owner's access was
// restricted), so the delete attempt exercises only the gate under test;
// since the gate 404s before deleting, the same grant ID can be re-used for
// both auth classes.
func TestDeleteCollectionGrant_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	grantee := f.seedGrantee(t)

	hiddenGrant, err := f.srv.store.CreateCollectionGrant(f.ws.ID, f.hiddenColl.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed hidden collection grant: %v", err)
	}
	visibleGrant, err := f.srv.store.CreateCollectionGrant(f.ws.ID, f.visibleColl.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed visible collection grant: %v", err)
	}

	hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants/" + hiddenGrant.ID
	rr := doRequestWithHeaders(f.srv, "DELETE", hiddenPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner delete grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "DELETE", hiddenPath, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner delete grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if g, err := f.srv.store.GetCollectionGrant(hiddenGrant.ID); err != nil || g == nil {
		t.Fatalf("hidden grant should NOT have been deleted by the 404'd attempts: %v, %v", g, err)
	}

	visiblePath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.visibleColl.Slug + "/grants/" + visibleGrant.ID
	rr = doRequestWithHeaders(f.srv, "DELETE", visiblePath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer restricted owner delete grant on visible collection: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestDeleteItemGrant_RestrictedOwner_HiddenCollection404 pins the item-grant
// twin of BUG-1923's delete-side fix.
func TestDeleteItemGrant_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	grantee := f.seedGrantee(t)

	hiddenGrant, err := f.srv.store.CreateItemGrant(f.ws.ID, f.hiddenItem.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed hidden item grant: %v", err)
	}
	visibleGrant, err := f.srv.store.CreateItemGrant(f.ws.ID, f.visibleItem.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed visible item grant: %v", err)
	}

	hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants/" + hiddenGrant.ID
	rr := doRequestWithHeaders(f.srv, "DELETE", hiddenPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner delete grant on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "DELETE", hiddenPath, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner delete grant on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if g, err := f.srv.store.GetItemGrant(hiddenGrant.ID); err != nil || g == nil {
		t.Fatalf("hidden grant should NOT have been deleted by the 404'd attempts: %v, %v", g, err)
	}

	visiblePath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.visibleItem.Slug + "/grants/" + visibleGrant.ID
	rr = doRequestWithHeaders(f.srv, "DELETE", visiblePath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer restricted owner delete grant on visible item: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleDeleteShareLink / handleShareLinkViews (BUG-1923) ───────────

// TestDeleteShareLink_RestrictedOwner_HiddenTarget404 pins BUG-1923 for both
// share-link target classes: handleDeleteShareLink operated on the link ID
// directly with no check on the underlying item/collection.
func TestDeleteShareLink_RestrictedOwner_HiddenTarget404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	hiddenItemLink, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.hiddenItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed hidden item share link: %v", err)
	}
	hiddenCollLink, err := f.srv.store.CreateShareLink(f.ws.ID, "collection", f.hiddenColl.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed hidden collection share link: %v", err)
	}
	visibleItemLink, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.visibleItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed visible item share link: %v", err)
	}

	deletePath := "/api/v1/workspaces/" + f.ws.Slug + "/share-links/"

	rr := doRequestWithHeaders(f.srv, "DELETE", deletePath+hiddenItemLink.ID, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner delete share link on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "DELETE", deletePath+hiddenItemLink.ID, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner delete share link on hidden item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "DELETE", deletePath+hiddenCollLink.ID, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner delete share link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "DELETE", deletePath+hiddenCollLink.ID, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner delete share link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	if link, err := f.srv.store.GetShareLink(hiddenItemLink.ID); err != nil || link == nil {
		t.Fatalf("hidden item share link should NOT have been deleted by the 404'd attempts: %v, %v", link, err)
	}

	rr = doRequestWithHeaders(f.srv, "DELETE", deletePath+visibleItemLink.ID, nil, f.bearerHeaders())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer restricted owner delete share link on visible item: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestShareLinkViews_RestrictedOwner_HiddenTarget404 pins BUG-1923 for the
// view-history GET: handleShareLinkViews already resolved and
// workspace-scoped the link but never checked visibility on its target.
func TestShareLinkViews_RestrictedOwner_HiddenTarget404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	hiddenItemLink, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.hiddenItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed hidden item share link: %v", err)
	}
	hiddenCollLink, err := f.srv.store.CreateShareLink(f.ws.ID, "collection", f.hiddenColl.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed hidden collection share link: %v", err)
	}
	visibleItemLink, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.visibleItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed visible item share link: %v", err)
	}

	viewsPath := "/api/v1/workspaces/" + f.ws.Slug + "/share-links/"

	rr := doRequestWithHeaders(f.srv, "GET", viewsPath+hiddenItemLink.ID+"/views", nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner view-history on hidden item link: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", viewsPath+hiddenItemLink.ID+"/views", nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner view-history on hidden item link: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "GET", viewsPath+hiddenCollLink.ID+"/views", nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner view-history on hidden collection link: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "GET", viewsPath+visibleItemLink.ID+"/views", nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer restricted owner view-history on visible item link: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── Item-grant-only must NOT promote to collection-wide delete/views ─

// TestCollectionGrantAndShareLinkDelete_ItemGrantOnly_HiddenCollection404
// extends the item-grant-only non-promotion rule (see
// TestCollectionShareLinksAndGrants_ItemGrantOnly_HiddenCollection404 above)
// to the BUG-1923 delete/view-history handlers: an item-level grant on ONE
// item inside the hidden collection must not let the owner delete a
// collection-scoped grant/share-link for that collection, while delete
// operations scoped to the granted item itself keep working.
func TestCollectionGrantAndShareLinkDelete_ItemGrantOnly_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	f.grantHiddenItemToOwner(t)
	grantee := f.seedGrantee(t)

	collGrant, err := f.srv.store.CreateCollectionGrant(f.ws.ID, f.hiddenColl.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed hidden collection grant: %v", err)
	}
	collLink, err := f.srv.store.CreateShareLink(f.ws.ID, "collection", f.hiddenColl.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed hidden collection share link: %v", err)
	}

	collGrantPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants/" + collGrant.ID
	rr := doRequestWithHeaders(f.srv, "DELETE", collGrantPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner delete collection grant on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	linkDeletePath := "/api/v1/workspaces/" + f.ws.Slug + "/share-links/" + collLink.ID
	rr = doRequestWithHeaders(f.srv, "DELETE", linkDeletePath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner delete collection share-link on hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// Item-scoped grant/share-link on the granted item itself must still
	// be deletable by the item-grant-only owner.
	itemGrant, err := f.srv.store.CreateItemGrant(f.ws.ID, f.hiddenItem.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed item grant on granted item: %v", err)
	}
	itemLink, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.hiddenItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed item share link on granted item: %v", err)
	}

	itemGrantPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants/" + itemGrant.ID
	rr = doRequestWithHeaders(f.srv, "DELETE", itemGrantPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer item-grant-only owner delete item grant on granted item: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	itemLinkPath := "/api/v1/workspaces/" + f.ws.Slug + "/share-links/" + itemLink.ID
	rr = doRequestWithHeaders(f.srv, "DELETE", itemLinkPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer item-grant-only owner delete item share-link on granted item: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── Layering: requireMinRole (403) must run BEFORE visibility (404),
//     ID-keyed delete/views handlers (BUG-1923) ──────────────────────────

// TestShareLinkAndGrantDeleteViews_NonOwner_403BeforeVisibility extends the
// 403-before-404 layering guarantee to the four ID-keyed handlers fixed by
// BUG-1923: a non-owner member gets 403 from requireMinRole regardless of
// the target's visibility or existence.
func TestShareLinkAndGrantDeleteViews_NonOwner_403BeforeVisibility(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	grantee := f.seedGrantee(t)

	editor, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "editor2@example.com", Name: "Editor2", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create editor: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("add editor member: %v", err)
	}
	editorTok, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{
		Name: "editor2-pat", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken editor: %v", err)
	}
	editorHeaders := map[string]string{"Authorization": "Bearer " + editorTok.Token}

	collGrant, err := f.srv.store.CreateCollectionGrant(f.ws.ID, f.hiddenColl.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed collection grant: %v", err)
	}
	itemGrant, err := f.srv.store.CreateItemGrant(f.ws.ID, f.hiddenItem.ID, grantee.ID, "view", f.ownerID)
	if err != nil {
		t.Fatalf("seed item grant: %v", err)
	}
	link, err := f.srv.store.CreateShareLink(f.ws.ID, "item", f.hiddenItem.ID, "view", f.ownerID, nil)
	if err != nil {
		t.Fatalf("seed share link: %v", err)
	}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"delete collection grant", "DELETE", "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants/" + collGrant.ID},
		{"delete item grant", "DELETE", "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants/" + itemGrant.ID},
		{"delete share link", "DELETE", "/api/v1/workspaces/" + f.ws.Slug + "/share-links/" + link.ID},
		{"share link views", "GET", "/api/v1/workspaces/" + f.ws.Slug + "/share-links/" + link.ID + "/views"},
	}
	for _, tc := range cases {
		rr := doRequestWithHeaders(f.srv, tc.method, tc.path, nil, editorHeaders)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: editor (non-owner) expected 403, got %d: %s", tc.name, rr.Code, rr.Body.String())
		}
	}
}

// ─── Layering: requireMinRole (403) must run BEFORE visibility (404) ───

// TestItemShareLinksAndGrants_NonOwner_403BeforeVisibility pins the
// dispatcher-required layering: a non-owner member gets 403 from
// requireMinRole regardless of item visibility — the visibility check
// (404) never even runs for them. Exercised against the hidden item so a
// regression that swapped the check order (visibility before role) would
// surface as an incorrect 404 here instead of the expected 403.
func TestItemShareLinksAndGrants_NonOwner_403BeforeVisibility(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	editor, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "editor@example.com", Name: "Editor", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create editor: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("add editor member: %v", err)
	}
	editorTok, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{
		Name: "editor-pat", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken editor: %v", err)
	}
	editorHeaders := map[string]string{"Authorization": "Bearer " + editorTok.Token}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"create item share link", "POST", "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/share-links"},
		{"list item share links", "GET", "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/share-links"},
		{"create item grant", "POST", "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants"},
		{"list item grants", "GET", "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.hiddenItem.Slug + "/grants"},
		{"create collection share link", "POST", "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/share-links"},
		{"list collection share links", "GET", "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/share-links"},
		{"create collection grant", "POST", "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants"},
		{"list collection grants", "GET", "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug + "/grants"},
	}
	for _, tc := range cases {
		rr := doRequestWithHeaders(f.srv, tc.method, tc.path, map[string]interface{}{}, editorHeaders)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: editor (non-owner) expected 403, got %d: %s", tc.name, rr.Code, rr.Body.String())
		}
	}
}
