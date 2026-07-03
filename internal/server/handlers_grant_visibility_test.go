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
