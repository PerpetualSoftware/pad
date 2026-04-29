package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// uploadHelper does a fresh upload and returns the parsed response.
func uploadHelper(t *testing.T, srv *Server, slug string, filename string, body []byte) (id, mime, urlPath string) {
	t.Helper()
	rr := doMultipartUpload(srv, slug, filename, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload(%s): status=%d body=%s", filename, rr.Code, rr.Body.String())
	}
	var resp struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		MIME string `json:"mime"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload resp: %v", err)
	}
	return resp.ID, resp.MIME, resp.URL
}

func TestDownload_HappyPathPNG(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	id, mime, urlPath := uploadHelper(t, srv, slug, "screen.png", body)

	req := httptest.NewRequest("GET", urlPath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != mime {
		t.Errorf("Content-Type = %q, want %q", got, mime)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline; ...", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=3600" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Body.Bytes(); len(got) != len(body) {
		t.Errorf("body len = %d, want %d", len(got), len(body))
	}
	// Sanity: the served bytes match the uploaded bytes.
	if !equalBytes(rr.Body.Bytes(), body) {
		t.Errorf("served bytes do not match uploaded bytes (id=%s)", id)
	}
}

func TestDownload_HTMLForcedAsAttachment(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	html := []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	_, _, urlPath := uploadHelper(t, srv, slug, "page.html", html)

	req := httptest.NewRequest("GET", urlPath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("Content-Disposition = %q, want attachment; ... (HTML must force download)", got)
	}
}

func TestDownload_NotFound(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/attachments/00000000-0000-0000-0000-000000000000", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDownload_HEADReturnsHeadersWithoutBody(t *testing.T) {
	// The editor's file-chip NodeView (TASK-877) probes attachment metadata
	// via a HEAD request — Content-Type powers the MIME-aware icon and
	// Content-Length feeds the size readout. chi doesn't auto-route HEAD
	// to the GET handler, so the registration is explicit; this test
	// guards that registration plus the seekable-path HEAD behavior of
	// http.ServeContent (headers set, body omitted).
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	_, mime, urlPath := uploadHelper(t, srv, slug, "screen.png", body)

	req := httptest.NewRequest("HEAD", urlPath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != mime {
		t.Errorf("HEAD Content-Type = %q, want %q", got, mime)
	}
	// Content-Length must reflect the actual blob size so the chip can
	// render the readout. http.ServeContent computes this from the
	// underlying ReadSeeker.
	if got := rr.Header().Get("Content-Length"); got == "" {
		t.Errorf("HEAD missing Content-Length header")
	} else if n, err := strconv.Atoi(got); err != nil || n != len(body) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(body))
	}
	// Body MUST be empty on HEAD — Go's responseWriter discards body
	// writes, but the assertion catches a regression where someone
	// changes the handler in a way that bypasses ServeContent.
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD response body should be empty, got %d bytes", rr.Body.Len())
	}
}

func TestDownload_HEADCrossWorkspaceReturns404(t *testing.T) {
	// Cross-workspace probing must return 404, not 403, on HEAD too —
	// otherwise the metadata HEAD becomes a side-channel that lets a
	// member of workspace B enumerate attachment IDs in workspace A.
	srv, slugA := testServerWithAttachments(t)
	slugB := createWSForTest(t, srv)
	id, _, _ := uploadHelper(t, srv, slugA, "secret.png", realPNG())

	req := httptest.NewRequest("HEAD", "/api/v1/workspaces/"+slugB+"/attachments/"+id, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("HEAD cross-workspace status = %d, want 404 (NOT 403)", rr.Code)
	}
}

func TestDownload_CrossWorkspaceReturns404(t *testing.T) {
	srv, slugA := testServerWithAttachments(t)
	slugB := createWSForTest(t, srv)

	// Upload into workspace A.
	id, _, _ := uploadHelper(t, srv, slugA, "secret.png", realPNG())

	// Try to fetch from workspace B with A's attachment ID.
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slugB+"/attachments/"+id, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	// MUST be 404, not 403 — leaking 403 vs 404 lets a member of B
	// enumerate attachment IDs in A.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace status = %d, want 404 (NOT 403)", rr.Code)
	}
}

func TestDownload_Range_206(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	// Build a payload long enough that a Range request makes sense.
	// Use a real MP4 magic header so the upload passes MIME sniff, then
	// pad with zero bytes.
	mp4 := []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2',
		0x00, 0x00, 0x00, 0x00, 'm', 'p', '4', '2', 'i', 's', 'o', 'm',
	}
	payload := append(mp4, make([]byte, 4096)...)
	_, _, urlPath := uploadHelper(t, srv, slug, "clip.mp4", payload)

	req := httptest.NewRequest("GET", urlPath, nil)
	req.Header.Set("Range", "bytes=10-29")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rr.Code)
	}
	if got := rr.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rr.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 10-29/") {
		t.Errorf("Content-Range = %q, want bytes 10-29/...", got)
	}
	if got := rr.Body.Len(); got != 20 {
		t.Errorf("body len = %d, want 20", got)
	}
	if got, want := rr.Body.Bytes(), payload[10:30]; !equalBytes(got, want) {
		t.Errorf("range bytes mismatch")
	}

	// Content-Length = 20 on a successful Range response.
	if cl, _ := strconv.Atoi(rr.Header().Get("Content-Length")); cl != 20 {
		t.Errorf("Content-Length = %v, want 20", cl)
	}
}

func TestDownload_VariantFallbackToOriginal(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	_, _, urlPath := uploadHelper(t, srv, slug, "tiny.png", body)

	// Ask for thumb-sm — TASK-878 hasn't generated thumbs yet, so the
	// handler must silently serve the original.
	req := httptest.NewRequest("GET", urlPath+"?variant=thumb-sm", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (variant should fall back to original)", rr.Code)
	}
	if !equalBytes(rr.Body.Bytes(), body) {
		t.Errorf("variant fallback returned different bytes than original")
	}
}

func TestDownload_VariantUnknown(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	_, _, urlPath := uploadHelper(t, srv, slug, "x.png", realPNG())
	req := httptest.NewRequest("GET", urlPath+"?variant=quantum-foo", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown variant", rr.Code)
	}
}

func TestDownload_VariantHitsDerivedRow(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	id, _, urlPath := uploadHelper(t, srv, slug, "with-thumb.png", body)

	// Hand-craft a derived row pointing at the same blob (so the FSStore
	// already has the bytes — TASK-878 will do this for real with a
	// real thumbnail). Variant lookup MUST find it and serve it.
	wsID := mustWorkspaceID(t, srv, slug)
	att, _ := srv.store.GetAttachment(id)
	parentID := att.ID
	variant := models.AttachmentVariantThumbSm
	derived := &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "system",
		StorageKey:  att.StorageKey, // same blob
		ContentHash: att.ContentHash,
		MimeType:    att.MimeType,
		SizeBytes:   att.SizeBytes,
		Filename:    "thumb-sm.png",
		ParentID:    &parentID,
		Variant:     &variant,
	}
	if err := srv.store.CreateAttachment(derived); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", urlPath+"?variant=thumb-sm", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "thumb-sm.png") {
		t.Errorf("Content-Disposition = %q, want filename to be the derived row's (thumb-sm.png)", got)
	}
}

func TestDownload_BlobMissingOnDisk(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _, urlPath := uploadHelper(t, srv, slug, "ephemeral.png", realPNG())

	// Yank the blob out from under the row by deleting it via the store
	// directly. This simulates the "DB row points at a hash whose file
	// has been GC'd / corrupted" case.
	att, _ := srv.store.GetAttachment(id)
	store, err := srv.attachments.Resolve(att.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), att.StorageKey); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", urlPath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when blob missing for live row", rr.Code)
	}
}

func TestDownload_FilenameSanitized(t *testing.T) {
	// We can't trigger this via upload (filepath.Base strips path
	// separators) so test the helper directly.
	cases := map[string]string{
		"normal.png":          "normal.png",
		`"quoted".png`:        "quoted.png",
		"control\x00\x01.png": "control.png",
		`with\backslash.png`:  "withbackslash.png",
		"":                    "attachment",
		"\x00\x01\x02":        "attachment",
	}
	for in, want := range cases {
		if got := sanitizeHeaderFilename(in); got != want {
			t.Errorf("sanitizeHeaderFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// silence unused-import linter when this file is the only consumer
var _ = io.EOF
var _ = fmt.Sprintf
var _ = attachments.FSPrefix
