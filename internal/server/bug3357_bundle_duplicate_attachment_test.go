package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3357, bundle side: pad-attachment references are remapped by
// attachment id, so a manifest listing one id twice (under two filenames)
// would rehydrate both and point every reference at whichever came last.
// The bundle is refused, and the workspace its pad-export.json minted is
// rolled back. The duplicate source-id check on collections and items lives
// in the store (internal/store/bug3357_import_duplicate_ids_test.go) and
// covers both import doors.
func TestBUG3357_BundleDuplicateAttachmentIDRefused(t *testing.T) {
	manifest := func(ids ...string) []byte {
		m := models.AttachmentManifest{Version: 1}
		for i, id := range ids {
			m.Entries = append(m.Entries, models.AttachmentManifestEntry{
				ID: id, Filename: []string{"a.png", "b.png", "c.png"}[i], MIME: "image/png",
			})
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	const id = "11111111-2222-3333-4444-555555555555"
	export := exportJSONFrom(t)

	srv, _ := testServerWithAttachments(t)
	rr := postBundle(srv, "DupAttWS", gzipTar(t, []bundleEntry{
		{"pad-export.json", export},
		{"attachments/manifest.json", manifest(id, id)},
	}))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "duplicate attachment id") {
		t.Fatalf("duplicate attachment id: got %d %s, want 400 naming the duplicate", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, srv, "DupAttWS") {
		t.Fatal("the partial workspace was not rolled back")
	}

	// Control: distinct ids import.
	ctl, _ := testServerWithAttachments(t)
	if rr := postBundle(ctl, "DistinctAttWS", gzipTar(t, []bundleEntry{
		{"pad-export.json", export},
		{"attachments/manifest.json", manifest(id, "66666666-7777-8888-9999-000000000000")},
	})); rr.Code != http.StatusCreated {
		t.Fatalf("distinct attachment ids: got %d %s", rr.Code, rr.Body.String())
	}
}
