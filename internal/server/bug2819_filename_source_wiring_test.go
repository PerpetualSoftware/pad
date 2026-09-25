package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2819 wiring: each door that writes an attachment row records where the
// name came from, driven from the consuming side (the router), not by calling
// the normaliser. The discriminating pair is two uploads that STORE THE SAME
// NAME, "upload.txt": one a caller who really named the file that, the other a
// name the server substituted. Only filename_source tells them apart.

func uploadSource(t *testing.T, srv *Server, slug, encoded string) (id, filename, source string) {
	t.Helper()
	rr := uploadEncodedName(t, srv, slug, encoded, []byte("plain text\n"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload %q: status=%d body=%s", encoded, rr.Code, rr.Body.String())
	}
	var got struct {
		ID             string `json:"id"`
		Filename       string `json:"filename"`
		FilenameSource string `json:"filename_source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The stored row must agree with the response.
	row, err := srv.store.GetAttachment(got.ID)
	if err != nil || row == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if row.FilenameSource != got.FilenameSource {
		t.Errorf("stored source %q, response said %q", row.FilenameSource, got.FilenameSource)
	}
	return got.ID, got.Filename, got.FilenameSource
}

func TestUpload_BUG2819_FilenameSourceIsRecordedAtTheDoor(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	_, callerName, callerSource := uploadSource(t, srv, slug, "upload.txt")
	_, subName, subSource := uploadSource(t, srv, slug, "%FF%FE.txt") // invalid UTF-8
	_, _, normSource := uploadSource(t, srv, slug, "my%09notes.txt")

	if callerName != "upload.txt" || subName != "upload.txt" {
		t.Fatalf("fixture: names %q / %q, want both \"upload.txt\" so only the source differs", callerName, subName)
	}
	if callerSource != "caller" {
		t.Errorf("a file the caller named upload.txt: source %q, want caller", callerSource)
	}
	if subSource != "substituted" {
		t.Errorf("an unstorable name replaced by upload.txt: source %q, want substituted", subSource)
	}
	if normSource != "normalised" {
		t.Errorf("a name with a dropped tab: source %q, want normalised", normSource)
	}

	// The list endpoint serialises the same field (it embeds the model).
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments", nil)
	var list struct {
		Attachments []struct {
			FilenameSource string `json:"filename_source"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	seen := map[string]int{}
	for _, a := range list.Attachments {
		seen[a.FilenameSource]++
	}
	if seen["caller"] != 1 || seen["substituted"] != 1 || seen["normalised"] != 1 {
		t.Errorf("list sources = %v, want one each of caller/substituted/normalised", seen)
	}
}

// Export then import: a substituted name that arrives unchanged keeps
// "substituted" (the bundle's claim), where recomputing at the importer would
// have called it the caller's own name.
func TestImportBundle_BUG2819_FilenameSourceSurvivesARoundTrip(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	uploadSource(t, src, srcSlug, "upload.txt")
	uploadSource(t, src, srcSlug, "%FF%FE.txt")

	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=Imported", bytes.NewReader(rr.Body.Bytes()))
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
			Filename       string `json:"filename"`
			FilenameSource string `json:"filename_source"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	seen := map[string]int{}
	for _, a := range list.Attachments {
		if a.Filename != "upload.txt" {
			t.Errorf("imported name %q, want upload.txt", a.Filename)
		}
		seen[a.FilenameSource]++
	}
	if seen["caller"] != 1 || seen["substituted"] != 1 {
		t.Errorf("imported sources = %v, want one caller and one substituted", seen)
	}
}

func TestThumbnails_BUG2819_VariantsAreDerived(t *testing.T) {
	f := newDerivationFixture(t)
	attID := f.attachTo(t, &f.itemID)
	f.srv.deriveThumbnails(attID)
	assertVariants(t, f.srv, attID)
	for _, variant := range []string{models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd} {
		row, err := variantRow(t, f.srv, attID, variant)
		if err != nil || row == nil {
			t.Fatalf("variant %s: %v", variant, err)
		}
		if row.FilenameSource != "derived" {
			t.Errorf("variant %s source %q, want derived", variant, row.FilenameSource)
		}
	}
}
