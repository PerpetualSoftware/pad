package server

import (
	"bytes"
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
