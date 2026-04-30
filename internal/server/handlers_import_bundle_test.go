package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestImportBundle_RoundTrip pins TASK-885's acceptance criterion:
// export from one workspace → import into a fresh server → items
// keep their pad-attachment:UUID references intact (rewritten to
// the new attachment ids), the blobs are reachable through the
// download endpoint, and the byte content matches the original
// upload.
//
// The most realistic test of a feature that touches three layers
// (export tar, import dispatch, attachment-reference remap). If any
// stage drops a UUID or fails to rewrite content, the final item
// won't render the image and this test catches it.
func TestImportBundle_RoundTrip(t *testing.T) {
	// 1. Source workspace: upload an attachment, attach it to an
	//    item, embed the pad-attachment: reference in the item's
	//    markdown content.
	src, srcSlug := testServerWithAttachments(t)

	body := realPNG()
	rr := doMultipartUpload(src, srcSlug, "logo.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var upload struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	// Create an item that references the attachment in markdown.
	itemContent := fmt.Sprintf("Hello world\n\n![logo](pad-attachment:%s)\n", upload.ID)
	rr = doRequest(src, "POST", "/api/v1/workspaces/"+srcSlug+"/collections/docs/items",
		map[string]any{"title": "With Image", "content": itemContent})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}

	// 2. Export the source workspace as a bundle.
	rr = doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	bundle := rr.Body.Bytes()

	// 3. Spin up a fresh server (independent storage) and import the
	//    bundle into it. Using a separate server is what proves the
	//    UUID remap actually works — same-server import would
	//    accidentally pass even if the remap were broken.
	dest, _ := testServerWithAttachments(t)

	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=Imported",
		bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var newWS models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &newWS); err != nil {
		t.Fatalf("decode new ws: %v", err)
	}

	// 4. The destination workspace must have a new attachments table
	//    with the rehydrated row, and the imported item's content
	//    must reference the NEW attachment id (not the old one).
	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+newWS.Slug+"/attachments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list attachments: %d %s", rr.Code, rr.Body.String())
	}
	var attResp struct {
		Attachments []struct {
			ID        string `json:"id"`
			Filename  string `json:"filename"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &attResp); err != nil {
		t.Fatalf("decode att list: %v", err)
	}
	if len(attResp.Attachments) != 1 {
		t.Fatalf("imported attachments: got %d, want 1", len(attResp.Attachments))
	}
	newAttID := attResp.Attachments[0].ID
	if newAttID == upload.ID {
		t.Fatalf("attachment id was NOT remapped (got %s, original %s)", newAttID, upload.ID)
	}
	if attResp.Attachments[0].SizeBytes != int64(len(body)) {
		t.Errorf("imported attachment size=%d, want %d",
			attResp.Attachments[0].SizeBytes, len(body))
	}
	if attResp.Attachments[0].Filename != "logo.png" {
		t.Errorf("imported attachment filename=%q, want logo.png",
			attResp.Attachments[0].Filename)
	}

	// 5. Item content must reference the NEW attachment id (rewrite
	//    pass worked) and NOT the old one. Read the imported item
	//    via the docs collection's items endpoint.
	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+newWS.Slug+"/collections/docs/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items: %d %s", rr.Code, rr.Body.String())
	}
	var items []models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode items: %v body=%s", err, rr.Body.String())
	}
	var imported *models.Item
	for i := range items {
		if items[i].Title == "With Image" {
			imported = &items[i]
			break
		}
	}
	if imported == nil {
		t.Fatalf("imported item not found; got %d items", len(items))
	}
	if !strings.Contains(imported.Content, "pad-attachment:"+newAttID) {
		t.Errorf("imported content missing new attachment ref %s; content=%q",
			newAttID, imported.Content)
	}
	if strings.Contains(imported.Content, "pad-attachment:"+upload.ID) {
		t.Errorf("imported content still has stale old attachment ref %s; content=%q",
			upload.ID, imported.Content)
	}

	// 6. Download the rehydrated blob and confirm bytes match the
	//    original upload. This is the strongest guarantee that the
	//    storage backend correctly received the bytes from the bundle.
	rr = doRequest(dest, "GET",
		"/api/v1/workspaces/"+newWS.Slug+"/attachments/"+newAttID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("download imported blob: %d", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), body) {
		t.Errorf("imported blob differs from original upload (got %d bytes, want %d)",
			rr.Body.Len(), len(body))
	}
}

// TestImportBundle_LegacyJSONStillWorks confirms the JSON dispatch
// still works alongside the new bundle path. Hits the same endpoint
// with JSON content-type — must route to the existing handler.
func TestImportBundle_LegacyJSONStillWorks(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items",
		map[string]any{"title": "Plain", "content": "no attachments"}); rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d", rr.Code)
	}
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export json: %d", rr.Code)
	}
	jsonExport := rr.Body.Bytes()

	// Import into a fresh server using JSON content-type.
	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=JsonImport",
		bytes.NewReader(jsonExport))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("legacy JSON import: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestImportBundle_RejectsOutOfOrderTar pins the streaming-import
// invariant added in PR #306 round 1 (Codex P1): the bundle MUST
// place pad-export.json + manifest.json BEFORE any blob so the
// server can stream-rehydrate without buffering. Bundles that put
// blobs first would still work in the previous implementation but
// would force buffering of every blob — we now reject them up front
// with a clear error rather than silently buffering.
func TestImportBundle_RejectsOutOfOrderTar(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	// Hand-craft a tar.gz where a blob entry comes BEFORE
	// pad-export.json. Strict-format violation.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "attachments/abcd.png", Mode: 0o644, Size: 4}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	// Even if we'd write a manifest after, the blob coming first
	// already violates the contract. Stop here.
	tw.Close()
	gzw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/import", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("out-of-order bundle: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ordering") {
		t.Errorf("expected ordering-violation error, got body=%s", rr.Body.String())
	}
}

// TestImportBundle_RejectsBadGzip pins the early-error path: a
// truncated/invalid gzip body must return 400, not 500. Catches
// regressions where the gzip-reader error gets swallowed and the
// handler proceeds with a half-decompressed stream.
func TestImportBundle_RejectsBadGzip(t *testing.T) {
	srv := testServer(t)
	// Wire the attachment registry so the dispatcher doesn't 503.
	srv.SetAttachments(attachments.NewRegistry(), 0)

	garbage := []byte("not a gzip stream")
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import",
		bytes.NewReader(garbage))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad gzip: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestImportBundle_RejectsDuplicateExport pins the duplicate-entry
// guard added during the PLAN-890 audit (TASK-891). A bundle that
// contains two pad-export.json entries used to call ImportWorkspace
// twice — leaving the first workspace as an orphan with no
// attachments. The handler now rejects the second occurrence with a
// 400 so the failure is loud.
func TestImportBundle_RejectsDuplicateExport(t *testing.T) {
	// Build a real, valid pad-export.json by exporting an empty
	// workspace from a live server, then assemble a tar.gz that
	// includes it twice.
	src, srcSlug := testServerWithAttachments(t)
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export src: %d %s", rr.Code, rr.Body.String())
	}
	exportJSON := rr.Body.Bytes()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for i := 0; i < 2; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o644, Size: int64(len(exportJSON))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(exportJSON); err != nil {
			t.Fatalf("write export: %v", err)
		}
	}
	tw.Close()
	gzw.Close()

	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=DupExport",
		bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("duplicate export: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "duplicate") {
		t.Errorf("expected duplicate-export error, got body=%s", rr.Body.String())
	}
}

// TestImportBundle_RejectsDuplicateManifest pins the matching guard
// for attachments/manifest.json. A second occurrence would silently
// overwrite manifestByPath and any blobs that matched the first
// manifest's keys would look orphaned — mark as a consumed-skip and
// be lost.
func TestImportBundle_RejectsDuplicateManifest(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export src: %d %s", rr.Code, rr.Body.String())
	}
	exportJSON := rr.Body.Bytes()

	// Hand-build the manifest bytes — tiny empty manifest is fine for
	// this test; we only care that it parses, and a second copy
	// triggers the guard.
	manifest := []byte(`{"version":1,"entries":[]}`)

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o644, Size: int64(len(exportJSON))}); err != nil {
		t.Fatalf("write export header: %v", err)
	}
	if _, err := tw.Write(exportJSON); err != nil {
		t.Fatalf("write export: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "attachments/manifest.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
			t.Fatalf("write manifest header: %v", err)
		}
		if _, err := tw.Write(manifest); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	tw.Close()
	gzw.Close()

	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=DupManifest",
		bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("duplicate manifest: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "duplicate") {
		t.Errorf("expected duplicate-manifest error, got body=%s", rr.Body.String())
	}
}

// TestImportBundle_RejectsPathTraversal pins the defense-in-depth
// reject of tar entries with `..` segments or absolute paths. The
// storage backend is hash-keyed so a malicious entry name can't
// actually escape the attachment store, but rejecting up front
// keeps the audit story unambiguous and means hand-edited bundles
// fail loudly rather than slipping through the default arm.
//
// Cases involving NUL bytes are covered by the unit test on
// isSafeBundleEntryName below — Go's archive/tar refuses to encode a
// NUL-bearing header name, so we can't exercise that path through
// the integration handler.
func TestImportBundle_RejectsPathTraversal(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{"dot-dot in path", "attachments/../../etc/passwd", "unsafe entry name"},
		{"absolute unix path", "/etc/passwd", "unsafe entry name"},
		{"absolute windows path", "\\windows\\system32\\config", "unsafe entry name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			if err := tw.WriteHeader(&tar.Header{Name: tc.entry, Mode: 0o644, Size: 4}); err != nil {
				t.Fatalf("write header: %v", err)
			}
			if _, err := tw.Write([]byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
				t.Fatalf("write blob: %v", err)
			}
			tw.Close()
			gzw.Close()

			srv, _ := testServerWithAttachments(t)
			req := httptest.NewRequest("POST", "/api/v1/workspaces/import",
				bytes.NewReader(buf.Bytes()))
			req.Header.Set("Content-Type", "application/gzip")
			req.RemoteAddr = "127.0.0.1:1234"
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s: status=%d, want 400; body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantErr) {
				t.Errorf("%s: expected %q in body, got %s", tc.name, tc.wantErr, rr.Body.String())
			}
		})
	}
}

// TestIsSafeBundleEntryName covers isSafeBundleEntryName as a pure
// helper. Some inputs (NUL bytes, empty strings) can't be exercised
// through archive/tar — Go's tar.WriteHeader refuses to encode them
// — so we test the helper directly to keep the defense-in-depth
// guarantees verifiable.
func TestIsSafeBundleEntryName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"normal export", "pad-export.json", true},
		{"normal manifest", "attachments/manifest.json", true},
		{"normal blob", "attachments/abc123.png", true},
		{"single dot segment is fine", "./pad-export.json", true},
		{"empty string", "", false},
		{"absolute unix", "/etc/passwd", false},
		{"absolute windows", "\\windows\\system32", false},
		{"dot-dot prefix", "../etc/passwd", false},
		{"dot-dot mid", "attachments/../../etc/passwd", false},
		{"dot-dot only", "..", false},
		{"NUL byte mid", "attachments/foo\x00.png", false},
		{"backslash dot-dot", "attachments\\..\\..\\etc", false},
		{"trailing dot-dot", "attachments/..", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isSafeBundleEntryName(tc.in)
			if got != tc.want {
				t.Errorf("isSafeBundleEntryName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// readBundleAsBytes is a tiny helper for callers that want the raw
// bundle to feed straight into another POST. Kept separate from
// readBundle (which extracts a map) so import-side tests don't have
// to re-encode the bundle.
func readBundleAsBytes(t *testing.T, body io.Reader) []byte {
	t.Helper()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return out
}
