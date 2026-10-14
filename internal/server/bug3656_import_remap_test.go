package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3655 / BUG-3656: after a bundle import every pad-attachment reference,
// in current content and in version history, names an attachment in the NEW
// workspace, including on the kept-partial path; and a failed remap is never
// reported as a 201.

var bundleRefRE = regexp.MustCompile(`pad-attachment:([0-9a-fA-F-]{36})`)

// withOversizedSecondAttachment adds a second manifest entry whose blob sorts
// after the golden's one and is over the cap the test sets, so the first
// attachment is rehydrated and the import then fails on the keep path.
func withOversizedSecondAttachment(t *testing.T, files map[string][]byte) map[string][]byte {
	t.Helper()
	var manifest models.AttachmentManifest
	if err := json.Unmarshal(files["attachments/manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 {
		t.Fatalf("golden manifest has %d entries, this test expects 1", len(manifest.Entries))
	}
	big := manifest.Entries[0]
	big.ID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	big.Filename = "big.png"
	big.ContentHash = strings.Repeat("f", 64)
	big.SizeBytes = 4096
	manifest.Entries = append(manifest.Entries, big)
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for k, v := range files {
		out[k] = v
	}
	out["attachments/manifest.json"] = b
	out[bundleAttachmentPath(big.ID, big.Filename)] = make([]byte, 4096)
	return out
}

// assertRefsAreLocal fails unless the workspace's item content and resolved
// version bodies carry at least one reference each, and every reference names
// an attachment row in this workspace.
func assertRefsAreLocal(t *testing.T, srv *Server, ws *models.Workspace) {
	t.Helper()
	items, err := srv.store.ListItems(ws.ID, models.ItemListParams{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	inContent, inHistory := 0, 0
	check := func(where, text string) int {
		n := 0
		for _, m := range bundleRefRE.FindAllStringSubmatch(text, -1) {
			n++
			att, err := srv.store.GetAttachment(m[1])
			if err != nil || att == nil || att.WorkspaceID != ws.ID {
				t.Errorf("%s references pad-attachment:%s, which is not an attachment in the imported workspace", where, m[1])
			}
		}
		return n
	}
	for _, it := range items {
		inContent += check("content of "+it.Title, it.Content)
		versions, err := srv.store.ListItemVersionsResolved(it.ID, it.Content)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			if strings.HasPrefix(v.Content, "[patch error") {
				t.Errorf("a version of %s does not resolve: %s", it.Title, v.Content)
			}
			inHistory += check("a version of "+it.Title, v.Content)
		}
	}
	if inContent == 0 || inHistory == 0 {
		t.Fatalf("expected references in content and in history, found %d and %d", inContent, inHistory)
	}
}

func TestBUG3656_KeptPartialImportRemapsReferences_SQLite(t *testing.T) {
	keptPartialImportRemaps(t, store.DriverSQLite)
}

func TestBUG3656_KeptPartialImportRemapsReferences_Postgres(t *testing.T) {
	keptPartialImportRemaps(t, store.DriverPostgres)
}

func keptPartialImportRemaps(t *testing.T, driver store.DriverType) {
	dest := attachmentsServerOn(t, driver)
	dest.attachmentMaxBytes = 1024 // the golden's blob fits, the added one does not
	rr := postBundle(dest, "PartialRemap", packInterchangeBundle(t, withOversizedSecondAttachment(t, readInterchangeGolden(t))))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "exceeds") {
		t.Fatalf("want the keep path's 400 on the oversized blob, got %d %s", rr.Code, rr.Body.String())
	}
	ws, err := dest.store.GetWorkspaceBySlug("PartialRemap")
	if err != nil || ws == nil {
		t.Fatalf("the partial workspace was not kept: %v %v", ws, err)
	}
	assertRefsAreLocal(t, dest, ws)
}

// The complete import, for both drivers: the golden's history carries a
// reference in a full snapshot and in an older reverse patch (BUG-3655).
func TestBUG3655_ImportRemapsVersionHistory_SQLite(t *testing.T) {
	importRemapsVersionHistory(t, store.DriverSQLite)
}

func TestBUG3655_ImportRemapsVersionHistory_Postgres(t *testing.T) {
	importRemapsVersionHistory(t, store.DriverPostgres)
}

func importRemapsVersionHistory(t *testing.T, driver store.DriverType) {
	dest := attachmentsServerOn(t, driver)
	rr := postBundle(dest, "FullRemap", packInterchangeBundle(t, readInterchangeGolden(t)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	ws, err := dest.store.GetWorkspaceBySlug("FullRemap")
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v %v", ws, err)
	}
	assertRefsAreLocal(t, dest, ws)
}

func TestBUG3656_RemapFailureIsNotA201(t *testing.T) {
	dest := attachmentsServerOn(t, store.DriverSQLite)
	dest.importBundleRemapFault = func() error { return errors.New("injected remap failure") }
	rr := postBundle(dest, "RemapFails", packInterchangeBundle(t, readInterchangeGolden(t)))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "attachment reference remap") {
		t.Fatalf("a failed remap must fail the import, got %d %s", rr.Code, rr.Body.String())
	}
	ws, err := dest.store.GetWorkspaceBySlug("RemapFails")
	if err != nil || ws == nil {
		t.Fatalf("the workspace should be kept, as on every keep-path error: %v %v", ws, err)
	}
	if got := partialImportCategory(errors.New("attachment reference remap: x")); !strings.Contains(got, "references could not be updated") {
		t.Errorf("partial marker category = %q", got)
	}
}
