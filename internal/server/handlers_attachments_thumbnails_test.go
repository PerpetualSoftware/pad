//go:build !libvips

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// variantRow looks up a derived variant of parentID. GetAttachmentVariant is
// workspace-scoped (PLAN-2391 DR-16), so the scope is read back off the
// parent row rather than threaded through every call site — these tests care
// about derivation behaviour, not about the scope itself, which
// TestDownload_ForeignVariantIsNotServed pins directly.
func variantRow(t *testing.T, srv *Server, parentID, variant string) (*models.Attachment, error) {
	t.Helper()
	parent, err := srv.store.GetAttachment(parentID)
	if err != nil {
		t.Fatalf("GetAttachment(%s): %v", parentID, err)
	}
	if parent == nil {
		t.Fatalf("GetAttachment(%s): parent row missing", parentID)
	}
	return srv.store.GetAttachmentVariant(parent.WorkspaceID, parentID, variant)
}

// makeIntegrationPNG returns a real PNG large enough that both
// thumb-sm (256px) and thumb-md (1024px) variants must scale down.
// Filled with a stripe pattern so the encoder doesn't reduce it to a
// trivially-compressible single colour.
func makeIntegrationPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x ^ y) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode integration PNG: %v", err)
	}
	return buf.Bytes()
}

func makeIntegrationJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode integration JPEG: %v", err)
	}
	return buf.Bytes()
}

// TestThumbnails_GeneratedOnPNGUpload uploads a 2000x1500 PNG and
// asserts that both thumb-sm and thumb-md variant rows exist after
// the upload's async derivation completes. Server.Stop() drains
// goAsync goroutines, so calling it deterministically waits for the
// thumbnail pipeline before assertions run.
func TestThumbnails_GeneratedOnPNGUpload(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := makeIntegrationPNG(t, 2000, 1500)

	rr := doMultipartUpload(srv, slug, "screenshot.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload resp: %v", err)
	}

	// Drain the async thumbnail goroutine.
	srv.Stop()

	for _, variant := range []string{models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd} {
		row, err := variantRow(t, srv, resp.ID, variant)
		if err != nil {
			t.Fatalf("GetAttachmentVariant(%s): %v", variant, err)
		}
		if row == nil {
			t.Errorf("variant %q row missing", variant)
			continue
		}
		if row.ParentID == nil || *row.ParentID != resp.ID {
			t.Errorf("variant %q ParentID = %v, want %s", variant, row.ParentID, resp.ID)
		}
		if row.Variant == nil || *row.Variant != variant {
			t.Errorf("variant %q Variant = %v, want %s", variant, row.Variant, variant)
		}
		// PNG → PNG (preserves alpha)
		if row.MimeType != "image/png" {
			t.Errorf("variant %q MimeType = %q, want image/png", variant, row.MimeType)
		}
		// Variant dimensions must be ≤ MaxLong on both axes.
		var maxLong int
		if variant == models.AttachmentVariantThumbSm {
			maxLong = 256
		} else {
			maxLong = 1024
		}
		if row.Width == nil || row.Height == nil {
			t.Errorf("variant %q dimensions missing", variant)
			continue
		}
		if *row.Width > maxLong || *row.Height > maxLong {
			t.Errorf("variant %q dims = %dx%d, both must be ≤ %d", variant, *row.Width, *row.Height, maxLong)
		}
		// We deliberately do NOT assert the variant is smaller than the
		// parent in bytes — synthetic test patterns can compress
		// pathologically well at 2000x1500 (run-length-friendly scan
		// rows) and then decompress to high-entropy noise after Lanczos
		// resize. Real-world content (photos, screenshots) is the
		// opposite, but a unit test that depends on PNG compression
		// ratios isn't measuring correctness. Dimensions ARE the
		// correctness signal — they prove the resize ran.
	}
}

