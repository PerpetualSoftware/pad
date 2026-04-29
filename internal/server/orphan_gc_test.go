package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
)

// TestOrphanGC_ReclaimsSoftDeleted pins TASK-886's main case: a
// soft-deleted attachment past the grace period gets hard-deleted
// from the DB AND its blob removed from the storage backend.
func TestOrphanGC_ReclaimsSoftDeleted(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	body := realPNG()
	rr := doMultipartUpload(srv, slug, "doomed.png", body)
	if rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	id := getOnlyAttachmentID(t, srv, workspaceIDForSlug(t, srv, slug))

	// Soft-delete via the user-facing endpoint.
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil)
	if rr.Code != 204 {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}

	// Sanity check: row exists, soft-deleted.
	att, err := srv.store.GetAttachment(id)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if att == nil || att.DeletedAt == nil {
		t.Fatalf("expected soft-deleted row, got %+v", att)
	}
	storageKey := att.StorageKey
	store, err := srv.attachments.Resolve(storageKey)
	if err != nil {
		t.Fatalf("resolve backend: %v", err)
	}
	if _, err := store.Stat(context.Background(), storageKey); err != nil {
		t.Fatalf("blob missing before GC: %v", err)
	}

	// Run the sweep with a graceCutoff in the future so the soft-
	// deleted row qualifies immediately.
	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("Deleted=%d, want >= 1", res.Deleted)
	}
	if res.BlobsReclaimed < 1 {
		t.Errorf("BlobsReclaimed=%d, want >= 1", res.BlobsReclaimed)
	}
	if res.BytesReclaimed != int64(len(body)) {
		t.Errorf("BytesReclaimed=%d, want %d", res.BytesReclaimed, len(body))
	}

	// DB row gone.
	att, err = srv.store.GetAttachment(id)
	if err != nil {
		t.Fatalf("post-GC GetAttachment: %v", err)
	}
	if att != nil {
		t.Errorf("row still present after GC: %+v", att)
	}
	// Blob gone too.
	if _, err := store.Stat(context.Background(), storageKey); !errors.Is(err, attachments.ErrNotFound) {
		t.Errorf("blob still on disk after GC; Stat err=%v", err)
	}
}

