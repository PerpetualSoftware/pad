package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
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

// TestOrphanGC_KeepsAttachmentReferencedFromComment is the IDEA-1650
// sibling of the item-content case above: a screenshot pasted into a
// comment is referenced ONLY from comments.body, with attachments.item_id
// left NULL. The GC's reference scan must cover comment bodies, otherwise
// a never-attached row past the grace period would be reclaimed and the
// comment's embed would break.
func TestOrphanGC_KeepsAttachmentReferencedFromComment(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "kept.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, wsID)

	// Create a plain item (no reference in its content), then post a
	// comment that references the attachment — mirrors the comment
	// composer flow (upload → post comment with the ref). item_id on
	// the attachment row stays NULL.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items",
		map[string]any{"title": "Has A Comment"})
	if rr.Code != 201 {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	itemSlug, _ := item["slug"].(string)
	if itemSlug == "" {
		t.Fatalf("item response missing slug: %s", rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemSlug+"/comments",
		map[string]any{"body": "see ![](pad-attachment:" + id + ")"})
	if rr.Code != 201 {
		t.Fatalf("create comment: %d %s", rr.Code, rr.Body.String())
	}

	// Backdate the attachment past the 30-day grace so the orphan
	// SELECT picks it up.
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
		t.Errorf("Deleted=%d, want 0 (a comment references the attachment)", res.Deleted)
	}
	if got, _ := srv.store.GetAttachment(id); got == nil {
		t.Errorf("comment-referenced attachment was hard-deleted by GC; row gone")
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

// TestOrphanGC_ClaimRefusesFreshlyReferencedRow pins BUG-2415 at the
// sweep level: a never-attached row whose last_referenced_at stamp is
// FRESH (a writer referenced it after the sweep's content scan would
// have run) survives the sweep — row AND blob — because the row
// deletion is a conditional claim and the blob is only reclaimed after
// a successful claim. The stamped row deliberately has NO content
// reference, so the LIKE scan passes it and the claim predicate is the
// only thing standing between it and deletion — exactly the filed
// mid-sweep window.
func TestOrphanGC_ClaimRefusesFreshlyReferencedRow(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	body := realPNG()
	rr := doMultipartUpload(srv, slug, "raced.png", body)
	if rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	id := getOnlyAttachmentID(t, srv, wsID)

	// Fresh stamp, no content reference — the post-scan writer's trace.
	nowTS := time.Now().UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE attachments SET last_referenced_at = ? WHERE id = ?`), nowTS, id); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	att, err := srv.store.GetAttachment(id)
	if err != nil || att == nil {
		t.Fatalf("GetAttachment: %v %v", att, err)
	}
	store, err := srv.attachments.Resolve(att.StorageKey)
	if err != nil {
		t.Fatalf("resolve backend: %v", err)
	}

	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted=%d, want 0 — fresh-stamped row was reclaimed", res.Deleted)
	}

	// Row survives.
	att2, err := srv.store.GetAttachment(id)
	if err != nil {
		t.Fatalf("post-sweep GetAttachment: %v", err)
	}
	if att2 == nil {
		t.Fatalf("fresh-stamped row deleted by sweep — the filed data-loss race")
	}
	// Blob survives (row-before-bytes: an unclaimed row's bytes are
	// never touched).
	if _, err := store.Stat(context.Background(), att.StorageKey); err != nil {
		t.Errorf("blob missing after refused claim: %v", err)
	}

	// COUNTERFACTUAL ARM: age the stamp past the stale window and sweep
	// again — the same row must now be reclaimed, proving the stamp
	// condition (not the scan, not the grace cutoff) is what protected
	// it above.
	staleTS := time.Now().Add(-2 * orphanGCRefStaleWindow).UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE attachments SET last_referenced_at = ? WHERE id = ?`), staleTS, id); err != nil {
		t.Fatalf("age stamp: %v", err)
	}
	res, err = srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("second sweep Deleted=%d, want >= 1 — stale stamp should not protect", res.Deleted)
	}
	att3, err := srv.store.GetAttachment(id)
	if err != nil {
		t.Fatalf("final GetAttachment: %v", err)
	}
	if att3 != nil {
		t.Errorf("stale-stamped row survived the second sweep")
	}
}

