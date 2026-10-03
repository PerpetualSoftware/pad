package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestTask3376_RestrictedEditorViewGrantCannotWrite pins PR #1756 codex
// round 2's P1. requireEditPermission let any editor through before it
// looked at the collection, so a member with collection_access='specific'
// who could SEE an item only through a VIEW grant could PATCH it and create
// beside it. The editor role now reaches a collection only when the member's
// access includes it ('all', or listed); anywhere else the grant decides,
// and a view grant is not an edit grant. Run for an ordinary collection and
// a system one (the case TASK-3376 newly opened). The edit-grant leg is the
// control: same member, same item, same requests, only the grant level
// differs.
func TestTask3376_RestrictedEditorViewGrantCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		system bool
	}{{"ordinary", false}, {"system", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefResolverFixture(t)
			st := f.srv.store

			listed, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Listed", Slug: "listed", Prefix: "LST"})
			if err != nil {
				t.Fatalf("CreateCollection listed: %v", err)
			}
			hidden, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{
				Name: "Hidden", Slug: "hidden", Prefix: "HID", IsSystem: tc.system,
			})
			if err != nil {
				t.Fatalf("CreateCollection hidden: %v", err)
			}
			target, err := st.CreateItem(f.ws.ID, hidden.ID, models.ItemCreate{Title: "Granted item"})
			if err != nil {
				t.Fatalf("CreateItem: %v", err)
			}

			editor, err := st.CreateUser(models.UserCreate{
				Email: "ed-" + tc.name + "@example.com", Name: "Ed", Username: "ed" + tc.name, Password: "pw-test-12345",
			})
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if err := st.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
				t.Fatalf("AddWorkspaceMember: %v", err)
			}
			if err := st.SetMemberCollectionAccess(f.ws.ID, editor.ID, "specific", []string{listed.ID}); err != nil {
				t.Fatalf("SetMemberCollectionAccess: %v", err)
			}
			grant, err := st.CreateItemGrant(f.ws.ID, target.ID, editor.ID, "view", f.owner.ID)
			if err != nil {
				t.Fatalf("CreateItemGrant: %v", err)
			}
			tok, err := st.CreateAPIToken(editor.ID, models.APITokenCreate{Name: "ed-tok", WorkspaceID: f.ws.ID}, 0, 0)
			if err != nil {
				t.Fatalf("CreateAPIToken: %v", err)
			}
			do := func(method, path string, body interface{}) int {
				t.Helper()
				return f.doAuth(tok.Token, method, path, body).Code
			}
			itemPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + target.Ref
			createPath := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + hidden.Slug + "/items"
			commentPath := itemPath + "/comments"

			// Precondition: the view grant really makes the item readable.
			if code := do("GET", itemPath, nil); code != http.StatusOK {
				t.Fatalf("precondition: view-granted item GET = %d, want 200", code)
			}

			// View grant: no write of any kind.
			if code := do("PATCH", itemPath, map[string]interface{}{"title": "Escalated"}); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Errorf("view grant: PATCH = %d, want 403/404", code)
			}
			if code := do("POST", createPath, map[string]interface{}{"title": "Planted"}); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Errorf("view grant: create in the collection = %d, want 403/404", code)
			}
			if code := do("POST", commentPath, map[string]interface{}{"body": "hi"}); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Errorf("view grant: comment = %d, want 403/404", code)
			}
			// Bulk: the per-item gate refuses it as forbidden, not applied.
			rr := f.doAuth(tok.Token, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/bulk",
				map[string]interface{}{"ids": []string{target.ID}, "op": "archive"})
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"code":"forbidden"`) {
				t.Errorf("view grant: bulk archive = %d %s, want 200 with a forbidden failure", rr.Code, rr.Body.String())
			}
			// Collab: admitted to read through the grant, but not to write.
			collabReq := httptest.NewRequest("GET", "/api/v1/collab/"+target.ID, nil)
			collabReq = collabReq.WithContext(context.WithValue(collabReq.Context(), ctxCurrentUser, editor))
			if access, err := f.srv.authorizeCollabAccess(collabReq, target); err != nil || access.canWrite {
				t.Errorf("view grant: collab access = %+v, %v; want admitted read-only", access, err)
			}
			if got, _ := st.GetItem(target.ID); got == nil || got.Title != "Granted item" || got.DeletedAt != nil {
				t.Errorf("view grant: item changed: %+v", got)
			}

			// Control: an EDIT grant on the same item permits the edit.
			if err := st.DeleteItemGrant(grant.ID, f.ws.ID); err != nil {
				t.Fatalf("DeleteItemGrant: %v", err)
			}
			if _, err := st.CreateItemGrant(f.ws.ID, target.ID, editor.ID, "edit", f.owner.ID); err != nil {
				t.Fatalf("CreateItemGrant edit: %v", err)
			}
			if code := do("PATCH", itemPath, map[string]interface{}{"title": "Edited"}); code != http.StatusOK {
				t.Errorf("edit grant: PATCH = %d, want 200", code)
			}
			collabReq2 := httptest.NewRequest("GET", "/api/v1/collab/"+target.ID, nil)
			collabReq2 = collabReq2.WithContext(context.WithValue(collabReq2.Context(), ctxCurrentUser, editor))
			if access, err := f.srv.authorizeCollabAccess(collabReq2, target); err != nil || !access.canWrite {
				t.Errorf("edit grant: collab access = %+v, %v; want writable", access, err)
			}
		})
	}
}

// A restricted member with the OWNER role keeps the full edit bypass (PR #1756
// codex round 3, lead ruling): an owner can rewrite their own collection
// access, and the web client's canEditItem treats owners as editors
// everywhere, so the server must agree. Same shape as the editor test above,
// where the same request is refused: only the role differs.
func TestTask3376_RestrictedOwnerRoleKeepsEdit(t *testing.T) {
	f := newRefResolverFixture(t)
	st := f.srv.store
	listed, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Listed", Slug: "listed", Prefix: "LST"})
	if err != nil {
		t.Fatalf("CreateCollection listed: %v", err)
	}
	hidden, err := st.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Hidden", Slug: "hidden", Prefix: "HID"})
	if err != nil {
		t.Fatalf("CreateCollection hidden: %v", err)
	}
	target, err := st.CreateItem(f.ws.ID, hidden.ID, models.ItemCreate{Title: "Granted item"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	co, err := st.CreateUser(models.UserCreate{Email: "co@example.com", Name: "Co", Username: "coowner", Password: "pw-test-12345"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Role owner, but NOT workspaces.owner_id, so ResolveUserPermission's
	// owner_id step does not apply and the role path is what is measured.
	if err := st.AddWorkspaceMember(f.ws.ID, co.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := st.SetMemberCollectionAccess(f.ws.ID, co.ID, "specific", []string{listed.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	if _, err := st.CreateItemGrant(f.ws.ID, target.ID, co.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	tok, err := st.CreateAPIToken(co.ID, models.APITokenCreate{Name: "co-tok", WorkspaceID: f.ws.ID}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if rr := f.doAuth(tok.Token, "PATCH", "/api/v1/workspaces/"+f.ws.Slug+"/items/"+target.Ref, map[string]interface{}{"title": "Owner edit"}); rr.Code != http.StatusOK {
		t.Errorf("restricted owner role: PATCH = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	collabReq := httptest.NewRequest("GET", "/api/v1/collab/"+target.ID, nil)
	collabReq = collabReq.WithContext(context.WithValue(collabReq.Context(), ctxCurrentUser, co))
	if access, err := f.srv.authorizeCollabAccess(collabReq, target); err != nil || !access.canWrite {
		t.Errorf("restricted owner role: collab access = %+v, %v; want writable", access, err)
	}
}
