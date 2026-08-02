package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// Delete-authorization tests for PLAN-2382 / TASK-2384.
//
// The item-detail attachment strip offers a delete affordance gated on the
// UI's grant-aware `canEdit` (web/src/lib/utils/permissions.ts::canEditItem),
// which returns true for a user holding an item- or collection-level `edit`
// grant regardless of workspace role. The handler used to open with a flat
// requireMinRole("editor"), so that user saw the control and got a 403.
//
// handleDeleteWorkspaceAttachment now authorizes per-attachment, mirroring
// the upload handler's BUG-1661 pattern: item-bound attachments go through
// requireItemVisible → requireEditPermission (in that order), orphans keep
// the flat editor-role gate.

// deleteAsUser issues DELETE /attachments/{id} as a specific user + workspace
// role, bypassing the auth middleware the same way uploadAsGuest does.
func deleteAsUser(srv *Server, wsID, attachmentID string, user *models.User, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", "/api/v1/workspaces/x/attachments/"+attachmentID, nil)
	req.RemoteAddr = "127.0.0.1:1234"

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("attachmentID", attachmentID)

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
	ctx = context.WithValue(ctx, ctxCurrentUser, user)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, role)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleDeleteWorkspaceAttachment(rr, req)
	return rr
}

// authzFixture builds a workspace with one item plus an attachment bound to
// it, and (optionally) a second free-floating orphan attachment.
type authzFixture struct {
	wsID   string
	itemID string
	attID  string
	orphan string
}

func newDeleteAuthzFixture(t *testing.T, srv *Server) authzFixture {
	t.Helper()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DeleteAuthz"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Granted", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	bound := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		UploadedBy:  "system",
		StorageKey:  "fs:authz-bound",
		ContentHash: "authzhash-bound",
		MimeType:    "image/png",
		SizeBytes:   64,
		Filename:    "bound.png",
	}
	if err := srv.store.CreateAttachment(bound); err != nil {
		t.Fatalf("CreateAttachment bound: %v", err)
	}

	orphan := &models.Attachment{
		WorkspaceID: ws.ID,
		UploadedBy:  "system",
		StorageKey:  "fs:authz-orphan",
		ContentHash: "authzhash-orphan",
		MimeType:    "image/png",
		SizeBytes:   64,
		Filename:    "orphan.png",
	}
	if err := srv.store.CreateAttachment(orphan); err != nil {
		t.Fatalf("CreateAttachment orphan: %v", err)
	}

	return authzFixture{wsID: ws.ID, itemID: item.ID, attID: bound.ID, orphan: orphan.ID}
}

func mkUser(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    email,
		Name:     email,
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return u
}

// TestDeleteAttachment_GrantBasedEditorCanDelete is the core regression for
// TASK-2384: a user with NO workspace editor role but an item-level edit
// grant must be able to delete an attachment on that item — matching the
// affordance the UI already renders for them, and matching what upload
// already allows (BUG-1661).
func TestDeleteAttachment_GrantBasedEditorCanDelete(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDeleteAuthzFixture(t, srv)

	granted := mkUser(t, srv, "granted@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, granted.ID, "edit", granted.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	rr := deleteAsUser(srv, f.wsID, f.attID, granted, "guest")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("grant-based delete: status = %d, want 204; body = %s", rr.Code, rr.Body.String())
	}
}

// TestDeleteAttachment_ViewGrantCannotDelete confirms the fix didn't widen
// access: a VIEW grant is not an EDIT grant. requireEditPermission rejects
// it with 403 (the item is visible, so non-disclosure doesn't apply).
func TestDeleteAttachment_ViewGrantCannotDelete(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDeleteAuthzFixture(t, srv)

	viewer := mkUser(t, srv, "viewonly@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, viewer.ID, "view", viewer.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	rr := deleteAsUser(srv, f.wsID, f.attID, viewer, "guest")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("view-grant delete: status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
}

// TestDeleteAttachment_InvisibleItemIs404Not403 pins the ORDERING that
// Codex round 2 on PLAN-2382 called out: visibility is checked BEFORE edit
// permission, so an attachment whose parent item the caller cannot see
// returns 404 rather than 403. A 403 here would confirm the attachment
// exists — the non-disclosure behavior the handler deliberately preserves.
func TestDeleteAttachment_InvisibleItemIs404Not403(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDeleteAuthzFixture(t, srv)

	stranger := mkUser(t, srv, "stranger@test.com")

	rr := deleteAsUser(srv, f.wsID, f.attID, stranger, "guest")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("invisible-item delete: status = %d, want 404 (not 403 — that would "+
			"disclose the attachment exists); body = %s", rr.Code, rr.Body.String())
	}
}

