package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-2247: attach binds an UNATTACHED attachment to an item, which is what
// makes an image uploaded with `upload -` and then embedded render on that
// item's share. The rules under test: unattached only (never a move), same
// workspace, a live original (not a variant), the caller can see and edit the
// item, and the caller uploaded the attachment or is a workspace owner.

// attachAs POSTs an attach as a specific user + workspace role, bypassing the
// auth middleware the way transformAs does.
func attachAs(srv *Server, wsID, attachmentID, itemRef string, user *models.User, role string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"item": itemRef})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/x/attachments/"+attachmentID+"/attach", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", "x")
	rctx.URLParams.Add("attachmentID", attachmentID)

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
	ctx = context.WithValue(ctx, ctxCurrentUser, user)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, role)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleAttachAttachment(rr, req)
	return rr
}

// seedOrphanImage creates an UNATTACHED image original uploaded by uploader,
// plus its thumb-md variant (unattached too, as upload leaves them), and
// returns (originalID, variantID, thumbMdBytes).
func seedOrphanImage(t *testing.T, f shareAssetFixture, uploader string, seed byte) (string, string, []byte) {
	t.Helper()
	orig := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, UploadedBy: uploader, Filename: "orphan.png",
	}, distinctPNG(t, seed))
	mdBody := distinctPNG(t, seed+1)
	variant := models.AttachmentVariantThumbMd
	v := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, UploadedBy: uploader, ParentID: &orig.ID,
		Variant: &variant, Filename: "orphan-thumb-md.png",
	}, mdBody)
	return orig.ID, v.ID, mdBody
}

func boundItemID(t *testing.T, srv *Server, id string) string {
	t.Helper()
	a, err := srv.store.GetAttachment(id)
	if err != nil || a == nil {
		t.Fatalf("GetAttachment(%s): %v", id, err)
	}
	if a.ItemID == nil {
		return ""
	}
	return *a.ItemID
}

func attachMember(t *testing.T, f shareAssetFixture, email, role string) *models.User {
	t.Helper()
	u := mkUser(t, f.srv, email)
	if err := f.srv.store.AddWorkspaceMember(f.wsID, u.ID, role); err != nil {
		t.Fatalf("AddWorkspaceMember %s: %v", email, err)
	}
	return u
}

// The goal: an unattached image an item embeds is a placeholder on that
// item's share (404 from the asset endpoint, no ref minted) until attach, and
// served afterwards. The variant moves with the original.
func TestAttach_UploaderBindsOrphanAndShareServesIt(t *testing.T) {
	f := newShareAssetFixture(t)
	uploader := attachMember(t, f, "uploader@test.com", "editor")
	orig, variantID, md := seedOrphanImage(t, f, uploader.ID, 0x41)
	f.setContent(t, f.itemID, imageRef(orig))
	link := f.createLink(t, "item", f.itemID, nil)

	if rr := f.getAsset(link.Token, orig, "variant=thumb-md"); rr.Code != http.StatusNotFound {
		t.Fatalf("before attach: share served an unattached image, status %d", rr.Code)
	}
	if _, ok := f.resolvedRefs(t, link.Token, "")[orig]; ok {
		t.Fatalf("before attach: share minted a ref for an unattached image")
	}

	rr := attachAs(f.srv, f.wsID, orig, f.itemID, uploader, "editor")
	if rr.Code != http.StatusOK {
		t.Fatalf("attach: status %d, body %s", rr.Code, rr.Body.String())
	}
	if got := boundItemID(t, f.srv, orig); got != f.itemID {
		t.Fatalf("original item_id = %q, want %q", got, f.itemID)
	}
	if got := boundItemID(t, f.srv, variantID); got != f.itemID {
		t.Fatalf("variant item_id = %q, want %q (a variant carries its parent's item)", got, f.itemID)
	}

	if _, ok := f.resolvedRefs(t, link.Token, "")[orig]; !ok {
		t.Fatalf("after attach: share did not mint a ref")
	}
	got := f.getAsset(link.Token, orig, "variant=thumb-md")
	if got.Code != http.StatusOK {
		t.Fatalf("after attach: share asset status %d, body %s", got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), md) {
		t.Errorf("after attach: share served the wrong bytes")
	}
}

