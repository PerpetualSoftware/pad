package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// isoBMFFFixture reads one of internal/attachments/testdata's real encoder
// outputs. The fixtures live in that package because that is where the sniffer
// they exercise lives; this file reads across rather than keeping a second copy,
// on the same reasoning as TestMIMEFamiliesCoverAllowlist reading the web-side
// fixture — one copy cannot drift from itself. See that directory's README for
// how each was produced and what it does not cover.
func isoBMFFFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "attachments", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestUpload_AcceptsRealISOBMFFStillImages is the binding half of BUG-2961: the
// package-level tests vouch for the sniffer, and this vouches for the door
// actually consulting it. Before the fix these three uploads answered 415
// mime_not_allowed with "application/octet-stream" — through this exact handler,
// which is the one the web UI and the mobile share sheet both post to.
func TestUpload_AcceptsRealISOBMFFStillImages(t *testing.T) {
	cases := []struct {
		fixture  string
		filename string
		wantMIME string
	}{
		{"apple-still-heic.head512", "IMG_0001.heic", "image/heic"},
		{"apple-still.avif", "IMG_0001.avif", "image/avif"},
		{"still.avif", "libheif.avif", "image/avif"},
		{"still-mif1.heif", "libheif.heif", "image/heif"},
	}
	for _, tc := range cases {
		srv, slug := testServerWithAttachments(t)
		body := isoBMFFFixture(t, tc.fixture)
		rr := doMultipartUpload(srv, slug, tc.filename, body)
		if rr.Code != http.StatusCreated {
			t.Errorf("upload %s: status = %d, want 201; body = %s", tc.fixture, rr.Code, rr.Body.String())
			continue
		}
		var resp struct {
			MIME       string `json:"mime"`
			Category   string `json:"category"`
			RenderMode string `json:"render_mode"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Errorf("upload %s: decode response: %v", tc.fixture, err)
			continue
		}
		if resp.MIME != tc.wantMIME {
			t.Errorf("upload %s: mime = %q, want %q", tc.fixture, resp.MIME, tc.wantMIME)
		}
		if resp.Category != "image" || resp.RenderMode != "inline" {
			t.Errorf("upload %s: category/render_mode = %q/%q, want image/inline", tc.fixture, resp.Category, resp.RenderMode)
		}
	}
}

// TestUpload_ISOBMFFStillRefusesNonImageBytes is the row the reporter measured
// and asked to keep: the fix must not have quietly become "trust the filename".
// A file named .heic whose bytes are an executable is still refused, and a real
// HEIC named .jpg is still stored as what its BYTES say.
func TestUpload_ISOBMFFStillRefusesNonImageBytes(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	exe := []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff")
	if rr := doMultipartUpload(srv, slug, "totally-a-photo.heic", exe); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("exe named .heic: status = %d, want 415; body = %s", rr.Code, rr.Body.String())
	}

	rr := doMultipartUpload(srv, slug, "IMG_0001.jpg", isoBMFFFixture(t, "apple-still-heic.head512"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("HEIC named .jpg: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MIME string `json:"mime"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.MIME != "image/heic" {
		t.Errorf("HEIC named .jpg: mime = %q, want image/heic (the bytes decide, not the name)", resp.MIME)
	}
}
