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
)

// TASK-3612: the bundle import's 201 carries an import_summary pairing what
// the bundle carried with what the new workspace holds, so a person moving a
// workspace to Cloud can see that everything arrived.

type importSummaryCounts struct {
	Collections int `json:"collections"`
	Items       int `json:"items"`
	Comments    int `json:"comments"`
	Attachments int `json:"attachments"`
}

type importSummaryResponse struct {
	Slug          string `json:"slug"`
	ImportSummary *struct {
		InBundle importSummaryCounts `json:"in_bundle"`
		Imported importSummaryCounts `json:"imported"`
	} `json:"import_summary"`
}

// exportSummarySource builds a source workspace holding a known population,
// including rows the export deliberately leaves out (a deleted item, a
// deleted comment), and returns its bundle plus the counts it must carry.
func exportSummarySource(t *testing.T) ([]byte, importSummaryCounts) {
	t.Helper()
	src, slug := testServerWithAttachments(t)

	rr := doMultipartUpload(src, slug, "logo.png", realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}

	var itemSlugs []string
	for _, title := range []string{"Kept one", "Kept two", "Deleted"} {
		rr = doRequest(src, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items",
			map[string]any{"title": title, "content": "body"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %q: %d %s", title, rr.Code, rr.Body.String())
		}
		var it struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &it); err != nil {
			t.Fatalf("decode item: %v", err)
		}
		itemSlugs = append(itemSlugs, it.Slug)
	}
	rr = doRequest(src, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+itemSlugs[2], nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("delete item: %d %s", rr.Code, rr.Body.String())
	}

	commentIDs := make([]string, 0, 2)
	for _, body := range []string{"stays", "goes"} {
		rr = doRequest(src, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemSlugs[0]+"/comments",
			map[string]any{"body": body})
		if rr.Code != http.StatusCreated {
			t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
		}
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
			t.Fatalf("decode comment: %v", err)
		}
		commentIDs = append(commentIDs, c.ID)
	}
	rr = doRequest(src, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+itemSlugs[0]+"/comments/"+commentIDs[1], nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("delete comment: %d %s", rr.Code, rr.Body.String())
	}

	// An archived collection travels in the bundle (export carries them) and
	// must be counted on both sides, though the live list leaves it out.
	rr = doRequest(src, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
		"name": "Old Stuff", "slug": "old-stuff", "prefix": "OLD",
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(src, "DELETE", "/api/v1/workspaces/"+slug+"/collections/old-stuff", nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("archive collection: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(src, "GET", "/api/v1/workspaces/"+slug+"/collections", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list collections: %d %s", rr.Code, rr.Body.String())
	}
	var cols []json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &cols); err != nil {
		t.Fatalf("decode collections: %v", err)
	}

	rr = doRequest(src, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	// Known from what was written above, not from the bundle: the live
	// collections plus the archived one, two live items, one live comment,
	// one original attachment.
	return rr.Body.Bytes(), importSummaryCounts{
		Collections: len(cols) + 1, Items: 2, Comments: 1, Attachments: 1,
	}
}

func importBundleForSummary(t *testing.T, bundle []byte) importSummaryResponse {
	t.Helper()
	dest, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=Moved", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	dest.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var resp importSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if resp.Slug == "" {
		t.Fatalf("import response lost the workspace fields: %s", rr.Body.String())
	}
	if resp.ImportSummary == nil {
		t.Fatalf("import response has no import_summary: %s", rr.Body.String())
	}
	return resp
}

func TestImportBundle_SummaryMatchesWhatTheSourceHeld(t *testing.T) {
	t.Parallel()
	bundle, want := exportSummarySource(t)
	resp := importBundleForSummary(t, bundle)

	if got := resp.ImportSummary.InBundle; got != want {
		t.Errorf("in_bundle = %+v, want %+v", got, want)
	}
	if got := resp.ImportSummary.Imported; got != want {
		t.Errorf("imported = %+v, want %+v", got, want)
	}
}

// The case the check exists for: an attachment the manifest lists never
// lands. The import still succeeds (that path is a logged skip), so the
// summary is the only place the shortfall shows.
func TestImportBundle_SummaryReportsAMissingAttachment(t *testing.T) {
	t.Parallel()
	bundle, want := exportSummarySource(t)
	resp := importBundleForSummary(t, dropAttachmentBlobs(t, bundle))

	if got := resp.ImportSummary.InBundle.Attachments; got != want.Attachments {
		t.Errorf("in_bundle.attachments = %d, want %d", got, want.Attachments)
	}
	if got := resp.ImportSummary.Imported.Attachments; got != 0 {
		t.Errorf("imported.attachments = %d, want 0 (the blob was removed from the bundle)", got)
	}
	if got := resp.ImportSummary.Imported.Items; got != want.Items {
		t.Errorf("imported.items = %d, want %d", got, want.Items)
	}
}

// dropAttachmentBlobs rewrites a bundle without its attachment blobs,
// keeping pad-export.json and attachments/manifest.json.
func dropAttachmentBlobs(t *testing.T, bundle []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	tr := tar.NewReader(gr)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	dropped := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if strings.HasPrefix(hdr.Name, "attachments/") && hdr.Name != "attachments/manifest.json" {
			dropped++
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write entry: %v", err)
		}
	}
	if dropped == 0 {
		t.Fatal("the bundle had no attachment blob to drop")
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return out.Bytes()
}
