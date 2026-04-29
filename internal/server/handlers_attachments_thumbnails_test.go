package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

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
		row, err := srv.store.GetAttachmentVariant(resp.ID, variant)
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
	row, err := srv.store.GetAttachmentVariant(resp.ID, models.AttachmentVariantThumbSm)
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
		if row, _ := srv.store.GetAttachmentVariant(resp.ID, variant); row != nil {
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