// TestOrphanGC_ReclaimsLongOrphans pins the never-attached path:
// rows with item_id IS NULL AND deleted_at IS NULL aged past the
// grace period get reclaimed.
func TestOrphanGC_ReclaimsLongOrphans(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	if rr := doMultipartUpload(srv, slug, "orphan.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, workspaceIDForSlug(t, srv, slug))

	// Push the row's created_at into the past via direct SQL — the
	// upload handler stamps "now" and there's no API to backdate.
	pastTs := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET created_at = ? WHERE id = ?`, pastTs, id,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Sweep with a 30-day grace cutoff.
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	res, err := srv.runOrphanGCSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("Deleted=%d, want >= 1", res.Deleted)
	}
	if got, _ := srv.store.GetAttachment(id); got != nil {
		t.Errorf("orphan still present after GC: %+v", got)
	}
}

// TestOrphanGC_KeepsRecentRows pins the safety case: rows still
// inside the grace window MUST NOT be reclaimed. Catches a typo
// in the WHERE clause that would silently destroy live attachments.
func TestOrphanGC_KeepsRecentRows(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	if rr := doMultipartUpload(srv, slug, "fresh.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, workspaceIDForSlug(t, srv, slug))

	// Soft-delete it but use a grace cutoff way in the past so the
	// row is NOT yet past grace.
	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil)
	if rr.Code != 204 {
		t.Fatalf("delete: %d", rr.Code)
	}
	cutoff := time.Now().UTC().Add(-365 * 24 * time.Hour)
	res, err := srv.runOrphanGCSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted=%d, want 0 (row still in grace)", res.Deleted)
	}
	// Row must still exist (soft-deleted).
	if att, _ := srv.store.GetAttachment(id); att == nil {
		t.Errorf("row hard-deleted while still in grace")
	}
}

// TestOrphanGC_PreservesSharedBlob pins the dedupe-safety case:
// when two rows reference the same content_hash and only one is
// orphan, the row gets hard-deleted but the blob stays on disk so
// the other row keeps working.
func TestOrphanGC_PreservesSharedBlob(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	body := realPNG()
	// Two uploads with identical bytes → same content_hash → one
	// physical blob on disk, two attachment rows.
	if rr := doMultipartUpload(srv, slug, "a.png", body); rr.Code != 201 {
		t.Fatalf("upload a: %d", rr.Code)
	}
	if rr := doMultipartUpload(srv, slug, "b.png", body); rr.Code != 201 {
		t.Fatalf("upload b: %d", rr.Code)
	}
	wsID := workspaceIDForSlug(t, srv, slug)

	// Pull both row IDs via direct SQL — easier than the public list
	// API, which paginates and doesn't expose storage_key.
	var firstID, secondID, sharedKey string
	dbRows, err := srv.store.DB().Query(
		`SELECT id, storage_key FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, wsID)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	for dbRows.Next() {
		var id, key string
		if err := dbRows.Scan(&id, &key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if firstID == "" {
			firstID = id
			sharedKey = key
		} else {
			secondID = id
		}
	}
	dbRows.Close()
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("expected two distinct row ids; got %q / %q", firstID, secondID)
	}

	// Soft-delete the second row FIRST while item_id is still NULL
	// (the delete handler's orphan branch is happy with workspace
	// owner role and doesn't run the requireItemVisible check that
	// would otherwise 404 on a synthetic item_id).
	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+secondID, nil)
	if rr.Code != 204 {
		t.Fatalf("delete: %d", rr.Code)
	}

	// Now tag the FIRST (still-live) row with a synthetic item_id so
	// the never-attached-orphan path doesn't reclaim it under the
	// future grace cutoff. The test is about dedupe-aware blob
	// preservation, not the orphan-from-start case (covered by
	// TestOrphanGC_ReclaimsLongOrphans).
	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET item_id = ? WHERE id = ?`,
		"synthetic-item", firstID,
	); err != nil {
		t.Fatalf("attach first row: %v", err)
	}

	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("Deleted=%d, want >= 1", res.Deleted)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0 (other row still references the blob)",
			res.BlobsReclaimed)
	}

	// First row still works — blob still on disk.
	store, err := srv.attachments.Resolve(sharedKey)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := store.Stat(context.Background(), sharedKey); err != nil {
		t.Errorf("shared blob disappeared after GC: %v", err)
	}
}

// TestOrphanGC_KeepsReferencedNeverAttachedRows pins Codex P1 on
// PR #307 round 1: the editor's normal upload flow leaves
// attachments.item_id NULL and only the markdown reference inside
// item.content connects them. So a "never-attached" row past the
// grace period might still be referenced — the GC must scan item
// content before reclaiming.
func TestOrphanGC_KeepsReferencedNeverAttachedRows(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "kept.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, wsID)

	// Create an item whose content references the attachment, but
	// don't update attachments.item_id — exactly mirrors the
	// production editor flow (upload → PATCH content with the ref).
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items",
		map[string]any{"title": "Holds Image", "content": "ref: pad-attachment:" + id})
	if rr.Code != 201 {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}

	// Backdate the attachment's created_at past the 30-day grace
	// so the orphan SELECT picks it up.
	pastTs := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET created_at = ? WHERE id = ?`, pastTs, id,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	res, err := srv.runOrphanGCSweep(context.Background(),
		time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted=%d, want 0 (item content references the attachment)", res.Deleted)
	}
	if got, _ := srv.store.GetAttachment(id); got == nil {
		t.Errorf("referenced attachment was hard-deleted by GC; row gone")
	}
}

