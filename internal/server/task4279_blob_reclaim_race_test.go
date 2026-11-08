package server

import (
	"context"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4279 (audit 10-29, high 4): both blob reclaimers counted the rows
// protecting a hash BEFORE taking the in-flight lock. An upload of the same
// content that marked, Put, committed its row and released entirely between
// the count and the lock left the reclaimer looking at "nothing in flight" and
// "nothing protecting", and it deleted bytes the new row points at. The hook
// commits that finished upload at exactly that point; the reclaimer must
// re-check under the lock and keep the blob.

func lateUploadRow(wsID string, base *models.Attachment) *models.Attachment {
	return &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "late-writer",
		StorageKey:  base.StorageKey,
		ContentHash: base.ContentHash,
		MimeType:    base.MimeType,
		SizeBytes:   base.SizeBytes,
		Filename:    "late.png",
	}
}

func TestOrphanGC_KeepsBlobForAnUploadThatCommitsAfterTheCount(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "victim.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, wsID)
	att, err := srv.store.GetAttachment(id)
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil); rr.Code != 204 {
		t.Fatalf("delete: %d", rr.Code)
	}

	hookRan := false
	srv.orphanPostCountHook = func(h string) {
		if h != att.ContentHash {
			return
		}
		hookRan = true
		if err := srv.store.CreateAttachment(lateUploadRow(wsID, att)); err != nil {
			t.Errorf("hook CreateAttachment: %v", err)
		}
	}
	defer func() { srv.orphanPostCountHook = nil }()

	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !hookRan {
		t.Fatal("post-count hook never ran: the race leg was not exercised")
	}
	if res.Deleted < 1 {
		t.Errorf("Deleted=%d, want >= 1 (the orphan row still goes)", res.Deleted)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0: a row committed after the count protects the bytes", res.BlobsReclaimed)
	}
	if !blobPresent(t, srv, att.StorageKey) {
		t.Error("blob deleted although a row committed after the count points at it")
	}
}

func TestWorkspacePurge_KeepsBlobForAnUploadThatCommitsAfterTheCount(t *testing.T) {
	t.Parallel()
	srv, doomedSlug := testServerWithAttachments(t)
	keeperSlug := createWSForTest(t, srv)

	if rr := doMultipartUpload(srv, doomedSlug, "doomed.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	doomedWS := workspaceIDForSlug(t, srv, doomedSlug)
	keeperWS := workspaceIDForSlug(t, srv, keeperSlug)
	att, err := srv.store.GetAttachment(getOnlyAttachmentID(t, srv, doomedWS))
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	softDeleteWorkspaceAt(t, srv, doomedWS, time.Now().Add(-31*24*time.Hour))

	hookRan := false
	var lateID string
	srv.purgePostCountHook = func(h string) {
		if h != att.ContentHash {
			return
		}
		hookRan = true
		late := lateUploadRow(keeperWS, att)
		if err := srv.store.CreateAttachment(late); err != nil {
			t.Errorf("hook CreateAttachment: %v", err)
		}
		lateID = late.ID
	}
	defer func() { srv.purgePostCountHook = nil }()

	res, err := srv.runWorkspacePurgeSweep(context.Background(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !hookRan {
		t.Fatal("post-count hook never ran: the race leg was not exercised")
	}
	if res.Purged != 1 {
		t.Errorf("Purged=%d, want 1 (the doomed workspace still goes)", res.Purged)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0: another workspace's row committed after the count", res.BlobsReclaimed)
	}
	if got, _ := srv.store.GetAttachment(lateID); got == nil {
		t.Error("the late upload's row was removed")
	}
	if !blobPresent(t, srv, att.StorageKey) {
		t.Error("blob deleted although another workspace's row, committed after the count, points at it")
	}
}

// A recount that cannot answer (here: the sweep's context ends between the
// early count and the lock) must keep the blob, never delete on an unknown.
func TestOrphanGC_KeepsBlobWhenTheRecountCannotAnswer(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)
	if rr := doMultipartUpload(srv, slug, "victim.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, wsID)
	att, err := srv.store.GetAttachment(id)
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil); rr.Code != 204 {
		t.Fatalf("delete: %d", rr.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hookRan := false
	srv.orphanPostCountHook = func(string) { hookRan = true; cancel() }
	defer func() { srv.orphanPostCountHook = nil }()

	res, err := srv.runOrphanGCSweep(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !hookRan {
		t.Fatal("post-count hook never ran: the failed-recount leg was not exercised")
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0: the recount could not answer", res.BlobsReclaimed)
	}
	if !blobPresent(t, srv, att.StorageKey) {
		t.Error("blob deleted although the recount under the lock failed")
	}
}

func TestWorkspacePurge_DefersWhenTheRecountCannotAnswer(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	if rr := doMultipartUpload(srv, slug, "doomed.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	att, err := srv.store.GetAttachment(getOnlyAttachmentID(t, srv, wsID))
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	softDeleteWorkspaceAt(t, srv, wsID, time.Now().Add(-31*24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hookRan := false
	srv.purgePostCountHook = func(string) { hookRan = true; cancel() }
	defer func() { srv.purgePostCountHook = nil }()

	res, err := srv.runWorkspacePurgeSweep(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !hookRan {
		t.Fatal("post-count hook never ran: the failed-recount leg was not exercised")
	}
	if res.BlobsReclaimed != 0 || res.Purged != 0 {
		t.Errorf("Purged=%d BlobsReclaimed=%d, want 0 and 0: the workspace is deferred", res.Purged, res.BlobsReclaimed)
	}
	if !blobPresent(t, srv, att.StorageKey) {
		t.Error("blob deleted although the recount under the lock failed")
	}
	if got, _ := srv.store.GetAttachment(att.ID); got == nil {
		t.Error("attachment row removed although the workspace was deferred")
	}
}
