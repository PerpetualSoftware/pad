package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2818: a name the blocklist passed was served as a blocked extension,
// because the Content-Disposition sanitiser dropped characters (control
// bytes, '"', '\') that the blocklist had counted as part of the extension.

// svgScript sniffs as text/xml, an ALLOWED type, so only the extension refuses it.
var svgScript = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)

// uploadEncodedName uploads body under an RFC 5987 encoded filename. That is
// the reachable spelling for these bytes: Go's multipart reader refuses a raw
// control byte in a header line before any handler sees it.
func uploadEncodedName(t *testing.T, srv *Server, slug, encoded string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename*=UTF-8''`+encoded)
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	part.Write(body)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// The attack at the upload door: every spelling that used to store and serve
// as ".svg" is now refused as extension_blocked, because the blocklist judges
// the normalised name.
func TestUpload_BUG2818_HiddenBlockedExtensionIsRefused(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	for _, encoded := range []string{
		"x.s%0Bvg",    // vertical tab (C0)
		"x.s%08vg",    // backspace
		"x.s%7Fvg",    // DEL
		"x.s%C2%85vg", // NEL (C1)
		"x.s%22vg",    // double quote
		"x.svg.",      // trailing dot
		"x.svg%20",    // trailing space
	} {
		rr := uploadEncodedName(t, srv, slug, encoded, svgScript)
		if rr.Code != http.StatusUnsupportedMediaType || !strings.Contains(rr.Body.String(), "extension_blocked") {
			t.Errorf("%s: status=%d body=%s; want 415 extension_blocked", encoded, rr.Code, rr.Body.String())
		}
	}
	// Control: the same bytes under an honest, allowed name are accepted, so
	// the refusals above are about the name.
	if rr := uploadEncodedName(t, srv, slug, "x.xml", svgScript); rr.Code != http.StatusCreated {
		t.Fatalf("control upload x.xml: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// A benign name carrying a stray control byte is stored without it, not refused.
func TestUpload_BUG2818_BenignControlByteIsNormalisedNotRefused(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := uploadEncodedName(t, srv, slug, "my%09notes.txt", []byte("plain text\n"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Filename != "mynotes.txt" {
		t.Errorf("stored filename = %q, want %q", got.Filename, "mynotes.txt")
	}
}

// A LEGACY row, stored before ingest normalised, is served under a name that
// cannot be the blocked type, whatever its stored bytes say.
func TestDownload_BUG2818_LegacyRowIsNotServedAsABlockedExtension(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	for _, legacy := range []string{"x.s\vvg", `x.s"vg`, "x.svg.", "setup.e\x08xe"} {
		id, _, urlPath := uploadHelper(t, srv, slug, "legacy.xml", svgScript)
		if _, err := srv.store.DB().Exec(`UPDATE attachments SET filename = ? WHERE id = ?`, legacy, id); err != nil {
			t.Fatalf("plant legacy name %q: %v", legacy, err)
		}
		req := httptest.NewRequest("GET", urlPath, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", legacy, rr.Code, rr.Body.String())
		}
		served := dispositionFilename(t, rr.Header().Get("Content-Disposition"))
		if attachments.BlockedExtension(filepath.Ext(served)) {
			t.Errorf("legacy %q served as %q: a blocked extension the blocklist never judged", legacy, served)
		}
		if !strings.HasSuffix(served, ".bin") {
			t.Errorf("legacy %q served as %q, want the extension neutralised to .bin", legacy, served)
		}
	}
}

// dispositionFilename reads the filename parameter the way a client does.
func dispositionFilename(t *testing.T, header string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		t.Fatalf("parse Content-Disposition %q: %v", header, err)
	}
	return params["filename"]
}

// The SECOND door: bundle import stored the manifest's name verbatim. A bundle
// whose entry hides .svg is now judged on the normalised name, and refused
// (the importer skips a refused attachment), while an entry whose name only
// needs cleaning is imported cleaned.
func TestImportBundle_BUG2818_ManifestNamesAreNormalisedBeforeTheBlocklist(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	uploadHelper(t, src, srcSlug, "attack.xml", svgScript)
	uploadHelper(t, src, srcSlug, "benign.txt", []byte("plain text\n"))
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	bundle := rewriteManifestNames(t, rr.Body.Bytes(), map[string]string{
		"attack.xml": "x.s\vvg",
		"benign.txt": `..\no` + "\t" + `tes.txt`,
	})

	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=Imported", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode ws: %v", err)
	}
	rr = doRequest(dest, "GET", "/api/v1/workspaces/"+ws.Slug+"/attachments", nil)
	var list struct {
		Attachments []struct {
			Filename string `json:"filename"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	var names []string
	for _, a := range list.Attachments {
		names = append(names, a.Filename)
	}
	if len(names) != 1 || names[0] != "notes.txt" {
		t.Errorf("imported attachment names = %q, want only [\"notes.txt\"] (the hidden .svg refused, the benign one cleaned)", names)
	}
}

// rewriteManifestNames re-emits a bundle with manifest entries renamed, and
// renames each one's BLOB entry to match. The importer pairs a blob with its
// manifest entry by bundleAttachmentPath(id, filename), whose suffix comes from
// the filename, so renaming only the manifest makes a changed-extension entry
// match no blob and be skipped before it reaches the door under test (the
// first version of this fixture did exactly that, and a mutant of the door
// survived it). A crafted bundle names its blobs consistently.
func rewriteManifestNames(t *testing.T, bundle []byte, rename map[string]string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	ogz := gzip.NewWriter(&out)
	otw := tar.NewWriter(ogz)
	renamed, reblobbed := 0, 0
	blobRename := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if hdr.Name == "attachments/manifest.json" {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			entries, _ := m["entries"].([]any)
			for _, e := range entries {
				em := e.(map[string]any)
				if to, ok := rename[em["filename"].(string)]; ok {
					id := em["id"].(string)
					blobRename[bundleAttachmentPath(id, em["filename"].(string))] = bundleAttachmentPath(id, to)
					em["filename"] = to
					renamed++
				}
			}
			if body, err = json.Marshal(m); err != nil {
				t.Fatalf("remarshal: %v", err)
			}
		}
		name := hdr.Name
		if to, ok := blobRename[name]; ok {
			name = to
			reblobbed++
		}
		if err := otw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := otw.Write(body); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	otw.Close()
	ogz.Close()
	if renamed != len(rename) || reblobbed != len(rename) {
		t.Fatalf("renamed %d manifest entries and %d blobs, want %d of each: the fixture did not reach the door",
			renamed, reblobbed, len(rename))
	}
	return out.Bytes()
}

// The header and ingest drop EXACTLY the same characters, over every rune.
// This is the lead's ruled invariant for BUG-2818 as a test: a sanitiser with
// its own character switch (the pre-fix shape) differs on C1 and fails here.
func TestSanitizeHeaderFilename_DropsExactlyTheIngestPredicate(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		name := "a" + string(r) + "b"
		dropped := sanitizeHeaderFilename(name) == "ab"
		if dropped != attachments.DroppedFilenameRune(r) {
			t.Fatalf("U+%04X: header drops=%v, ingest predicate drops=%v; the two must share one rule",
				r, dropped, attachments.DroppedFilenameRune(r))
		}
	}
}
