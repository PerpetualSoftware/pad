package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3319: the member-route blob read names when and by whom the ORIGINAL
// was uploaded, for the viewer's caption. A variant's own row is when it was
// derived, so a thumb request must still report the original's time.

func TestAttachmentUploadedHeaders_OriginalAndVariant(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)
	uploader, err := srv.store.CreateUser(models.UserCreate{Email: "zoe@test.com", Name: "Zoë Ürdal", Password: "pw-zoe-123456"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	orig := putBlob(t, srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &f.itemID, Filename: "shot.png", UploadedBy: uploader.ID,
	}, distinctPNG(t, 0x51))
	// created_at is whole-second text: the variant must be derived LATER for
	// the thumb leg to tell the two times apart.
	time.Sleep(1100 * time.Millisecond)
	variant := models.AttachmentVariantThumbMd
	putBlob(t, srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &f.itemID, ParentID: &orig.ID, Variant: &variant, Filename: "shot-thumb-md.png",
	}, distinctPNG(t, 0x52))

	stored, err := srv.store.GetAttachment(orig.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	wantAt := stored.CreatedAt.UTC().Format(time.RFC3339)

	viewer := mkUser(t, srv, "viewer-3319@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, v := range []string{"", models.AttachmentVariantThumbMd} {
			rr := downloadAs(srv, method, f.wsID, orig.ID, v, viewer, "viewer")
			if rr.Code != http.StatusOK {
				t.Fatalf("%s variant=%q: status %d", method, v, rr.Code)
			}
			if v != "" && rr.Header().Get("X-Pad-Attachment-Variant") != v {
				t.Fatalf("precondition: %s variant=%q served %q", method, v, rr.Header().Get("X-Pad-Attachment-Variant"))
			}
			if got := rr.Header().Get("X-Pad-Attachment-Uploaded-At"); got != wantAt {
				t.Errorf("%s variant=%q: Uploaded-At = %q, want the original's %q", method, v, got, wantAt)
			}
			raw := rr.Header().Get("X-Pad-Attachment-Uploaded-By")
			name, uErr := url.PathUnescape(raw)
			if uErr != nil || name != "Zoë Ürdal" {
				t.Errorf("%s variant=%q: Uploaded-By = %q (decoded %q, %v), want Zoë Ürdal", method, v, raw, name, uErr)
			}
		}
	}
}

// An uploader that does not resolve to a named user (the "system" default,
// a deleted account) leaves the name out rather than inventing one.
func TestAttachmentUploadedHeaders_UnresolvedUploaderHasNoName(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)
	viewer := mkUser(t, srv, "viewer-3319b@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	rr := downloadAs(srv, http.MethodHead, f.wsID, f.origID, "", viewer, "viewer")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if rr.Header().Get("X-Pad-Attachment-Uploaded-At") == "" {
		t.Error("Uploaded-At missing")
	}
	if got := rr.Header().Get("X-Pad-Attachment-Uploaded-By"); got != "" {
		t.Errorf("Uploaded-By = %q for a 'system' upload, want absent", got)
	}
}

// The share page serves attachments to anonymous viewers: it names no one.
func TestAttachmentUploadedHeaders_ShareRouteNamesNoOne(t *testing.T) {
	f := newShareAssetFixture(t)
	orig := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &f.itemID, Filename: "s.png", UploadedBy: f.ownerID,
	}, distinctPNG(t, 0x61))
	variant := models.AttachmentVariantThumbMd
	putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &f.itemID, ParentID: &orig.ID, Variant: &variant, Filename: "s-thumb-md.png",
	}, distinctPNG(t, 0x62))
	f.setContent(t, f.itemID, imageRef(orig.ID))
	link := f.createLink(t, "item", f.itemID, nil)
	f.resolvedRefs(t, link.Token, "")
	rr := f.getAsset(link.Token, orig.ID, "variant="+models.AttachmentVariantThumbMd)
	if rr.Code != http.StatusOK {
		t.Fatalf("precondition: share asset status %d: %s", rr.Code, rr.Body.String())
	}
	for _, h := range []string{"X-Pad-Attachment-Uploaded-By", "X-Pad-Attachment-Uploaded-At"} {
		if got := rr.Header().Get(h); got != "" {
			t.Errorf("share route set %s = %q", h, got)
		}
	}
}
