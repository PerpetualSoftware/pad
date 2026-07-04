package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// This file pins BUG-1925: handleSetMemberCollectionAccess gated only on
// requireRole(r, "owner"), with zero validation of the CALLER's own
// visibility. A workspace-role "owner" who is independently restricted via
// collection_access="specific" (member_collection_access has no role
// exclusion — BUG-1920) could self-escalate to mode="all", or grant anyone
// (including themselves) a collection outside their own visible set,
// defeating the whole BUG-1917/1918/1920/1921 family plus the BUG-1922
// export gate. requireCallerCanSetCollectionAccess closes this.

func collectionAccessPath(f *restrictedOwnerVisibilityFixture, targetUserID string) string {
	return "/api/v1/workspaces/" + f.ws.Slug + "/members/" + targetUserID + "/collection-access"
}

// TestSetMemberCollectionAccess_RestrictedOwner_SelfToAll_Forbidden pins the
// self-escalation arm: a restricted owner cannot PATCH their own record to
// mode="all", over either auth class.
func TestSetMemberCollectionAccess_RestrictedOwner_SelfToAll_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	path := collectionAccessPath(f, f.ownerID)
	body := map[string]interface{}{"mode": "all"}

	rr := doRequestWithHeaders(f.srv, "PUT", path, body, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bearer restricted owner self mode=all: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "PUT", path, body, f.sessionToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("session restricted owner self mode=all: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	assertMemberCollectionAccessUnchanged(t, f, f.ownerID, "specific", []string{f.visibleColl.ID})
}

// TestSetMemberCollectionAccess_RestrictedOwner_HiddenID_Forbidden pins the
// per-ID membership check: a restricted owner cannot include a collection
// outside their own visible set, even alongside a visible one, and even
// when the target is themselves.
func TestSetMemberCollectionAccess_RestrictedOwner_HiddenID_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	path := collectionAccessPath(f, f.ownerID)
	body := map[string]interface{}{
		"mode":           "specific",
		"collection_ids": []string{f.visibleColl.ID, f.hiddenColl.ID},
	}

	rr := doRequestWithHeaders(f.srv, "PUT", path, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner hidden ID: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "PUT", path, body, f.sessionToken)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("session restricted owner hidden ID: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	assertMemberCollectionAccessUnchanged(t, f, f.ownerID, "specific", []string{f.visibleColl.ID})
}

// TestSetMemberCollectionAccess_RestrictedOwner_CrossMemberHiddenID_Forbidden
// pins the cross-member leak arm: a restricted owner cannot grant a
// DIFFERENT member a collection the granting owner cannot themselves see.
func TestSetMemberCollectionAccess_RestrictedOwner_CrossMemberHiddenID_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	grantee := f.seedGrantee(t)
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, grantee.ID, "editor"); err != nil {
		t.Fatalf("add grantee as member: %v", err)
	}

	path := collectionAccessPath(f, grantee.ID)
	body := map[string]interface{}{
		"mode":           "specific",
		"collection_ids": []string{f.hiddenColl.ID},
	}
	rr := doRequestWithHeaders(f.srv, "PUT", path, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bearer restricted owner cross-member hidden ID: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}

	grants, err := f.srv.store.GetMemberCollectionAccess(f.ws.ID, grantee.ID)
	if err != nil {
		t.Fatalf("GetMemberCollectionAccess: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected no grants written to grantee, got %v", grants)
	}
}

// TestSetMemberCollectionAccess_RestrictedOwner_WithinScope_OK confirms a
// restricted owner can still manage collection access within their own
// visible set, including an empty collection_ids list (which grants
// nothing and cannot escalate).
func TestSetMemberCollectionAccess_RestrictedOwner_WithinScope_OK(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	rr := doRequestWithHeaders(f.srv, "PUT", collectionAccessPath(f, f.ownerID),
		map[string]interface{}{"mode": "specific", "collection_ids": []string{f.visibleColl.ID}}, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted owner within-scope specific: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "PUT", collectionAccessPath(f, f.ownerID),
		map[string]interface{}{"mode": "specific", "collection_ids": []string{}}, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted owner empty collection_ids: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertMemberCollectionAccessUnchanged(t, f, f.ownerID, "specific", []string{})
}

// TestSetMemberCollectionAccess_RestrictedOwner_ItemGrantOnly_StillForbidden
// pins the fullCollIDs narrowing (mirrors requireCollectionFullyVisible,
// BUG-1920 codex R2): a restricted owner who holds only an ITEM-level grant
// into the hidden collection must not be able to designate that collection
// as a mode="specific" target. visibleCollectionIDs is nav-lenient and
// folds item-grant-derived collections in for navigation purposes; using it
// raw here would let a single-item grant escalate into full collection-wide
// member_collection_access.
func TestSetMemberCollectionAccess_RestrictedOwner_ItemGrantOnly_StillForbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	f.grantHiddenItemToOwner(t)

	path := collectionAccessPath(f, f.ownerID)
	body := map[string]interface{}{
		"mode":           "specific",
		"collection_ids": []string{f.hiddenColl.ID},
	}
	rr := doRequestWithHeaders(f.srv, "PUT", path, body, f.bearerHeaders())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("restricted owner item-grant-only escalation: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestSetMemberCollectionAccess_UnrestrictedOwner_OK confirms the guard is a
// no-op for an unrestricted (mode "all") owner — the common case.
func TestSetMemberCollectionAccess_UnrestrictedOwner_OK(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "unrestricted-owner@example.com", Name: "Owner", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Unrestricted", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	path := "/api/v1/workspaces/" + ws.Slug + "/members/" + owner.ID + "/collection-access"
	rr := doRequestWithCookie(srv, "PUT", path, map[string]interface{}{"mode": "all"}, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted owner mode=all: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithCookie(srv, "PUT", path,
		map[string]interface{}{"mode": "specific", "collection_ids": []string{coll.ID}}, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted owner mode=specific: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func assertMemberCollectionAccessUnchanged(t *testing.T, f *restrictedOwnerVisibilityFixture, userID, wantMode string, wantIDs []string) {
	t.Helper()
	member, err := f.srv.store.GetWorkspaceMember(f.ws.ID, userID)
	if err != nil {
		t.Fatalf("GetWorkspaceMember: %v", err)
	}
	if member.CollectionAccess != wantMode {
		t.Fatalf("expected collection_access to remain %q, got %q (partial write on a rejected request)", wantMode, member.CollectionAccess)
	}
	grants, err := f.srv.store.GetMemberCollectionAccess(f.ws.ID, userID)
	if err != nil {
		t.Fatalf("GetMemberCollectionAccess: %v", err)
	}
	if len(grants) != len(wantIDs) {
		t.Fatalf("expected grants %v to remain unchanged, got %v", wantIDs, grants)
	}
	want := make(map[string]bool, len(wantIDs))
	for _, id := range wantIDs {
		want[id] = true
	}
	for _, id := range grants {
		if !want[id] {
			t.Fatalf("unexpected grant %q present after rejected request: %v", id, grants)
		}
	}
}
