package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// deleteControlledStore is a minimal AttachmentStore whose Delete result
// is scriptable, so a test can force a backend failure (transient FS/S3
// error) and then flip it to success to prove retry self-heals. Only
// Delete is exercised by the purge sweeper; the rest return errors so a
// stray call is loud.
type deleteControlledStore struct{ deleteErr error }

func (d *deleteControlledStore) Put(context.Context, string, string, io.Reader) (string, error) {
	return "", errors.New("deleteControlledStore: Put not supported")
}
func (d *deleteControlledStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("deleteControlledStore: Get not supported")
}
func (d *deleteControlledStore) Stat(context.Context, string) (int64, error) {
	return 0, errors.New("deleteControlledStore: Stat not supported")
}
func (d *deleteControlledStore) Delete(context.Context, string) error { return d.deleteErr }

// softDeleteWorkspaceAt stamps a workspace's deleted_at to a specific
// time so the sweep's age cutoff can be exercised deterministically —
// there's no API to backdate. Server tests run on SQLite, so raw "?"
// placeholders are fine (mirrors orphan_gc_test.go).
func softDeleteWorkspaceAt(t *testing.T, srv *Server, wsID string, at time.Time) {
	t.Helper()
	ts := at.UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE workspaces SET deleted_at = ? WHERE id = ?`, ts, wsID,
	); err != nil {
		t.Fatalf("soft-delete backdate: %v", err)
	}
}

func blobPresent(t *testing.T, srv *Server, storageKey string) bool {
	t.Helper()
	store, err := srv.attachments.Resolve(storageKey)
	if err != nil {
		t.Fatalf("resolve backend: %v", err)
	}
	_, err = store.Stat(context.Background(), storageKey)
	if err == nil {
		return true
	}
	if errors.Is(err, attachments.ErrNotFound) {
		return false
	}
	t.Fatalf("stat blob: %v", err)
	return false
}

// TestWorkspacePurgeSweep_PurgesEligibleReclaimsBlob is the main case:
// a workspace soft-deleted past the retention window is hard-deleted and
// its attachment blob reclaimed through the storage backend.
func TestWorkspacePurgeSweep_PurgesEligibleReclaimsBlob(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	if rr := doMultipartUpload(srv, slug, "doomed.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	attID := getOnlyAttachmentID(t, srv, wsID)
	att, err := srv.store.GetAttachment(attID)
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	storageKey := att.StorageKey
	if !blobPresent(t, srv, storageKey) {
		t.Fatalf("blob missing before sweep")
	}

	// Soft-deleted 31 days ago → eligible for a 30-day cutoff.
	softDeleteWorkspaceAt(t, srv, wsID, time.Now().Add(-31*24*time.Hour))

	res, err := srv.runWorkspacePurgeSweep(context.Background(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 1 {
		t.Errorf("Purged=%d, want 1", res.Purged)
	}
	if res.BlobsReclaimed < 1 {
		t.Errorf("BlobsReclaimed=%d, want >= 1", res.BlobsReclaimed)
	}

	// Workspace row gone (query bypasses the deleted_at filter).
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, wsID).Scan(&n); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if n != 0 {
		t.Errorf("workspace row still present after purge")
	}
	// Attachment row gone + blob reclaimed.
	if got, _ := srv.store.GetAttachment(attID); got != nil {
		t.Errorf("attachment row still present after purge")
	}
	if blobPresent(t, srv, storageKey) {
		t.Errorf("blob still on disk after purge")
	}
}

// TestWorkspacePurgeSweep_LeavesRecentAndLiveUntouched pins the
// no-over-purge guarantee: a workspace soft-deleted inside the window
// and a live workspace are both invisible to the sweep.
func TestWorkspacePurgeSweep_LeavesRecentAndLiveUntouched(t *testing.T) {
	srv, liveSlug := testServerWithAttachments(t)
	liveWS := workspaceIDForSlug(t, srv, liveSlug)

	recentSlug := createWSForTest(t, srv)
	recentWS := workspaceIDForSlug(t, srv, recentSlug)
	// Soft-deleted only 29 days ago — inside the 30-day window.
	softDeleteWorkspaceAt(t, srv, recentWS, time.Now().Add(-29*24*time.Hour))

	res, err := srv.runWorkspacePurgeSweep(context.Background(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Eligible != 0 || res.Purged != 0 {
		t.Errorf("expected nothing eligible/purged, got eligible=%d purged=%d", res.Eligible, res.Purged)
	}

	for _, id := range []string{liveWS, recentWS} {
		var n int
		if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count workspace: %v", err)
		}
		if n != 1 {
			t.Errorf("workspace %s should be untouched, got %d rows", id, n)
		}
	}
}

// TestWorkspacePurgeSweep_CrossWorkspaceDedupKeepsSharedBlob pins the
// content-addressed safety: purging one workspace must NOT delete a blob
// that a different workspace still references (identical bytes dedupe to
// one physical blob).
func TestWorkspacePurgeSweep_CrossWorkspaceDedupKeepsSharedBlob(t *testing.T) {
	srv, doomedSlug := testServerWithAttachments(t)
	keeperSlug := createWSForTest(t, srv)

	// Same bytes uploaded to both workspaces → shared content_hash → one
	// physical blob.
	if rr := doMultipartUpload(srv, doomedSlug, "shared.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload doomed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doMultipartUpload(srv, keeperSlug, "shared.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload keeper: %d %s", rr.Code, rr.Body.String())
	}

	doomedWS := workspaceIDForSlug(t, srv, doomedSlug)
	keeperWS := workspaceIDForSlug(t, srv, keeperSlug)
	keeperAtt, err := srv.store.GetAttachment(getOnlyAttachmentID(t, srv, keeperWS))
	if err != nil || keeperAtt == nil {
		t.Fatalf("GetAttachment keeper: %v", err)
	}
	sharedKey := keeperAtt.StorageKey

	softDeleteWorkspaceAt(t, srv, doomedWS, time.Now().Add(-31*24*time.Hour))

	res, err := srv.runWorkspacePurgeSweep(context.Background(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 1 {
		t.Errorf("Purged=%d, want 1", res.Purged)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0 — shared blob must survive", res.BlobsReclaimed)
	}

	// Keeper workspace + its attachment + the shared blob all intact.
	if got, _ := srv.store.GetAttachment(keeperAtt.ID); got == nil {
		t.Errorf("keeper attachment row wrongly removed")
	}
	if !blobPresent(t, srv, sharedKey) {
		t.Errorf("shared blob wrongly deleted while keeper still references it")
	}
}

// TestWorkspacePurgeSweep_BlobFailureDefersNoOrphan is the P1 guard: if
// a blob can't be reclaimed (transient backend error), the workspace is
// DEFERRED — its DB rows are left intact so it stays eligible — rather
// than purged into an orphaned blob. A subsequent sweep with a healthy
// backend then completes the purge (idempotent self-heal).
func TestWorkspacePurgeSweep_BlobFailureDefersNoOrphan(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	// A blob whose backend Delete fails. Routed via a "ctl" prefix so
	// Resolve dispatches to our scriptable store.
	ctl := &deleteControlledStore{deleteErr: errors.New("s3 unavailable")}
	srv.attachments.Register("ctl", ctl)
	attID := "att-blobfail-1"
	if err := srv.store.CreateAttachment(&models.Attachment{
		ID: attID, WorkspaceID: wsID, UploadedBy: "tester",
		StorageKey: "ctl:blob-1", ContentHash: "hash-1",
		MimeType: "image/png", SizeBytes: 10, Filename: "x.png",
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	softDeleteWorkspaceAt(t, srv, wsID, time.Now().Add(-31*24*time.Hour))

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	res, err := srv.runWorkspacePurgeSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 0 || res.Skipped != 1 {
		t.Errorf("blob failure should defer: got purged=%d skipped=%d", res.Purged, res.Skipped)
	}
	// Workspace + attachment row must still exist — nothing orphaned.
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, wsID).Scan(&n); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if n != 1 {
		t.Errorf("deferred workspace row wrongly removed")
	}
	if got, _ := srv.store.GetAttachment(attID); got == nil {
		t.Errorf("attachment row wrongly removed while its blob delete failed")
	}

	// Heal the backend; the retry now completes the purge.
	ctl.deleteErr = nil
	res, err = srv.runWorkspacePurgeSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	if res.Purged != 1 {
		t.Errorf("retry should purge: got purged=%d skipped=%d", res.Purged, res.Skipped)
	}
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, wsID).Scan(&n); err != nil {
		t.Fatalf("count workspace: %v", err)
	}
	if n != 0 {
		t.Errorf("workspace not purged on healthy retry")
	}
}

// TestWorkspacePurgeSweep_Idempotent pins that re-running the sweep after
// everything eligible is purged is a clean no-op — a single run doesn't
// wedge the loop and repeats are safe.
func TestWorkspacePurgeSweep_Idempotent(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)
	softDeleteWorkspaceAt(t, srv, wsID, time.Now().Add(-31*24*time.Hour))

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	first, err := srv.runWorkspacePurgeSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.Purged != 1 {
		t.Fatalf("first Purged=%d, want 1", first.Purged)
	}
	second, err := srv.runWorkspacePurgeSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Eligible != 0 || second.Purged != 0 {
		t.Errorf("second sweep not a no-op: eligible=%d purged=%d", second.Eligible, second.Purged)
	}
}
