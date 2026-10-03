package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3379: workspace import keeps the export's comment authors, version
// kinds and attachment uploaders verbatim (BUG-3372 ruling (1)), so the rows
// it writes carry a marker the UI shows as "imported, author not verified".
// Nothing stored before could tell them apart: user_id is NULL after an
// account deletion too, and the export supplies the timestamps.

func importWorkspaceJSON(t *testing.T, srv *Server, export models.WorkspaceExport) string {
	t.Helper()
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=imported3379", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	return ws.Slug
}

func TestBUG3379_ImportedCommentsAndVersionsAreMarked(t *testing.T) {
	src := testServer(t)
	srcSlug := createWSForTest(t, src)
	rr := doRequest(src, "POST", "/api/v1/workspaces/"+srcSlug+"/collections/tasks/items",
		map[string]any{"title": "with history", "content": "first body"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	if rr := doRequest(src, "POST", "/api/v1/workspaces/"+srcSlug+"/items/"+item.Slug+"/comments",
		map[string]any{"body": "native"}); rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}

	// Control: the native comment, with no account (user_id empty, the same
	// NULL an account deletion leaves), is NOT marked. The marker is not
	// keyed on a missing account.
	rr = doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/items/"+item.Slug+"/comments", nil)
	var native []models.Comment
	parseJSON(t, rr, &native)
	if len(native) != 1 || native[0].UserID != "" || native[0].Imported {
		t.Fatalf("control: native comment = %+v, want one unmarked comment with no user_id", native)
	}

	rr = doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d", rr.Code)
	}
	var export models.WorkspaceExport
	parseJSON(t, rr, &export)
	if len(export.Comments) != 1 || len(export.ItemVersions) == 0 {
		t.Fatalf("export carries %d comments, %d versions", len(export.Comments), len(export.ItemVersions))
	}
	export.Comments[0].Author = "Dave Member"
	export.ItemVersions[0].CreatedBy = "user"

	dst := testServer(t)
	dstSlug := importWorkspaceJSON(t, dst, export)

	rr = doRequest(dst, "GET", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug+"/comments", nil)
	var imported []models.Comment
	parseJSON(t, rr, &imported)
	if len(imported) != 1 || !imported[0].Imported || imported[0].Author != "Dave Member" {
		t.Errorf("imported comment = %+v, want the export's author kept and imported=true", imported)
	}

	rr = doRequest(dst, "GET", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("versions: %d %s", rr.Code, rr.Body.String())
	}
	var versions []models.Version
	parseJSON(t, rr, &versions)
	if len(versions) == 0 {
		t.Fatal("no versions imported")
	}
	for _, v := range versions {
		if !v.Imported {
			t.Errorf("imported version %s not marked", v.ID)
		}
	}

	// A local edit right after the import starts its own, unmarked version
	// instead of being throttled into the imported one.
	if rr := doRequest(dst, "PATCH", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug,
		map[string]any{"content": "edited here"}); rr.Code != http.StatusOK {
		t.Fatalf("local edit: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(dst, "GET", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug+"/versions", nil)
	var afterEdit []models.Version
	parseJSON(t, rr, &afterEdit)
	if len(afterEdit) != len(versions)+1 {
		t.Errorf("versions after a local edit = %d, want %d (a new, native row)", len(afterEdit), len(versions)+1)
	} else if afterEdit[0].Imported {
		t.Error("the newest version after a local edit is marked imported")
	}

	// A comment written natively in the imported workspace is not marked.
	if rr := doRequest(dst, "POST", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug+"/comments",
		map[string]any{"body": "written here"}); rr.Code != http.StatusCreated {
		t.Fatalf("native comment in the import: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(dst, "GET", "/api/v1/workspaces/"+dstSlug+"/items/"+item.Slug+"/comments", nil)
	var after []models.Comment
	parseJSON(t, rr, &after)
	for _, c := range after {
		if c.Body == "written here" && c.Imported {
			t.Error("a comment written after the import is marked imported")
		}
	}
}

// An imported attachment's uploaded_by is the source instance's claim. It
// is never resolved to a local name, even when it is a real member's id.
func TestBUG3379_ImportedAttachmentNamesNoUploader(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	// Upload while the instance has no accounts yet; creating one ends that.
	id, _, _ := uploadSource(t, srv, slug, "native.txt")
	native, err := srv.store.GetAttachment(id)
	if err != nil || native == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	member, tok := loginTestUserAs(t, srv, "member3379@example.test", "Real Member", "pw-3379-abcdefgh")
	if err := srv.store.AddWorkspaceMember(native.WorkspaceID, member.ID, "owner"); err != nil {
		t.Fatal(err)
	}

	head := func(attID string) http.Header {
		t.Helper()
		rr := doRequestWithCookie(srv, "HEAD", "/api/v1/workspaces/"+slug+"/attachments/"+attID, nil, tok)
		if rr.Code != http.StatusOK {
			t.Fatalf("HEAD %s: %d", attID, rr.Code)
		}
		return rr.Header()
	}

	for _, imported := range []bool{false, true} {
		att := &models.Attachment{
			WorkspaceID: native.WorkspaceID, UploadedBy: member.ID, StorageKey: native.StorageKey,
			ContentHash: native.ContentHash, MimeType: native.MimeType, SizeBytes: native.SizeBytes,
			Filename: "claimed.txt", Imported: imported,
		}
		if err := srv.store.CreateAttachment(att); err != nil {
			t.Fatal(err)
		}
		h := head(att.ID)
		if imported {
			if h.Get("X-Pad-Attachment-Uploaded-By") != "" || h.Get("X-Pad-Attachment-Imported") != "1" {
				t.Errorf("imported: uploaded-by=%q imported=%q, want no name and imported=1",
					h.Get("X-Pad-Attachment-Uploaded-By"), h.Get("X-Pad-Attachment-Imported"))
			}
		} else if h.Get("X-Pad-Attachment-Uploaded-By") != "Real%20Member" || h.Get("X-Pad-Attachment-Imported") != "" {
			// Control: a native row naming the same account still resolves.
			t.Errorf("control: uploaded-by=%q imported=%q, want Real%%20Member and no marker",
				h.Get("X-Pad-Attachment-Uploaded-By"), h.Get("X-Pad-Attachment-Imported"))
		}
	}
}

// The bundle import marks the attachment rows it writes.
func TestBUG3379_BundleImportMarksAttachments(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	uploadSource(t, src, srcSlug, "carried.txt")
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d", rr.Code)
	}
	dst, _ := testServerWithAttachments(t)
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=bundle3379", bytes.NewReader(rr.Body.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr = httptest.NewRecorder()
	dst.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	rr = doRequest(dst, "GET", "/api/v1/workspaces/"+ws.Slug+"/attachments", nil)
	var list struct {
		Attachments []models.Attachment `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Attachments) == 0 {
		t.Fatalf("no attachments imported: %s", rr.Body.String())
	}
	for _, a := range list.Attachments {
		if !a.Imported {
			t.Errorf("imported attachment %s (%s) not marked", a.ID, a.Filename)
		}
	}
}