// Never a move: an attachment that belongs to an item is refused, and stays
// where it was. A caller who can see the owner gets 409; one who cannot gets
// the shared 404, so the refusal is not an oracle for hidden items.
func TestAttach_RefusesAlreadyAttached(t *testing.T) {
	f := newShareAssetFixture(t)
	editor := attachMember(t, f, "editor@test.com", "editor")
	owned, _ := f.seedImage(t, f.item2ID, 0x51)

	rr := attachAs(f.srv, f.wsID, owned, f.itemID, editor, "editor")
	if rr.Code != http.StatusConflict {
		t.Fatalf("attached row: status %d, want 409; body %s", rr.Code, rr.Body.String())
	}
	if got := boundItemID(t, f.srv, owned); got != f.item2ID {
		t.Fatalf("attached row moved to %q", got)
	}

	hidden, err := f.srv.store.CreateCollection(f.wsID, models.CollectionCreate{Name: "Secrets", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	secret, err := f.srv.store.CreateItem(f.wsID, hidden.ID, models.ItemCreate{Title: "Secret", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	secretAtt, _ := f.seedImage(t, secret.ID, 0x55)
	restricted := attachMember(t, f, "restricted@test.com", "editor")
	if err := f.srv.store.SetMemberCollectionAccess(f.wsID, restricted.ID, "specific", []string{f.collID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	rr = attachAs(f.srv, f.wsID, secretAtt, f.itemID, restricted, "editor")
	assertTransformDenied(t, rr, "restricted editor, attachment on a hidden item")
}

// A foreign workspace's attachment, a variant addressed directly, and an
// unknown id all get the shared 404 and change nothing.
func TestAttach_RefusesForeignVariantAndUnknown(t *testing.T) {
	f := newShareAssetFixture(t)
	uploader := attachMember(t, f, "uploader@test.com", "editor")

	other, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	foreign := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: other.ID, UploadedBy: uploader.ID, Filename: "foreign.png",
	}, distinctPNG(t, 0x61))
	rr := attachAs(f.srv, f.wsID, foreign.ID, f.itemID, uploader, "editor")
	assertTransformDenied(t, rr, "foreign-workspace attachment")
	if got := boundItemID(t, f.srv, foreign.ID); got != "" {
		t.Fatalf("foreign attachment was bound to %q", got)
	}

	_, variantID, _ := seedOrphanImage(t, f, uploader.ID, 0x65)
	rr = attachAs(f.srv, f.wsID, variantID, f.itemID, uploader, "editor")
	assertTransformDenied(t, rr, "variant addressed directly")
	if got := boundItemID(t, f.srv, variantID); got != "" {
		t.Fatalf("variant was bound to %q on its own", got)
	}

	rr = attachAs(f.srv, f.wsID, "00000000-0000-0000-0000-000000000000", f.itemID, uploader, "editor")
	assertTransformDenied(t, rr, "unknown id")
}

// Who may attach: the uploader, or a workspace owner. Another editor may not,
// and a guest cannot even see an unattached row, even one holding an edit
// grant on the target item. That guest case is the escalation this rule
// closes: a guest who could bind any id they know to an item they can edit
// would publish it through that item's share.
func TestAttach_WhoMayAttach(t *testing.T) {
	f := newShareAssetFixture(t)
	uploader := attachMember(t, f, "uploader@test.com", "editor")
	otherEditor := attachMember(t, f, "other@test.com", "editor")
	owner := attachMember(t, f, "wsowner@test.com", "owner")
	viewer := attachMember(t, f, "viewer@test.com", "viewer")

	orig, _, _ := seedOrphanImage(t, f, uploader.ID, 0x71)

	rr := attachAs(f.srv, f.wsID, orig, f.itemID, otherEditor, "editor")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-uploader editor: status %d, want 403; body %s", rr.Code, rr.Body.String())
	}
	// The viewer uploaded this one, so only the edit gate can refuse it.
	viewerOwn, _, _ := seedOrphanImage(t, f, viewer.ID, 0x73)
	rr = attachAs(f.srv, f.wsID, viewerOwn, f.itemID, viewer, "viewer")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer, own upload: status %d, want 403 (cannot edit the item); body %s", rr.Code, rr.Body.String())
	}
	if got := boundItemID(t, f.srv, viewerOwn); got != "" {
		t.Fatalf("viewer bound its upload to %q", got)
	}

	guest := mkUser(t, f.srv, "guest@test.com")
	if _, err := f.srv.store.CreateItemGrant(f.wsID, f.itemID, guest.ID, "edit", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	guestOwn, _, _ := seedOrphanImage(t, f, guest.ID, 0x75)
	for _, id := range []string{orig, guestOwn} {
		rr = attachAs(f.srv, f.wsID, id, f.itemID, guest, "guest")
		assertTransformDenied(t, rr, "edit-grant guest")
	}

	for _, id := range []string{orig, guestOwn} {
		if got := boundItemID(t, f.srv, id); got != "" {
			t.Fatalf("%s was bound to %q by a refused caller", id, got)
		}
	}

	rr = attachAs(f.srv, f.wsID, orig, f.itemID, owner, "owner")
	if rr.Code != http.StatusOK {
		t.Fatalf("workspace owner (not the uploader): status %d, body %s", rr.Code, rr.Body.String())
	}
}

// The target item must resolve and be live.
func TestAttach_TargetItem(t *testing.T) {
	f := newShareAssetFixture(t)
	uploader := attachMember(t, f, "uploader@test.com", "editor")
	orig, _, _ := seedOrphanImage(t, f, uploader.ID, 0x81)

	rr := attachAs(f.srv, f.wsID, orig, "NOPE-999", uploader, "editor")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown item: status %d, want 404", rr.Code)
	}
	if err := f.srv.store.DeleteItem(f.item2ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	rr = attachAs(f.srv, f.wsID, orig, f.item2ID, uploader, "editor")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("archived item: status %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	if got := boundItemID(t, f.srv, orig); got != "" {
		t.Fatalf("bound to %q through a refused target", got)
	}
}

// Attach and the never-attached GC claim are exclusive on the row: whichever
// commits first wins, and the other matches nothing.
func TestAttach_ExclusiveWithNeverAttachedClaim(t *testing.T) {
	f := newShareAssetFixture(t)
	a, _, _ := seedOrphanImage(t, f, "u", 0x91)
	if err := f.srv.store.AttachAttachmentToItem(f.wsID, a, f.itemID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	claimed, err := f.srv.store.ClaimNeverAttachedAttachment(a, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("GC claimed an attached row")
	}

	b, _, _ := seedOrphanImage(t, f, "u", 0x95)
	claimed, err = f.srv.store.ClaimNeverAttachedAttachment(b, time.Now().Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim of a never-attached row: claimed=%v err=%v", claimed, err)
	}
	if err := f.srv.store.AttachAttachmentToItem(f.wsID, b, f.itemID); !errors.Is(err, store.ErrAttachmentNotAttachable) {
		t.Fatalf("attach after claim: err = %v, want ErrAttachmentNotAttachable", err)
	}
}

func TestServerCapabilitiesAdvertisesAttachmentAttach(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, http.MethodGet, "/api/v1/server/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities: status %d", rr.Code)
	}
	var caps map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if caps["attachment_attach"] != true {
		t.Fatalf("attachment_attach = %v, want true", caps["attachment_attach"])
	}
}

// The store refuses to move an attached row on its own, without the handler's
// earlier 409 in front of it: the row changes only while item_id IS NULL.
func TestAttachStore_NeverMovesAnAttachedRow(t *testing.T) {
	f := newShareAssetFixture(t)
	owned, _ := f.seedImage(t, f.item2ID, 0xa1)
	if err := f.srv.store.AttachAttachmentToItem(f.wsID, owned, f.itemID); !errors.Is(err, store.ErrAttachmentNotAttachable) {
		t.Fatalf("attach of an attached row: err = %v, want ErrAttachmentNotAttachable", err)
	}
	if got := boundItemID(t, f.srv, owned); got != f.item2ID {
		t.Fatalf("attached row moved to %q", got)
	}
}

// A restricted member cannot read unattached rows (PLAN-2382 DR-4), so it
// cannot attach one either, even its own upload onto an item it can edit.
func TestAttach_RestrictedMemberGets404(t *testing.T) {
	f := newShareAssetFixture(t)
	restricted := attachMember(t, f, "restricted@test.com", "editor")
	if err := f.srv.store.SetMemberCollectionAccess(f.wsID, restricted.ID, "specific", []string{f.collID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	orig, _, _ := seedOrphanImage(t, f, restricted.ID, 0xb1)
	rr := attachAs(f.srv, f.wsID, orig, f.itemID, restricted, "editor")
	assertTransformDenied(t, rr, "restricted member, own orphan")
	if got := boundItemID(t, f.srv, orig); got != "" {
		t.Fatalf("bound to %q by a restricted member", got)
	}
}

// The store scopes the bind to the workspace on its own: an attachment from
// another workspace is not attachable to an item here, whatever the caller
// checked first.
func TestAttachStore_RefusesForeignWorkspaceRow(t *testing.T) {
	f := newShareAssetFixture(t)
	other, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	foreign := putBlob(t, f.srv, &models.Attachment{WorkspaceID: other.ID, Filename: "foreign.png"}, distinctPNG(t, 0xc1))
	if err := f.srv.store.AttachAttachmentToItem(f.wsID, foreign.ID, f.itemID); !errors.Is(err, store.ErrAttachmentNotAttachable) {
		t.Fatalf("attach of a foreign row: err = %v, want ErrAttachmentNotAttachable", err)
	}
	if got := boundItemID(t, f.srv, foreign.ID); got != "" {
		t.Fatalf("foreign row bound to %q", got)
	}
}
