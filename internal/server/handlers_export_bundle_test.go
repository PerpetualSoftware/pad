package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestExportBundle_RoundTrip pins TASK-884: GET /export?format=tar
// returns a gzip'd tar containing pad-export.json + an attachment
// manifest + every original blob, and the bytes match what was
// uploaded.
func TestExportBundle_RoundTrip(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	// Upload two attachments so the bundle has something to ship.
	body1 := realPNG()
	body2 := append(realPNG(), 0xCA, 0xFE) // distinct content → distinct hash
	if rr := doMultipartUpload(srv, slug, "first.png", body1); rr.Code != http.StatusCreated {
		t.Fatalf("upload 1: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doMultipartUpload(srv, slug, "second.png", body2); rr.Code != http.StatusCreated {
		t.Fatalf("upload 2: %d %s", rr.Code, rr.Body.String())
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", got)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, ".tar.gz") {
		t.Errorf("Content-Disposition = %q, want filename=...tar.gz", got)
	}

	files := readBundle(t, rr.Body.Bytes())

	// Required entries: pad-export.json + manifest + 2 blobs.
	if _, ok := files["pad-export.json"]; !ok {
		t.Fatalf("bundle missing pad-export.json; got entries: %v", keys(files))
	}
	if _, ok := files["attachments/manifest.json"]; !ok {
		t.Fatalf("bundle missing attachments/manifest.json; got entries: %v", keys(files))
	}

	// Decode the manifest and confirm the entries point at real blobs
	// in the archive whose bytes match SizeBytes + the upload payload.
	var manifest models.AttachmentManifest
	if err := json.Unmarshal(files["attachments/manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	if manifest.Version != exportBundleVersion {
		t.Errorf("manifest version=%d, want %d", manifest.Version, exportBundleVersion)
	}
	if len(manifest.Entries) != 2 {
		t.Fatalf("manifest entries = %d, want 2", len(manifest.Entries))
	}

	// Map filename → expected bytes (from the uploads).
	expected := map[string][]byte{
		"first.png":  body1,
		"second.png": body2,
	}
	for _, e := range manifest.Entries {
		want, ok := expected[e.Filename]
		if !ok {
			t.Errorf("manifest entry has unexpected filename %q", e.Filename)
			continue
		}
		if e.SizeBytes != int64(len(want)) {
			t.Errorf("entry %s: size=%d, want %d", e.Filename, e.SizeBytes, len(want))
		}
		// The blob must live at attachments/<id><ext>.
		path := bundleAttachmentPath(e.ID, e.Filename)
		got, ok := files[path]
		if !ok {
			t.Fatalf("bundle missing blob %q; got entries: %v", path, keys(files))
		}
		if !bytes.Equal(got, want) {
			t.Errorf("blob %q: bytes differ from upload (got %d bytes, want %d)",
				path, len(got), len(want))
		}
	}

	// The pad-export.json inside the bundle must round-trip through
	// the existing WorkspaceExport decoder.
	var export models.WorkspaceExport
	if err := json.Unmarshal(files["pad-export.json"], &export); err != nil {
		t.Fatalf("pad-export.json decode: %v body=%s", err, files["pad-export.json"])
	}
	if export.Version != 1 {
		t.Errorf("export version=%d, want 1", export.Version)
	}
	if export.Workspace.Slug != slug {
		t.Errorf("export workspace slug=%q, want %q", export.Workspace.Slug, slug)
	}
}

// TestExportBundle_HidesThumbnails confirms derived attachment rows
// (parent_id != NULL) don't show up in the bundle. They're re-derived
// on import; shipping them would double the bundle size for no
// benefit.
func TestExportBundle_HidesThumbnails(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "x.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}
	originalID := getOnlyAttachmentID(t, srv, wsID)

	// Synthesize a thumbnail row directly. We use a fake hash + key so
	// the bundle code doesn't try to fetch a real thumb blob (and
	// fail) — the assertion is purely about manifest filtering.
	// Without filtering, the manifest would list this row and the
	// streaming step would 404 on the missing key, which is its own
	// (different) bug. The check on the manifest count alone catches
	// the regression we care about.
	thumbVariant := "thumb-sm"
	thumb := &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "system",
		StorageKey:  "fs:fakethumb",
		ContentHash: "fakethumb",
		MimeType:    "image/png",
		SizeBytes:   1,
		Filename:    "x-thumb-sm.png",
		ParentID:    &originalID,
		Variant:     &thumbVariant,
	}
	if err := srv.store.CreateAttachment(thumb); err != nil {
		t.Fatalf("CreateAttachment thumb: %v", err)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s", rr.Code, rr.Body.String())
	}
	files := readBundle(t, rr.Body.Bytes())

	var manifest models.AttachmentManifest
	if err := json.Unmarshal(files["attachments/manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest decode: %v", err)
	}
	if len(manifest.Entries) != 1 {
		t.Fatalf("manifest entries=%d, want 1 (thumbnails must be excluded)", len(manifest.Entries))
	}
	for _, e := range manifest.Entries {
		if e.ID == thumb.ID {
			t.Errorf("thumbnail %s leaked into export manifest", thumb.ID)
		}
	}
}

// TestExportBundle_TruncatedBlobAbortsStream pins the close-error
// path that Codex flagged on PR #305 round 2/3: if a backend returns
// fewer bytes than the size_bytes column claims, the streaming
// handler must NOT silently emit a corrupt 200. Two complementary
// signals must surface so a downstream client can detect corruption:
//
//   - The HTTP trailer X-Bundle-Status is absent or != "ok"
//     (handler skips setting it on the error path).
//   - The gzip stream itself is truncated (handler skips the clean
//     close so the gzip trailer is unwritten and a gzip reader
//     returns ErrUnexpectedEOF when reaching EOF).
//
// We synthesize the desync by setting size_bytes higher than the
// actual on-disk blob length.
func TestExportBundle_TruncatedBlobAbortsStream(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "small.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := getOnlyAttachmentID(t, srv, wsID)

	if _, err := srv.store.DB().Exec(
		`UPDATE attachments SET size_bytes = 999999 WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("desync size_bytes: %v", err)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s", rr.Code, rr.Body.String())
	}
	// The httptest recorder doesn't echo trailers separately the way
	// a real http.Response does, but the handler's ResponseWriter is
	// the recorder itself — so any trailer the handler set lands in
	// rr.Header(). Assert the success trailer is absent.
	if got := rr.Header().Get("X-Bundle-Status"); got == "ok" {
		t.Errorf("X-Bundle-Status=%q after truncation; want absent or non-ok", got)
	}

	// Independently: the gzip stream should fail to fully decode
	// because the handler never closed the gzip writer cleanly.
	gz, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		// Truncated gzip header is acceptable too.
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	sawErr := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			sawErr = true
			break
		}
		if _, err := io.ReadAll(tr); err != nil {
			sawErr = true
			break
		}
		_ = hdr
	}
	// Belt: if the tar reader didn't error, the gzip footer should
	// have been malformed and gz.Close() should report it.
	if !sawErr {
		if err := gz.Close(); err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("truncated blob produced a clean bundle; expected gzip/tar to surface the corruption")
	}
}

// TestExportBundle_SuccessTrailer pins the success path of the
// X-Bundle-Status trailer added in PR #305 round 3: a clean stream
// sets the trailer to "ok" so a CLI can confirm the bundle is
// complete before reporting success to the user.
func TestExportBundle_SuccessTrailer(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	if rr := doMultipartUpload(srv, slug, "x.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Bundle-Status"); got != "ok" {
		t.Errorf("X-Bundle-Status = %q on clean stream, want ok", got)
	}
}

// TestExportBundle_LegacyJSONStillWorks confirms the default (no
// query param) export endpoint still returns plain JSON, so any
// existing automation continues to work after TASK-884 lands.
func TestExportBundle_LegacyJSONStillWorks(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy export: status=%d body=%s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("legacy Content-Type=%q, want application/json", ct)
	}
	var export models.WorkspaceExport
	if err := json.Unmarshal(rr.Body.Bytes(), &export); err != nil {
		t.Fatalf("legacy export decode: %v", err)
	}
}

// readBundle decompresses + extracts a gzipped tar bundle into a
// map[name]bytes. Test helper kept here so both export and (later)
// import tests can read bundle output.
func readBundle(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = buf
	}
	return out
}

// keys is a tiny helper for deterministic-ish error messages —
// fmt.Sprintf("%v", map) order isn't stable across Go versions and
// hides the actual contents.
func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// httptestRecorder is a small helper used by callers that don't care
// about the request path — keeps signatures terse without dragging
// the bare httptest.NewRecorder import into every test.
var _ = httptest.NewRecorder // keep linter happy if helper goes unused
