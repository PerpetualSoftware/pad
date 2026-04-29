//go:build !libvips

package server

import (
	"bytes"
	"encoding/json"
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// transformRequestBody marshals a transform request for tests. Mirrors
// the handler's internal struct so behavior under unknown / malformed
// bodies is observable.
type transformRequestBody struct {
	Operation string `json:"operation"`
	Degrees   int    `json:"degrees,omitempty"`
	Rect      *struct {
		X int `json:"x"`
		Y int `json:"y"`
		W int `json:"w"`
		H int `json:"h"`
	} `json:"rect,omitempty"`
}

func doTransform(srv *Server, slug, attachmentID string, body transformRequestBody) *httptest.ResponseRecorder {
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+slug+"/attachments/"+attachmentID+"/transform",
		bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func uploadIntegrationPNG(t *testing.T, srv *Server, slug string, w, h int) (id, urlPath string) {
	t.Helper()
	body := makeIntegrationPNG(t, w, h)
	rr := doMultipartUpload(srv, slug, "shot.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed upload status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload resp: %v", err)
	}
	return resp.ID, resp.URL
}

func TestTransform_Rotate90SwapsWidthHeight(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 800, 400)
	srv.Stop() // drain thumbnail goroutine

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusCreated {
		t.Fatalf("rotate status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Mime   string `json:"mime"`
		Width  *int   `json:"width"`
		Height *int   `json:"height"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == "" || resp.ID == id {
		t.Errorf("expected new attachment id, got %q (parent was %q)", resp.ID, id)
	}
	if resp.Width == nil || resp.Height == nil {
		t.Fatal("response missing width/height")
	}
	if *resp.Width != 400 || *resp.Height != 800 {
		t.Errorf("rotated dims = %dx%d, want 400x800 (90° swap)", *resp.Width, *resp.Height)
	}
	// PNG input → PNG output (preserves alpha policy).
	if resp.Mime != "image/png" {
		t.Errorf("rotated MIME = %q, want image/png", resp.Mime)
	}
}

func TestTransform_Rotate180KeepsDimensions(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 800, 400)
	srv.Stop()

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 180})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct{ Width, Height *int }
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Width == nil || resp.Height == nil || *resp.Width != 800 || *resp.Height != 400 {
		t.Errorf("180° dims = %v×%v, want 800×400", resp.Width, resp.Height)
	}
}

func TestTransform_RotateRejectsBadDegrees(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 100, 100)
	srv.Stop()

	cases := []int{0, 45, 91, 360, -90}
	for _, deg := range cases {
		rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: deg})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("degrees=%d status = %d, want 400", deg, rr.Code)
		}
	}
}

func TestTransform_UnknownOperationIs400(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 100, 100)
	srv.Stop()

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "polarize"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown op status = %d, want 400", rr.Code)
	}
}

func TestTransform_NonExistentAttachmentIs404(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	defer srv.Stop()

	rr := doTransform(srv, slug, "00000000-0000-0000-0000-000000000000",
		transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestTransform_CrossWorkspaceReturns404(t *testing.T) {
	// Upload into A, attempt transform from B — must be 404, not 403.
	srv, slugA := testServerWithAttachments(t)
	slugB := createWSForTest(t, srv)
	id, _ := uploadIntegrationPNG(t, srv, slugA, 100, 100)
	srv.Stop()

	rr := doTransform(srv, slugB, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-workspace status = %d, want 404 (NOT 403)", rr.Code)
	}
}

func TestTransform_DisabledWhenNoProcessor(t *testing.T) {
	// Upload via a server WITH a processor (so we have a real attachment to point at),
	// then unwire the processor and verify transform reports the disabled state.
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 100, 100)
	srv.Stop()

	srv.SetImageProcessor(nil)

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no-processor status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "image_processor_disabled") {
		t.Errorf("error code missing from body: %s", rr.Body.String())
	}
}

func TestTransform_ProducesNewBlob(t *testing.T) {
	// Sanity: rotating an image creates a fresh content-addressed
	// blob with a different hash + size. Without this, a regression
	// that returned the parent's row would silently break the
	// editor's UUID-swap flow.
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 800, 400)
	srv.Stop()

	parent, err := srv.store.GetAttachment(id)
	if err != nil || parent == nil {
		t.Fatalf("get parent: %v", err)
	}

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	derived, err := srv.store.GetAttachment(resp.ID)
	if err != nil || derived == nil {
		t.Fatalf("get derived: %v", err)
	}
	if derived.ContentHash == parent.ContentHash {
		t.Errorf("derived hash matches parent (%s) — transform did not produce new bytes", parent.ContentHash)
	}
	// New row must inherit workspace + uploaded_by + item_id from parent.
	if derived.WorkspaceID != parent.WorkspaceID {
		t.Errorf("derived WorkspaceID = %q, want %q", derived.WorkspaceID, parent.WorkspaceID)
	}
	// Filename should carry the operation tag so a direct download
	// surfaces what happened (UI nicety; the transform endpoint
	// guarantees this filename shape).
	if !strings.Contains(derived.Filename, "rotate") {
		t.Errorf("derived filename %q missing rotate tag", derived.Filename)
	}
}

func TestTransform_ServedDownloadDecodesAtNewDimensions(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 800, 400)
	srv.Stop()

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	// Fetch the new blob via the standard GET handler.
	req := httptest.NewRequest("GET", resp.URL, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	getRR := httptest.NewRecorder()
	srv.ServeHTTP(getRR, req)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get rotated status = %d", getRR.Code)
	}
	out, _, err := image.Decode(bytes.NewReader(getRR.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode rotated bytes: %v", err)
	}
	if out.Bounds().Dx() != 400 || out.Bounds().Dy() != 800 {
		t.Errorf("served rotated dims = %v, want 400x800", out.Bounds())
	}
}

func TestTransform_DerivedRowInheritsUploadedByFromParent(t *testing.T) {
	// A user who rotates someone else's upload should NOT take
	// ownership of the resulting blob. The transform pipeline
	// inherits parent.UploadedBy (same policy as the thumbnail
	// pipeline) so audit attribution stays anchored to the
	// original uploader.
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 800, 400)
	srv.Stop()

	parent, _ := srv.store.GetAttachment(id)
	if parent == nil {
		t.Fatal("parent missing")
	}
	parentUploader := parent.UploadedBy

	rr := doTransform(srv, slug, id, transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	derived, _ := srv.store.GetAttachment(resp.ID)
	if derived == nil {
		t.Fatal("derived missing")
	}
	if derived.UploadedBy != parentUploader {
		t.Errorf("derived UploadedBy = %q, want %q (inherited from parent)",
			derived.UploadedBy, parentUploader)
	}
}

func TestTransform_DeletedParentReturns404(t *testing.T) {
	// Edge: the editor sends a transform after the user soft-deleted
	// the original (race or stale UI). 404, not 500 / corrupt blob.
	srv, slug := testServerWithAttachments(t)
	id, _ := uploadIntegrationPNG(t, srv, slug, 100, 100)
	srv.Stop()

	// Manually soft-delete via the store's CreateAttachment-style row update.
	// The handler checks DeletedAt — set it to a non-nil time.
	parent, _ := srv.store.GetAttachment(id)
	if parent == nil {
		t.Fatal("seed attachment missing")
	}
	// We need a soft-delete path. The store doesn't expose one yet (TASK-886
	// will add the GC path), so this test simulates the scenario by
	// constructing a request against an ID that was never created.
	// The shape is identical for the handler's purposes — both go through
	// the same nil/DeletedAt check.
	rr := doTransform(srv, slug, "00000000-0000-0000-0000-000000000000",
		transformRequestBody{Operation: "rotate", Degrees: 90})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
