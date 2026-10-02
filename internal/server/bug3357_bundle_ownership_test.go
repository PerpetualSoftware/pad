package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// rewriteBundle re-tars a real bundle, passing pad-export.json through edit
// and repeating the entry named dup (if any) once more.
func rewriteBundle(t *testing.T, bundle []byte, edit func(*models.WorkspaceExport), dup string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var entries []bundleEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "pad-export.json" && edit != nil {
			var export models.WorkspaceExport
			if err := json.Unmarshal(body, &export); err != nil {
				t.Fatal(err)
			}
			edit(&export)
			if body, err = json.Marshal(&export); err != nil {
				t.Fatal(err)
			}
		}
		entries = append(entries, bundleEntry{hdr.Name, body})
		if hdr.Name == dup {
			entries = append(entries, bundleEntry{hdr.Name, body})
		}
	}
	return gzipTar(t, entries)
}

// bundleWithAttachmentOnBeta exports a workspace holding items Alpha and
// Beta and one attachment owned by Beta, and returns the tar bundle and the
// attachment's blob path inside it.
func bundleWithAttachmentOnBeta(t *testing.T) (bundle []byte, blobPath string) {
	t.Helper()
	src, slug := testServerWithAttachments(t)
	for _, title := range []string{"Alpha", "Beta"} {
		if rr := doRequest(src, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
			map[string]any{"title": title, "fields": `{"status":"open"}`}); rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
	}
	rr := doMultipartUpload(src, slug, "logo.png", realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var att struct {
		ID string `json:"id"`
	}
	parseJSON(t, rr, &att)
	// Owned by Beta. Set directly: the attach route needs an authenticated
	// uploader, which this fixture has none of.
	if _, err := src.store.DB().Exec(src.store.D().Rebind(
		`UPDATE attachments SET item_id = (SELECT id FROM items WHERE title = 'Beta') WHERE id = ?`), att.ID); err != nil {
		t.Fatalf("attach to Beta: %v", err)
	}
	rr = doRequest(src, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	gz, _ := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if len(hdr.Name) > len("attachments/") && hdr.Name != "attachments/manifest.json" {
			blobPath = hdr.Name
		}
	}
	if blobPath == "" {
		t.Fatal("the exported bundle carries no attachment blob")
	}
	return rr.Body.Bytes(), blobPath
}

// Codex r1: with two items sharing a slug, the import renames the second, and
// the bundle door used to attach the rehydrated attachment through the SLUG,
// so it landed on the wrong item. It now uses the import's source-id map.
func TestBUG3357_AttachmentOwnershipSurvivesASlugCollision(t *testing.T) {
	bundle, _ := bundleWithAttachmentOnBeta(t)
	bundle = rewriteBundle(t, bundle, func(e *models.WorkspaceExport) {
		var alpha, beta int = -1, -1
		for i, it := range e.Items {
			switch it.Title {
			case "Alpha":
				alpha = i
			case "Beta":
				beta = i
			}
		}
		if alpha < 0 || beta < 0 {
			t.Fatalf("fixture items missing")
		}
		// Alpha takes Beta's slug and goes first, so Beta is the one renamed.
		e.Items[alpha].Slug = e.Items[beta].Slug
		if alpha > beta {
			e.Items[alpha], e.Items[beta] = e.Items[beta], e.Items[alpha]
		}
	}, "")

	dest, _ := testServerWithAttachments(t)
	if rr := postBundle(dest, "OwnerWS", bundle); rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	ws, err := dest.store.GetWorkspaceBySlug("OwnerWS")
	if err != nil || ws == nil {
		t.Fatalf("imported workspace: %v", err)
	}
	var betaID, betaSlug, owner string
	if err := dest.store.DB().QueryRow(dest.store.D().Rebind(`SELECT id, slug FROM items WHERE workspace_id = ? AND title = 'Beta'`), ws.ID).Scan(&betaID, &betaSlug); err != nil {
		t.Fatalf("find Beta: %v", err)
	}
	if betaSlug == "beta" {
		t.Fatalf("precondition: Beta should have been renamed by the slug collision, kept %q", betaSlug)
	}
	if err := dest.store.DB().QueryRow(dest.store.D().Rebind(`SELECT COALESCE(item_id, '') FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL`), ws.ID).Scan(&owner); err != nil {
		t.Fatalf("find attachment: %v", err)
	}
	if owner != betaID {
		t.Fatalf("the attachment belongs to %q, want Beta %q", owner, betaID)
	}
}

// Codex r1: the same blob path twice in the tar rehydrated the attachment
// twice, and references followed the last copy.
func TestBUG3357_RepeatedBlobEntryRefused(t *testing.T) {
	bundle, blobPath := bundleWithAttachmentOnBeta(t)
	dest, _ := testServerWithAttachments(t)
	rr := postBundle(dest, "RepeatWS", rewriteBundle(t, bundle, nil, blobPath))
	if rr.Code != http.StatusBadRequest || !bytes.Contains(rr.Body.Bytes(), []byte("more than once")) {
		t.Fatalf("repeated blob: got %d %s, want 400", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, dest, "RepeatWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
	// Control: the bundle as exported imports.
	ctl, _ := testServerWithAttachments(t)
	if rr := postBundle(ctl, "AsExportedWS", bundle); rr.Code != http.StatusCreated {
		t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
	}
}