// TestOrphanGC_RespectsInFlightUploads pins Codex P2 on PR #307
// round 1: an upload that called Put but hasn't yet inserted the
// attachments row must NOT lose its blob to GC reclamation of an
// older soft-deleted row sharing the same hash.
//
// We simulate the race by registering an in-flight hash directly,
// running a sweep against an old soft-deleted row at that hash,
// and asserting the blob stayed.
func TestOrphanGC_RespectsInFlightUploads(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	if rr := doMultipartUpload(srv, slug, "victim.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, workspaceIDForSlug(t, srv, slug))

	att, _ := srv.store.GetAttachment(id)
	if att == nil {
		t.Fatal("expected attachment row")
	}

	// Soft-delete it.
	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil)
	if rr.Code != 204 {
		t.Fatalf("delete: %d", rr.Code)
	}

	// Pretend an upload is in flight for the same hash. (Production
	// upload code would have called this between Put and
	// CreateAttachment; the GC must see the in-flight signal.)
	release := srv.markUploadInFlight(att.ContentHash)
	defer release()

	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("Deleted=%d, want >= 1 (DB row should still go)", res.Deleted)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0 (in-flight upload protects the blob)",
			res.BlobsReclaimed)
	}
	// Blob still on disk so the in-flight upload can complete.
	store, err := srv.attachments.Resolve(att.StorageKey)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := store.Stat(context.Background(), att.StorageKey); err != nil {
		t.Errorf("blob disappeared despite in-flight signal: %v", err)
	}
}

