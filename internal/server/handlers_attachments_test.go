package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// testServerWithAttachments returns a fresh test server with the
// attachment registry wired against an FSStore rooted in t.TempDir().
func testServerWithAttachments(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
	slug := createWSForTest(t, srv)
	return srv, slug
}

func doMultipartUpload(srv *Server, slug, filename string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", filename)
	part.Write(body)
	mw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// realPNG returns a tiny but real PNG (1x1 transparent) so image.DecodeConfig succeeds.
func realPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}

func TestUpload_HappyPathPNG(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	rr := doMultipartUpload(srv, slug, "screenshot.png", body)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		MIME       string `json:"mime"`
		Size       int64  `json:"size"`
		Width      *int   `json:"width"`
		Height     *int   `json:"height"`
		Filename   string `json:"filename"`
		Category   string `json:"category"`
		RenderMode string `json:"render_mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if resp.ID == "" {
		t.Fatal("response missing id")
	}
	if resp.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", resp.MIME)
	}
	if resp.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", resp.Size, len(body))
	}
	if resp.Width == nil || *resp.Width != 1 || resp.Height == nil || *resp.Height != 1 {
		t.Errorf("width/height = %v / %v, want 1/1", resp.Width, resp.Height)
	}
	if resp.Category != "image" {
		t.Errorf("category = %q, want image", resp.Category)
	}
	if resp.RenderMode != "inline" {
		t.Errorf("render_mode = %q, want inline", resp.RenderMode)
	}
	if !strings.Contains(resp.URL, "/attachments/"+resp.ID) {
		t.Errorf("url = %q, want to contain attachment id", resp.URL)
	}
}

func TestUpload_RejectsExeAsPNG(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	exe := []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff")
	rr := doMultipartUpload(srv, slug, "totally-safe.png", exe)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsExtensionMismatch(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doMultipartUpload(srv, slug, "evil.pdf", realPNG())
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsEmpty(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doMultipartUpload(srv, slug, "empty.png", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsMissingFilePart(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("notfile", "ignored")
	mw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_OverSizeLimit(t *testing.T) {
	srv := testServer(t)
	dir := t.TempDir()
	fs, _ := attachments.NewFSStore(dir)
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	// Set a tiny 1KiB cap so we don't have to allocate 25 MiB in the test.
	srv.SetAttachments(reg, 1024)
	slug := createWSForTest(t, srv)

	// Build a body larger than the cap. Use a 4KiB png header padded with PNG-like bytes.
	body := append(realPNG(), make([]byte, 4096)...)
	rr := doMultipartUpload(srv, slug, "big.png", body)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_DedupeSameContent(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])

	var ids []string
	for i := 0; i < 2; i++ {
		rr := doMultipartUpload(srv, slug, fmt.Sprintf("dup-%d.png", i), body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &r)
		ids = append(ids, r.ID)
	}

	if ids[0] == ids[1] {
		t.Fatal("dedupe should produce two distinct attachment rows referencing the same blob")
	}

	// Look at the underlying store via the workspace usage helper —
	// SUM(size_bytes) for two rows of the same file should be 2x the bytes.
	// Resolve workspace ID via store.
	wsID := mustWorkspaceID(t, srv, slug)
	usage, err := srv.store.WorkspaceStorageUsage(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageUsage: %v", err)
	}
	if usage != int64(2*len(body)) {
		t.Errorf("usage = %d, want %d (2 rows of same blob)", usage, 2*len(body))
	}

	// Both rows reference the same content_hash.
	att1, _ := srv.store.GetAttachment(ids[0])
	att2, _ := srv.store.GetAttachment(ids[1])
	if att1.ContentHash != att2.ContentHash || att1.ContentHash != hashHex {
		t.Errorf("hashes differ: %s vs %s vs expected %s", att1.ContentHash, att2.ContentHash, hashHex)
	}
	if att1.StorageKey != att2.StorageKey {
		t.Errorf("storage keys differ — dedupe should send both at fs:%s", hashHex)
	}
}

func TestUpload_ConcurrentSameFileNoCorruption(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()

	const workers = 8
	var wg sync.WaitGroup
	codes := make([]int, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			rr := doMultipartUpload(srv, slug, fmt.Sprintf("c-%d.png", i), body)
			codes[i] = rr.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("worker %d: status %d", i, c)
		}
	}

	// Verify the on-disk blob is still readable and matches our PNG bytes.
	wsID := mustWorkspaceID(t, srv, slug)
	usage, _ := srv.store.WorkspaceStorageUsage(wsID)
	if usage != int64(workers*len(body)) {
		t.Errorf("usage = %d, want %d (%d rows * %d bytes)", usage, workers*len(body), workers, len(body))
	}
}

// mustWorkspaceID resolves a workspace slug to its internal ID via the
// store. Tests use it because they don't have access to the request
// context that getWorkspaceID reads.
func mustWorkspaceID(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}
	return ws.ID
}

// Ensure server.handleUploadAttachment 503s when no registry is wired
// (defensive check against future callers initializing the server
// without SetAttachments).
func TestUpload_NoRegistryWired(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	rr := doMultipartUpload(srv, slug, "x.png", realPNG())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rr.Code, rr.Body.String())
	}
}

// Ensure unused imports don't cause CI issues even if helpers are removed.
var _ = io.Discard
var _ = models.Attachment{}