// TestDeleteAttachment_ForeignItemGrantCannotReachAcrossWorkspaces pins the
// P1 Codex caught on this change.
//
// attachments.item_id has no FK or same-workspace constraint, and the upload
// handler stores the raw ?item_id verbatim, so a row in workspace A can point
// at an item in workspace B. Two downstream checks each fail to catch that
// alone: checkItemVisible admits any collection id when the caller is
// unrestricted in A, and ResolveUserPermission matches item grants by item_id
// with no workspace scoping (store/grants.go).
//
// Composed, that meant a user who was merely a VIEWER in A but held an edit
// grant on some item in B could delete A's attachment — an escalation the old
// flat editor gate blocked by accident. The handler now rejects a parent item
// whose WorkspaceID isn't the requested workspace, before either check.
func TestDeleteAttachment_ForeignItemGrantCannotReachAcrossWorkspaces(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	// Workspace B: the attacker holds a genuine edit grant here.
	bFixture := newDeleteAuthzFixture(t, srv)

	// Workspace A: holds an attachment whose item_id points into B.
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VictimA"})
	if err != nil {
		t.Fatalf("CreateWorkspace A: %v", err)
	}
	crossed := &models.Attachment{
		WorkspaceID: wsA.ID,
		ItemID:      &bFixture.itemID, // foreign parent — the whole point
		UploadedBy:  "system",
		StorageKey:  "fs:crossed",
		ContentHash: "authzhash-crossed",
		MimeType:    "image/png",
		SizeBytes:   64,
		Filename:    "crossed.png",
	}
	if err := srv.store.CreateAttachment(crossed); err != nil {
		t.Fatalf("CreateAttachment crossed: %v", err)
	}

	attacker := mkUser(t, srv, "cross-ws@test.com")
	// Full (unrestricted) VIEWER membership in A — enough to pass A's
	// collection-visibility filter, not enough to delete anything in A.
	if err := srv.store.AddWorkspaceMember(wsA.ID, attacker.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// ...plus a real edit grant on the B item.
	if _, err := srv.store.CreateItemGrant(bFixture.wsID, bFixture.itemID, attacker.ID, "edit", attacker.ID); err != nil {
		t.Fatalf("CreateItemGrant B: %v", err)
	}

	rr := deleteAsUser(srv, wsA.ID, crossed.ID, attacker, "viewer")
	if rr.Code == http.StatusNoContent {
		t.Fatalf("cross-workspace delete succeeded: a B edit grant must not " +
			"authorize deleting an attachment in A")
	}
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-workspace delete: status = %d, want 404 (non-disclosure); body = %s",
			rr.Code, rr.Body.String())
	}
}

// TestDeleteAttachment_OrphanStillRequiresEditorRole confirms the orphan
// branch kept the flat workspace editor-role gate. An orphan carries no item
// context to authorize against, so a grant on some other item must not
// unlock it.
func TestDeleteAttachment_OrphanStillRequiresEditorRole(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDeleteAuthzFixture(t, srv)

	granted := mkUser(t, srv, "orphan-probe@test.com")
	// A full-access VIEWER member, not a stranger. Without the membership the
	// request is refused by guestResourceFilter's restricted-user 404 and the
	// test would pass even with the orphan role gate deleted — it wouldn't pin
	// the rule it names (Codex round 10). As a full-access viewer the only
	// thing standing between this user and the orphan is requireMinRole.
	if err := srv.store.AddWorkspaceMember(f.wsID, granted.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, granted.ID, "edit", granted.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	rr := deleteAsUser(srv, f.wsID, f.orphan, granted, "viewer")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("orphan delete by a viewer holding an unrelated item grant: "+
			"status = %d, want 403 (the item grant must not unlock free-floating "+
			"attachments); body = %s", rr.Code, rr.Body.String())
	}

	// A real workspace editor still can. The membership row matters: the
	// orphan branch also runs guestResourceFilter, which treats a user with
	// no membership as restricted and 404s them regardless of the role
	// asserted on the request context.
	editor := mkUser(t, srv, "editor@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	rr = deleteAsUser(srv, f.wsID, f.orphan, editor, "editor")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("orphan delete by editor: status = %d, want 204; body = %s", rr.Code, rr.Body.String())
	}
}