// TestThumbnails_GeneratedOnJPEGUpload — thumb output should be JPEG
// when the source is JPEG (covers the ThumbnailFormat policy).
func TestThumbnails_GeneratedOnJPEGUpload(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := makeIntegrationJPEG(t, 1500, 1200)

	rr := doMultipartUpload(srv, slug, "photo.jpg", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rr.Code)
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	srv.Stop()
	row, err := variantRow(t, srv, resp.ID, models.AttachmentVariantThumbSm)
	if err != nil || row == nil {
		t.Fatalf("thumb-sm missing: row=%v err=%v", row, err)
	}
	if row.MimeType != "image/jpeg" {
		t.Errorf("MimeType = %q, want image/jpeg", row.MimeType)
	}
}

// TestThumbnails_SkippedForSmallSourceImage — when the parent's
// dimensions are already within a variant's bound, we don't emit a
// derived row (the download handler already falls back to original).
func TestThumbnails_SkippedForSmallSourceImage(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := makeIntegrationPNG(t, 200, 150) // < both 256 and 1024 → both variants skipped

	rr := doMultipartUpload(srv, slug, "small.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rr.Code)
	}
	var resp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	srv.Stop()
	for _, variant := range []string{models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd} {
		if row, _ := variantRow(t, srv, resp.ID, variant); row != nil {
			t.Errorf("variant %q should be skipped for small source, got row %s", variant, row.ID)
		}
	}
}

// TestThumbnails_ServeViaVariantQueryParam — the existing GET handler's
// ?variant= path should now find the derived row (instead of falling
// back to the original) once thumbnails exist.
func TestThumbnails_ServeViaVariantQueryParam(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := makeIntegrationPNG(t, 2000, 1500)

	rr := doMultipartUpload(srv, slug, "shot.png", body)
	var up struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &up)
	srv.Stop()

	// Fetch the thumb-md variant. Body must be a valid PNG smaller than
	// the original.
	req := httptest.NewRequest("GET", up.URL+"?variant=thumb-md", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("variant fetch status = %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("variant Content-Type = %q, want image/png", w.Header().Get("Content-Type"))
	}
	// Decoded variant dimensions must be within the 1024px ceiling.
	thumb, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode served thumb: %v", err)
	}
	if thumb.Bounds().Dx() > 1024 || thumb.Bounds().Dy() > 1024 {
		t.Errorf("thumb dims %v exceed 1024px", thumb.Bounds())
	}
}

// TestThumbnails_CountsTowardWorkspaceUsage — derived blobs are real
// bytes on disk and DOC-865 is explicit that they count against the
// quota. Verify the WorkspaceStorageUsage accumulator picks up both
// variants in addition to the original.
func TestThumbnails_CountsTowardWorkspaceUsage(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := makeIntegrationPNG(t, 2000, 1500)

	// Resolve workspace ID for the storage-usage probe.
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}

	beforeUsage, _ := srv.store.WorkspaceStorageUsage(ws.ID)
	rr := doMultipartUpload(srv, slug, "shot.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rr.Code)
	}
	srv.Stop()

	afterUsage, _ := srv.store.WorkspaceStorageUsage(ws.ID)
	delta := afterUsage - beforeUsage

	// The delta must include the original blob plus both variants.
	// We compare against `len(body)` (the original) — anything beyond
	// that is the variants. Equality with len(body) means thumbnails
	// were not generated; we want strictly greater.
	if delta <= int64(len(body)) {
		t.Errorf("usage delta = %d, want > %d (original + variants)", delta, len(body))
	}
}

// TestServerCapabilities_Endpoint — the new /api/v1/server/capabilities
// endpoint reports what the editor needs for rotate/crop UI gating.
func TestServerCapabilities_Endpoint(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()

	req := httptest.NewRequest("GET", "/api/v1/server/capabilities", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		Image attachments.Capabilities `json:"image"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantFormats := []string{"png", "jpeg", "gif", "bmp", "tiff"}
	if got, want := strings.Join(resp.Image.ImageFormats, ","), strings.Join(wantFormats, ","); got != want {
		t.Errorf("ImageFormats = %q, want %q", got, want)
	}
	if !resp.Image.CanTranscode {
		t.Error("CanTranscode = false on pure-Go default build")
	}
	if resp.Image.MaxPixels != attachments.MaxPixelsDefault {
		t.Errorf("MaxPixels = %d, want %d", resp.Image.MaxPixels, attachments.MaxPixelsDefault)
	}
}

// TestServerCapabilities_DegradedWhenProcessorMissing — if no
// processor is wired, the endpoint reports an empty image-formats
// list rather than 500-ing. The editor reads this as "disable
// rotate/crop UI" without failing the editor mount.
func TestServerCapabilities_DegradedWhenProcessorMissing(t *testing.T) {
	srv := testServer(t)
	// Intentionally NOT wiring SetImageProcessor.

	req := httptest.NewRequest("GET", "/api/v1/server/capabilities", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Image attachments.Capabilities `json:"image"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Image.ImageFormats) != 0 {
		t.Errorf("ImageFormats = %v, want [] (degraded)", resp.Image.ImageFormats)
	}
	if resp.Image.CanTranscode {
		t.Error("CanTranscode = true with no processor wired")
	}
}