// TestOrphanGC_RespectsSoftDeletedInGracePeer pins Codex P2 round
// 3: when two rows share a content_hash, GC reclaims the older one
// past grace but MUST NOT delete the blob if the second row is
// still inside its own grace window — the second row could be
// restored / inspected and would otherwise hit a missing blob.
func TestOrphanGC_RespectsSoftDeletedInGracePeer(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	body := realPNG()
	if rr := doMultipartUpload(srv, slug, "a.png", body); rr.Code != 201 {
		t.Fatalf("upload a: %d", rr.Code)
	}
	if rr := doMultipartUpload(srv, slug, "b.png", body); rr.Code != 201 {
		t.Fatalf("upload b: %d", rr.Code)
	}

	dbRows, err := srv.store.DB().Query(
		`SELECT id, storage_key, content_hash FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL ORDER BY created_at, id`, wsID)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	var firstID, secondID, sharedKey, sharedHash string
	for dbRows.Next() {
		var id, key, hash string
		if err := dbRows.Scan(&id, &key, &hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if firstID == "" {
			firstID = id
			sharedKey = key
			sharedHash = hash
		} else {
			secondID = id
		}
	}
	dbRows.Close()
	if firstID == "" || secondID == "" {
		t.Fatalf("expected two rows; got %q / %q", firstID, secondID)
	}

	// Soft-delete BOTH rows. Then backdate ONLY the first row's
	// deleted_at past the 30-day grace; the second stays "fresh".
	for _, id := range []string{firstID, secondID} {
		rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil)
		if rr.Code != 204 {
			t.Fatalf("delete %s: %d", id, rr.Code)
		}
	}
	pastTs := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET deleted_at = ? WHERE id = ?`, pastTs, firstID,
	); err != nil {
		t.Fatalf("backdate first: %v", err)
	}

	// Sweep with a 30-day cutoff. Only the first row qualifies.
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	res, err := srv.runOrphanGCSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted=%d, want 1 (only the older row qualifies)", res.Deleted)
	}
	if res.BlobsReclaimed != 0 {
		t.Errorf("BlobsReclaimed=%d, want 0 (newer soft-deleted peer is still in grace)",
			res.BlobsReclaimed)
	}
	// Blob still on disk so the still-in-grace row's hypothetical
	// undelete works.
	store, err := srv.attachments.Resolve(sharedKey)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := store.Stat(context.Background(), sharedKey); err != nil {
		t.Errorf("shared blob disappeared while peer still in grace: %v", err)
	}
	_ = sharedHash
}

// TestOrphanGC_DedupesBlobReclaimMetric pins Codex round 4: when
// multiple soft-deleted peers share a content_hash and all are
// past grace, the blob is deleted on the first peer and the
// remaining peers' Delete calls are idempotent no-ops. The earlier
// version still bumped BlobsReclaimed / BytesReclaimed for each
// no-op, inflating the sweep metrics.
func TestOrphanGC_DedupesBlobReclaimMetric(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	body := realPNG()
	if rr := doMultipartUpload(srv, slug, "a.png", body); rr.Code != 201 {
		t.Fatalf("upload a: %d", rr.Code)
	}
	if rr := doMultipartUpload(srv, slug, "b.png", body); rr.Code != 201 {
		t.Fatalf("upload b: %d", rr.Code)
	}

	// Soft-delete both rows + backdate both deleted_at past 30d so
	// they BOTH qualify for reclamation in the same sweep.
	dbRows, _ := srv.store.DB().Query(
		`SELECT id FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL`, wsID)
	var ids []string
	for dbRows.Next() {
		var id string
		dbRows.Scan(&id)
		ids = append(ids, id)
	}
	dbRows.Close()
	for _, id := range ids {
		rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+id, nil)
		if rr.Code != 204 {
			t.Fatalf("delete %s: %d", id, rr.Code)
		}
	}
	pastTs := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET deleted_at = ?`, pastTs,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	res, err := srv.runOrphanGCSweep(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted < 2 {
		t.Errorf("Deleted=%d, want >= 2 (both rows past grace)", res.Deleted)
	}
	if res.BlobsReclaimed != 1 {
		t.Errorf("BlobsReclaimed=%d, want 1 (single shared blob)", res.BlobsReclaimed)
	}
	if res.BytesReclaimed != int64(len(body)) {
		t.Errorf("BytesReclaimed=%d, want %d (single shared blob's size)",
			res.BytesReclaimed, len(body))
	}
}

// TestInFlightUploadHashes_ConcurrentReleaseReacquire pins Codex P1
// round 2 on PR #307: the prior sync.Map version raced when one
// release's decrement-to-zero ran in parallel with another upload's
// LoadOrStore-then-increment, leaving an in-flight upload's signal
// invisible to the GC. The mutex-protected map closes the window.
//
// Stress test: hammer a single hash with overlapping
// markUploadInFlight / release pairs. At every observation point,
// uploadInFlight must report > 0 whenever ANY goroutine is between
// its mark and its release.
func TestInFlightUploadHashes_ConcurrentReleaseReacquire(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	const goroutines = 20
	const iterations = 500
	hash := "race-test-hash"

	var wg sync.WaitGroup
	wg.Add(goroutines)
	// Spawn goroutines that each loop mark/release; all share the
	// same hash so the inc/dec interleaving is maximized.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				release := srv.markUploadInFlight(hash)
				// At least one goroutine (this one) is in flight.
				if !srv.uploadInFlight(hash) {
					t.Errorf("uploadInFlight=false while one goroutine holds the mark")
				}
				release()
			}
		}()
	}
	wg.Wait()

	// After everyone finishes, the counter should be exactly zero
	// and the map entry deleted.
	if srv.uploadInFlight(hash) {
		t.Errorf("uploadInFlight=true after all releases; counter leaked")
	}
}

// TestOrphanGC_StartStop pins the lifecycle: StartOrphanGC kicks the
// loop, Stop signals it to exit, and Server.Stop() actually drains.
// Catches regressions where a leaked goroutine would compound across
// every Stop cycle (BUG-851 echo).
func TestOrphanGC_StartStop(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	srv.SetOrphanGCConfig(1*time.Millisecond, 24*time.Hour)
	srv.StartOrphanGC()

	// Calling start a second time is a no-op.
	srv.StartOrphanGC()

	// Stop drains in the t.Cleanup hook from testServer; just give
	// the loop a tick to actually run a sweep.
	time.Sleep(10 * time.Millisecond)
	// If we got here without deadlocking on Stop, the loop drains
	// correctly. testServer's t.Cleanup will exercise Stop.
}