// TestDeleteAttachment_DenialBodiesAreByteIdentical pins the non-disclosure
// invariant on the DELETE path at the level it actually has to hold: the
// RESPONSE BODY, not just the status code.
//
// Every test above asserts 404 and stops there, which let a real oracle sit
// in plain sight — invisible parents were routed through requireItemVisible,
// whose body says "Item not found", while a missing or foreign attachment
// said "Attachment not found". Same status, different bytes: enough to sort
// "this id names an attachment on an item you can't see" from "this id names
// nothing". The read and transform paths closed the same oracle class by
// funnelling every denial through writeAttachmentNotFound; this path now
// shares that writer, and this test is what keeps it shared.
//
// The four denials compared are the ones a prober can actually reach.
func TestDeleteAttachment_DenialBodiesAreByteIdentical(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDeleteAuthzFixture(t, srv)

	// An attachment in f.wsID whose item_id points into ANOTHER workspace.
	otherWS, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DenialForeign"})
	if err != nil {
		t.Fatalf("CreateWorkspace foreign: %v", err)
	}
	otherCol, err := srv.store.CreateCollection(otherWS.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection foreign: %v", err)
	}
	otherItem, err := srv.store.CreateItem(otherWS.ID, otherCol.ID, models.ItemCreate{
		Title: "Foreign", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem foreign: %v", err)
	}
	foreignParent := &models.Attachment{
		WorkspaceID: f.wsID,
		ItemID:      &otherItem.ID,
		UploadedBy:  "system",
		StorageKey:  "fs:denial-foreign",
		ContentHash: "authzhash-denial-foreign",
		MimeType:    "image/png",
		SizeBytes:   64,
		Filename:    "foreign-parent.png",
	}
	if err := srv.store.CreateAttachment(foreignParent); err != nil {
		t.Fatalf("CreateAttachment foreignParent: %v", err)
	}

	// A second collection in f.wsID, so a restricted member can hold
	// "specific" collection access to something real while still being
	// restricted — that is the axis the orphan gate keys on.
	sideCol, err := srv.store.CreateCollection(f.wsID, models.CollectionCreate{
		Name: "Side", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection side: %v", err)
	}

	// The baseline: a plain lookup miss, taken as a full-access owner so it
	// is unambiguously "no such attachment" and not itself an authz denial.
	owner := mkUser(t, srv, "denial-owner@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember owner: %v", err)
	}
	baseline := deleteAsUser(srv, f.wsID, "00000000-0000-0000-0000-000000000000", owner, "owner")
	if baseline.Code != http.StatusNotFound {
		t.Fatalf("baseline lookup miss: status = %d, want 404; body = %s",
			baseline.Code, baseline.Body.String())
	}

	// An unrestricted-but-nonmember caller: the item is invisible to them, so
	// this is the requireItemVisible path that used to answer "Item not found".
	stranger := mkUser(t, srv, "denial-stranger@test.com")

	// A restricted editor probing the orphan.
	restricted := mkUser(t, srv, "denial-restricted@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, restricted.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember restricted: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(f.wsID, restricted.ID, "specific",
		[]string{sideCol.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	for _, tc := range []struct {
		label string
		attID string
		user  *models.User
		role  string
	}{
		{"invisible parent item", f.attID, stranger, "guest"},
		{"foreign parent item", foreignParent.ID, owner, "owner"},
		{"orphan as restricted editor", f.orphan, restricted, "editor"},
	} {
		rr := deleteAsUser(srv, f.wsID, tc.attID, tc.user, tc.role)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404; body = %s", tc.label, rr.Code, rr.Body.String())
			continue
		}
		if got, want := rr.Body.String(), baseline.Body.String(); got != want {
			t.Errorf("%s: body = %q, lookup-miss body = %q — must be byte-identical, "+
				"otherwise the response distinguishes a real attachment from a bad id",
				tc.label, got, want)
		}
	}
}