// TestServerCapabilities_PublicAfterBootstrap — once any user exists
// the RequireAuth middleware kicks in for non-public paths, so the
// capabilities endpoint must be on the isPublicAPIPath allowlist.
// Without that, the editor's pre-login fetch (e.g. on the share
// preview surface) gets 401'd. Regression guard for the exact issue
// Codex flagged in the round-1 review of TASK-878.
func TestServerCapabilities_PublicAfterBootstrap(t *testing.T) {
	srv := testServer(t)
	// Bootstrap an admin so the RequireAuth gate is active for
	// non-public paths. The test client below sends NO auth cookie,
	// so any path NOT on the public allowlist would return 401.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	req := httptest.NewRequest("GET", "/api/v1/server/capabilities", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated capabilities fetch returned %d (want 200); body = %s",
			w.Code, w.Body.String())
	}
}

// TestThumbnails_PersistRefusedWhenParentDeleted pins BUG-2388's race
// deterministically: persistThumbnail is called with a parent SNAPSHOT
// taken before a delete (exactly what the async derivation holds after
// its early liveness check), the delete cascade lands, and the
// conditional variant INSERT must refuse — no live variant row under a
// tombstoned parent, and the just-Put blob is cleaned up under the
// in-flight fence.
func TestThumbnails_PersistRefusedWhenParentDeleted(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	if srv.imageProcessor == nil {
		t.Skip("no image processor in this build")
	}

	rr := doMultipartUpload(srv, slug, "tiny.png", realPNG())
	if rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	id := getOnlyAttachmentID(t, srv, wsID)
	parent, err := srv.store.GetAttachment(id)
	if err != nil || parent == nil {
		t.Fatalf("GetAttachment: %v %v", parent, err)
	}

	// The delete cascade lands AFTER the derivation captured its parent
	// snapshot (the filed interleaving).
	if err := srv.store.SoftDeleteAttachment(parent.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if err := srv.persistThumbnail(context.Background(), parent, img, "thumb-md", "png"); err != nil {
		t.Fatalf("persistThumbnail should skip, not error: %v", err)
	}

	// No live variant row was minted.
	var n int
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT COUNT(*) FROM attachments WHERE parent_id = ? AND deleted_at IS NULL`), parent.ID).Scan(&n); err != nil {
		t.Fatalf("count variants: %v", err)
	}
	if n != 0 {
		t.Fatalf("race minted %d live variant row(s) under a tombstoned parent", n)
	}
	// And the refused thumbnail's just-Put blob was cleaned up (codex
	// round 1: without this assertion the cleanup branch was untested).
	// The thumb bytes are a deterministic function of the 8x8 RGBA +
	// encoder, so recompute the storage key the same way persistThumbnail
	// did and Stat it.
	{
		var buf bytes.Buffer
		if err := srv.imageProcessor.Encode(img, "png", &buf); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		thumbHash := sha256Hex(buf.Bytes())
		bstore, err := srv.attachments.Resolve(attachments.FSPrefix + ":" + thumbHash)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if _, err := bstore.Stat(context.Background(), attachments.FSPrefix+":"+thumbHash); err == nil {
			t.Errorf("refused thumbnail's blob still on disk — cleanup branch did not run")
		}
	}

	// CONTROL: the same call against a LIVE parent inserts.
	rr = doMultipartUpload(srv, slug, "tiny2.png", realPNG())
	if rr.Code != 201 {
		t.Fatalf("upload 2: %d %s", rr.Code, rr.Body.String())
	}
	var liveID string
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT id FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL AND parent_id IS NULL ORDER BY created_at DESC LIMIT 1`), wsID).Scan(&liveID); err != nil {
		t.Fatalf("find live upload: %v", err)
	}
	liveParent, err := srv.store.GetAttachment(liveID)
	if err != nil || liveParent == nil {
		t.Fatalf("GetAttachment live: %v %v", liveParent, err)
	}
	if err := srv.persistThumbnail(context.Background(), liveParent, img, "thumb-md", "png"); err != nil {
		t.Fatalf("persistThumbnail live: %v", err)
	}
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT COUNT(*) FROM attachments WHERE parent_id = ? AND deleted_at IS NULL`), liveParent.ID).Scan(&n); err != nil {
		t.Fatalf("count live variants: %v", err)
	}
	if n != 1 {
		t.Fatalf("control: expected 1 variant under live parent, got %d", n)
	}
}

// TestOrphanGC_ReclaimsLeakedLiveVariant pins the retro-cleanup class:
// a LIVE variant row under a tombstoned parent (the pre-fix leak
// artifact — inserted directly, as the old code path would have left
// it) is claimed by the sweep; and the claim refuses when the parent
// is restored first.
func TestOrphanGC_ReclaimsLeakedLiveVariant(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	rr := doMultipartUpload(srv, slug, "leaked.png", realPNG())
	if rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	parentID := getOnlyAttachmentID(t, srv, wsID)

	// Leak artifact: live variant row inserted the OLD unconditional way,
	// then the parent tombstoned without the (now-atomic) cascade seeing
	// it. item_id is SET (an attached upload's thumbnail — the common
	// case): that keeps the row out of BOTH pre-existing GC classes, so
	// only the new orphaned-variant class can reclaim it — which is what
	// makes this test discriminate against the old sweep.
	var anyItemID string
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT id FROM items WHERE workspace_id = ? LIMIT 1`), wsID).Scan(&anyItemID); err != nil {
		// No seeded items in this workspace — create one directly.
		anyItemID = ""
	}
	leakID := parentID + "-leak"
	if anyItemID == "" {
		var collID string
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
			`SELECT id FROM collections WHERE workspace_id = ? ORDER BY sort_order, slug LIMIT 1`), wsID).Scan(&collID); err != nil {
			t.Fatalf("pick collection: %v", err)
		}
		item, err := srv.store.CreateItem(wsID, collID, models.ItemCreate{Title: "leak host"})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		anyItemID = item.ID
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, parent_id, variant, created_at)
		 VALUES (?, ?, ?, '', ?, ?, 'image/png', 10, 'leak.png', ?, 'thumb-md', ?)`),
		leakID, wsID, anyItemID, "fs:leak-"+leakID, "hash-leak-"+leakID, parentID,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert leak: %v", err)
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE attachments SET deleted_at = ? WHERE id = ?`),
		time.Now().UTC().Format(time.RFC3339), parentID); err != nil {
		t.Fatalf("tombstone parent only: %v", err)
	}

	// RESTORE-WINS leg first: un-tombstone the parent, sweep — the
	// leaked row must survive (claim predicate re-asserts at delete
	// time; a restored original keeps its thumbnails).
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE attachments SET deleted_at = NULL WHERE id = ?`), parentID); err != nil {
		t.Fatalf("restore parent: %v", err)
	}
	if _, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if att, _ := srv.store.GetAttachment(leakID); att == nil {
		t.Fatalf("variant of RESTORED parent reclaimed")
	}

	// Now genuinely orphan it and sweep again — reclaimed.
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE attachments SET deleted_at = ? WHERE id = ?`),
		time.Now().Add(-40*24*time.Hour).UTC().Format(time.RFC3339), parentID); err != nil {
		t.Fatalf("re-tombstone parent: %v", err)
	}
	res, err := srv.runOrphanGCSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if res.Deleted < 1 {
		t.Errorf("sweep 2 Deleted=%d, want >= 1", res.Deleted)
	}
	if att, _ := srv.store.GetAttachment(leakID); att != nil {
		t.Errorf("leaked live variant survived the sweep")
	}
}