// TestOrphanGC_VariantProtectedByReferencedParent pins the parent-aware
// half of BUG-2415: content references an ORIGINAL's id, never a
// thumbnail's, so a referenced never-attached upload must keep its
// variants too — the scan consults the parent id for variant rows, and
// the claim refuses on a fresh PARENT stamp.
func TestOrphanGC_VariantProtectedByReferencedParent(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	rr := doMultipartUpload(srv, slug, "with-thumb.png", realPNG())
	if rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	origID := getOnlyAttachmentID(t, srv, wsID)

	// Seed a variant row hanging off the original. Direct insert keeps
	// the test independent of the thumbnail pipeline's async behavior.
	thumbID := origID + "-thumb"
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, parent_id, variant, created_at)
		 VALUES (?, ?, NULL, '', ?, ?, 'image/png', 10, 'thumb.png', ?, 'thumb-md', ?)`),
		thumbID, wsID, "fs:thumb-"+thumbID, "hash-thumb-"+thumbID, origID,
		time.Now().Add(-40*24*time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert variant: %v", err)
	}

	// Reference the ORIGINAL from item content — the only id content
	// ever carries.
	var collID string
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT id FROM collections WHERE workspace_id = ? ORDER BY sort_order, slug LIMIT 1`), wsID).Scan(&collID); err != nil {
		t.Fatalf("pick collection: %v", err)
	}
	if _, err := srv.store.CreateItem(wsID, collID, models.ItemCreate{
		Title:   "holder",
		Content: "see ![x](pad-attachment:" + origID + ")",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_ = res

	if att, _ := srv.store.GetAttachment(origID); att == nil {
		t.Fatalf("referenced original reclaimed")
	}
	if att, _ := srv.store.GetAttachment(thumbID); att == nil {
		t.Errorf("referenced original's VARIANT reclaimed — parent-aware scan failed")
	}
}

// --- Rowless-blob sweep (BUG-2406) -------------------------------------

// putRowlessBlob writes bytes straight into the fs backend with no
// attachments row — the artifact of a Put-then-insert failure (or a
// crash between the two), which the row-driven sweep cannot see.
func putRowlessBlob(t *testing.T, srv *Server, content []byte) (hash, key string) {
	t.Helper()
	backend, ok := srv.attachments.Backends()[attachments.FSPrefix]
	if !ok {
		t.Fatal("no fs backend registered")
	}
	hash = sha256Hex(content)
	key, err := backend.Put(context.Background(), hash, "application/octet-stream", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("direct Put: %v", err)
	}
	return hash, key
}

func statBlob(t *testing.T, srv *Server, key string) error {
	t.Helper()
	backend, err := srv.attachments.Resolve(key)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, statErr := backend.Stat(context.Background(), key)
	return statErr
}

// TestRowlessBlobSweep_ReclaimsAgedRowlessBlob pins the core: a blob no
// row references, older than the grace cutoff, is deleted — bytes,
// counters, and all. This is the leak the row-driven sweep can never
// reclaim, verified by the companion counterfactual below.
func TestRowlessBlobSweep_ReclaimsAgedRowlessBlob(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	content := []byte("leaked-by-failed-insert")
	_, key := putRowlessBlob(t, srv, content)

	// Counterfactual: the ROW sweep, given the same permissive cutoff,
	// walks zero rows and leaves the blob exactly where it is — proving
	// the class needs its own sweep rather than being covered already.
	rowRes, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("row sweep: %v", err)
	}
	if rowRes.BlobsReclaimed != 0 {
		t.Fatalf("row sweep reclaimed %d blobs; rowless blob should be invisible to it", rowRes.BlobsReclaimed)
	}
	if err := statBlob(t, srv, key); err != nil {
		t.Fatalf("blob missing after row sweep already: %v", err)
	}

	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep: %v", err)
	}
	if res.Listed < 1 {
		t.Errorf("Listed=%d, want >= 1", res.Listed)
	}
	if res.Reclaimed != 1 {
		t.Errorf("Reclaimed=%d, want 1", res.Reclaimed)
	}
	if res.BytesReclaimed != int64(len(content)) {
		t.Errorf("BytesReclaimed=%d, want %d", res.BytesReclaimed, len(content))
	}
	if err := statBlob(t, srv, key); !errors.Is(err, attachments.ErrNotFound) {
		t.Errorf("blob still present after rowless sweep; Stat err=%v", err)
	}
}

