package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-1921: handleUpdateCollection and handleDeleteCollection resolve a
// collection by slug and gated only on requireMinRole("owner") — no
// requireCollectionFullyVisible check. A workspace-role owner restricted via
// collection_access="specific" (member_collection_access has no role
// exclusion, per BUG-1920) could rename, re-schema, or delete a collection
// hidden from them. These tests reuse restrictedOwnerVisibilityFixture
// (handlers_grant_visibility_test.go) — the fixture and its hidden/visible
// collection pair are identical to BUG-1920's; only the endpoints under
// test differ.

// ─── handleUpdateCollection ─────────────────────────────────────────────

// TestUpdateCollection_RestrictedOwner_HiddenCollection404 pins BUG-1921 for
// PATCH: a restricted owner can no longer rename/re-schema a collection
// hidden from them, over either auth class. The same owner can still PATCH
// a visible collection.
func TestUpdateCollection_RestrictedOwner_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug
	body := map[string]interface{}{"name": "Renamed Hidden"}
	rr := doRequestWithHeaders(f.srv, "PATCH", hiddenPath, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner PATCH hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "PATCH", hiddenPath, body, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner PATCH hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	visPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.visibleColl.Slug
	rr = doRequestWithHeaders(f.srv, "PATCH", visPath, map[string]interface{}{"name": "Renamed Visible"}, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer restricted owner PATCH visible collection: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── handleDeleteCollection ─────────────────────────────────────────────

// TestDeleteCollection_RestrictedOwner_HiddenCollection404 pins BUG-1921 for
// DELETE: a restricted owner can no longer delete a collection hidden from
// them, over either auth class. A fresh fixture is used per sub-case so a
// successful delete of the visible collection in one case doesn't affect
// another.
func TestDeleteCollection_RestrictedOwner_HiddenCollection404(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		f := newRestrictedOwnerVisibilityFixture(t)
		hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug
		rr := doRequestWithHeaders(f.srv, "DELETE", hiddenPath, nil, f.bearerHeaders())
		if rr.Code != http.StatusNotFound {
			t.Fatalf("bearer restricted owner DELETE hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("session", func(t *testing.T) {
		f := newRestrictedOwnerVisibilityFixture(t)
		hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug
		rr := doRequestWithCookie(f.srv, "DELETE", hiddenPath, nil, f.sessionToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("session restricted owner DELETE hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("visible collection still deletable", func(t *testing.T) {
		f := newRestrictedOwnerVisibilityFixture(t)
		visPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.visibleColl.Slug
		rr := doRequestWithHeaders(f.srv, "DELETE", visPath, nil, f.bearerHeaders())
		if rr.Code != http.StatusNoContent {
			t.Fatalf("bearer restricted owner DELETE visible collection: expected 204, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// ─── Item-grant-only must NOT promote to collection-wide mutation rights ──

// TestUpdateDeleteCollection_ItemGrantOnly_HiddenCollection404 mirrors
// BUG-1920's codex R2 finding: VisibleCollectionIDs folds in collections
// visible ONLY via an item-level grant ("so the collection appears in
// navigation"), which would let a restricted owner holding nothing more
// than an item grant on ONE item inside the hidden collection rename or
// delete the ENTIRE hidden collection if the nav-lenient check were used
// here instead of requireCollectionFullyVisible. This is the load-bearing
// test of the set: it fails if handleUpdateCollection/handleDeleteCollection
// were wired to the nav-lenient visibleCollectionIDs+isCollectionVisible
// check (handleGetCollection's idiom) rather than the strict
// requireCollectionFullyVisible helper.
func TestUpdateDeleteCollection_ItemGrantOnly_HiddenCollection404(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	f.grantHiddenItemToOwner(t)

	hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug

	rr := doRequestWithHeaders(f.srv, "PATCH", hiddenPath, map[string]interface{}{"name": "Renamed"}, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner PATCH hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "PATCH", hiddenPath, map[string]interface{}{"name": "Renamed"}, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner PATCH hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "DELETE", hiddenPath, nil, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer item-grant-only owner DELETE hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "DELETE", hiddenPath, nil, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session item-grant-only owner DELETE hidden collection: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ─── Layering: requireMinRole (403) must run BEFORE visibility (404) ───

// TestUpdateDeleteCollection_NonOwner_403BeforeVisibility pins the required
// layering: a non-owner member gets 403 from requireMinRole regardless of
// collection visibility — the visibility check (404) never even runs for
// them. Exercised against the hidden collection so a regression that
// swapped the check order (visibility before role) would surface as an
// incorrect 404 here instead of the expected 403.
func TestUpdateDeleteCollection_NonOwner_403BeforeVisibility(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	editor, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "editor-crud@example.com", Name: "Editor", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create editor: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("add editor member: %v", err)
	}
	editorTok, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{
		Name: "editor-pat-crud", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken editor: %v", err)
	}
	editorHeaders := map[string]string{"Authorization": "Bearer " + editorTok.Token}

	hiddenPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.hiddenColl.Slug

	rr := doRequestWithHeaders(f.srv, "PATCH", hiddenPath, map[string]interface{}{"name": "Renamed"}, editorHeaders)
	if rr.Code != http.StatusForbidden {
		t.Errorf("editor PATCH hidden collection: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, "DELETE", hiddenPath, nil, editorHeaders)
	if rr.Code != http.StatusForbidden {
		t.Errorf("editor DELETE hidden collection: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