// TestRowlessBlobSweep_KeepsYoungBlob pins the age gate: a rowless blob
// younger than the cutoff is the normal transient state of an upload
// whose row insert hasn't happened yet, and must survive.
func TestRowlessBlobSweep_KeepsYoungBlob(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	_, key := putRowlessBlob(t, srv, []byte("mid-upload"))

	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep: %v", err)
	}
	if res.Reclaimed != 0 {
		t.Errorf("Reclaimed=%d, want 0 (blob younger than cutoff)", res.Reclaimed)
	}
	if err := statBlob(t, srv, key); err != nil {
		t.Errorf("young rowless blob was deleted: %v", err)
	}
}

// TestRowlessBlobSweep_KeepsRowBackedBlobs pins the subtraction: a blob
// with a LIVE row and a blob whose only row is SOFT-DELETED both
// survive — any row in any state owns its bytes via the row sweep's
// claim protocol, and the rowless sweep must not reach around it.
func TestRowlessBlobSweep_KeepsRowBackedBlobs(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	// Live row via the real upload endpoint.
	if rr := doMultipartUpload(srv, slug, "live.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	liveKey := "fs:" + sha256Hex(realPNG())

	// Soft-deleted-only row, planted directly.
	content := []byte("soft-deleted-owner")
	hash, sdKey := putRowlessBlob(t, srv, content)
	att := &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "someone",
		StorageKey:  sdKey,
		ContentHash: hash,
		MimeType:    "application/octet-stream",
		SizeBytes:   int64(len(content)),
		Filename:    "soon-deleted.bin",
	}
	if err := srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	if err := srv.store.SoftDeleteAttachment(att.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}

	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep: %v", err)
	}
	if res.Reclaimed != 0 {
		t.Errorf("Reclaimed=%d, want 0 (both blobs are row-backed)", res.Reclaimed)
	}
	for _, key := range []string{liveKey, sdKey} {
		if err := statBlob(t, srv, key); err != nil {
			t.Errorf("row-backed blob %s deleted by rowless sweep: %v", key, err)
		}
	}
}

// TestRowlessBlobSweep_RespectsInFlightUploads pins the in-flight fence,
// with the counterfactual leg: the same blob IS reclaimed once the
// registration releases, so the fence — not something else — is what
// held it.
func TestRowlessBlobSweep_RespectsInFlightUploads(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	hash, key := putRowlessBlob(t, srv, []byte("in-flight-window"))

	release := srv.markUploadInFlight(hash)
	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep (in flight): %v", err)
	}
	if res.Reclaimed != 0 {
		t.Errorf("Reclaimed=%d, want 0 while upload in flight", res.Reclaimed)
	}
	if err := statBlob(t, srv, key); err != nil {
		t.Fatalf("in-flight blob deleted: %v", err)
	}

	release()
	res, err = srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep (released): %v", err)
	}
	if res.Reclaimed != 1 {
		t.Errorf("Reclaimed=%d after release, want 1", res.Reclaimed)
	}
	if err := statBlob(t, srv, key); !errors.Is(err, attachments.ErrNotFound) {
		t.Errorf("blob survived after release; Stat err=%v", err)
	}
}

// TestRowlessBlobSweep_DeleteTimeRowRecheck pins the TOCTOU guard: a row
// committed AFTER the batched hash subtraction but BEFORE the delete —
// the fully-completed writer whose in-flight registration has already
// released — must still protect its bytes. The pre-delete hook commits
// the row at exactly that point.
func TestRowlessBlobSweep_DeleteTimeRowRecheck(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)
	content := []byte("late-row-writer")
	hash, key := putRowlessBlob(t, srv, content)

	hookRan := false
	srv.rowlessPreDeleteHook = func(h string) {
		if h != hash {
			return
		}
		hookRan = true
		att := &models.Attachment{
			WorkspaceID: wsID,
			UploadedBy:  "late-writer",
			StorageKey:  key,
			ContentHash: hash,
			MimeType:    "application/octet-stream",
			SizeBytes:   int64(len(content)),
			Filename:    "late.bin",
		}
		if err := srv.store.CreateAttachment(att); err != nil {
			t.Errorf("hook CreateAttachment: %v", err)
		}
	}
	defer func() { srv.rowlessPreDeleteHook = nil }()

	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep: %v", err)
	}
	if !hookRan {
		t.Fatal("pre-delete hook never ran — the TOCTOU leg was not exercised")
	}
	if res.Reclaimed != 0 {
		t.Errorf("Reclaimed=%d, want 0 (row committed before delete)", res.Reclaimed)
	}
	if err := statBlob(t, srv, key); err != nil {
		t.Errorf("blob deleted despite delete-time row: %v", err)
	}
}
