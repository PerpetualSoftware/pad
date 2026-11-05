package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// BUG-2709 (the parent filing of the husk class): the bundle import's
// mid-stream KEEP path. When a plain, non-validation error fires after
// pad-export.json has been imported — the manifest undecodable, a blob
// unreadable — the handler deliberately keeps the partial workspace
// (TASK-896) and answers 400 "workspace created but attachments not
// restored". But the owner row was only ever added on SUCCESS, so the kept
// workspace was live, ownerless, and invisible to its importer: absent from
// GET /workspaces, not restorable (never deleted), 403 on read and on delete,
// and holding its slug so a retry landed on `name-2` (measured on the trail).
// The message told the user a workspace existed that they could not find.
//
// The owner is now attached on that branch through addOwnerOrCompensate,
// which is FATAL: the partial workspace is either kept AND reachable, or —
// when the owner row cannot be written — removed by the compensation. The
// response says which, from a read of the row rather than from the helper's
// error text, so it is true for the branch taken.
//
// THE CONTROL: leg (a) against the pre-fix door was red on GetUserWorkspaces
// (0, want 1) and on the reads. TASK-896's rollback-or-partial design is not
// decided here; this makes the kept state reachable.
//
// Not parallel: leg (b) swaps a store seam and both read the process log.

// withUndecodableManifest replaces attachments/manifest.json's body with
// text that is not JSON, which fails manifest decode AFTER pad-export.json
// has been imported and BEFORE any blob — a plain error, not a validation
// reject, so it takes the keep path.
func withUndecodableManifest(t *testing.T, realBundle []byte) []byte {
	t.Helper()
	return withManifestBody(t, realBundle, []byte(`{not json`))
}

// withManifestBody replaces attachments/manifest.json's body with body.
func withManifestBody(t *testing.T, realBundle, replacement []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(realBundle))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	outGz := gzip.NewWriter(&out)
	outTw := tar.NewWriter(outGz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if hdr.Name == "attachments/manifest.json" {
			body = replacement
		}
		if err := outTw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := outTw.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	outTw.Close()
	outGz.Close()
	return out.Bytes()
}

// --- (a) kept partial is reachable by its importer --------------------------

func TestImportBundle_KeepPath_PartialWorkspaceIsReachable_SQLite(t *testing.T) {
	keepPathPartialIsReachable(t, store.DriverSQLite)
}

func TestImportBundle_KeepPath_PartialWorkspaceIsReachable_Postgres(t *testing.T) {
	keepPathPartialIsReachable(t, store.DriverPostgres)
}

func keepPathPartialIsReachable(t *testing.T, driver store.DriverType) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)

	rr := importAs(dest, "kept", bundle, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "manifest decode") {
		t.Fatalf("expected the keep path's 400 import_failed, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "kept") || !strings.Contains(rr.Body.String(), `\"kept\"`) {
		t.Errorf("the response must say the partial workspace was KEPT and name it, got: %s", rr.Body.String())
	}

	live, err := dest.store.GetWorkspaceBySlug("kept")
	if err != nil || live == nil {
		t.Fatalf("the keep path must keep the workspace (TASK-896): live=%v err=%v", live, err)
	}
	mine, err := dest.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != live.ID {
		t.Errorf("kept partial workspace is not in its importer's list (%d row(s)); an owner row must be attached on the keep path", len(mine))
	}
	if rr := doRequestWithCookie(dest, "GET", "/api/v1/workspaces/kept/collections", nil, tok); rr.Code != http.StatusOK {
		t.Errorf("importer cannot READ the kept partial workspace: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(dest, "DELETE", "/api/v1/workspaces/kept", nil, tok); rr.Code != http.StatusNoContent {
		t.Errorf("importer cannot DELETE the kept partial workspace: %d %s", rr.Code, rr.Body.String())
	}
}

// --- (b) owner row cannot be written: compensation removes, message says so -

func TestImportBundle_KeepPath_OwnerAddFailureRemovesThePartial_SQLite(t *testing.T) {
	keepPathOwnerAddFailureRemoves(t, store.DriverSQLite)
}

func TestImportBundle_KeepPath_OwnerAddFailureRemovesThePartial_Postgres(t *testing.T) {
	keepPathOwnerAddFailureRemoves(t, store.DriverPostgres)
}

func keepPathOwnerAddFailureRemoves(t *testing.T, driver store.DriverType) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)

	// The INSERT is rolled back and reported failed; the real membership
	// read answers ABSENT, so addOwnerOrCompensate removes the workspace.
	restore := dest.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)

	rr := importAs(dest, "gone", bundle, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "manifest decode") {
		t.Fatalf("expected 400 import_failed, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "removed") || strings.Contains(rr.Body.String(), "kept") {
		t.Errorf("the response must say the partial workspace was REMOVED, not kept, got: %s", rr.Body.String())
	}
	if live, _ := dest.store.GetWorkspaceBySlug("gone"); live != nil {
		t.Errorf("partial workspace is still live after the owner row could not be written")
	}
	if husks := restorableHusks(t, dest, u.ID); len(husks) != 0 {
		t.Errorf("removed partial workspace is restorable by the importer (%d row(s)); want none", len(husks))
	}
	if mine, _ := dest.store.GetUserWorkspaces(u.ID); len(mine) != 0 {
		t.Errorf("importer has %d workspace(s) after a removed partial; want 0", len(mine))
	}
}

// --- (c)/(d) helper KEEP arms: the row exists, ownership unconfirmed --------
//
// addOwnerOrCompensate keeps the workspace on two of its error arms — the
// membership read failed (an ack-lost commit may have landed the owner row),
// or a row exists with a non-owner role (which still permits reading). A read
// of the row cannot tell reachable from not, so the message must claim only
// existence and unconfirmed ownership.

func TestImportBundle_KeepPath_UnreadableMembershipSaysUnconfirmed(t *testing.T) {
	keepPathHelperKeepArm(t, "unread", func(string, string) (*models.WorkspaceMember, error) {
		return nil, errSimMemberAckLoss
	})
}

func TestImportBundle_KeepPath_WrongRoleMembershipSaysUnconfirmed(t *testing.T) {
	keepPathHelperKeepArm(t, "viewer", func(workspaceID, userID string) (*models.WorkspaceMember, error) {
		return &models.WorkspaceMember{WorkspaceID: workspaceID, UserID: userID, Role: "viewer"}, nil
	})
}

func keepPathHelperKeepArm(t *testing.T, name string, check func(string, string) (*models.WorkspaceMember, error)) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)

	restore := dest.store.SetAddWorkspaceMemberCommitHookForTesting(func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errSimMemberAckLoss
	})
	t.Cleanup(restore)
	dest.membershipCheck = check
	t.Cleanup(func() { dest.membershipCheck = nil })

	rr := importAs(dest, name, bundle, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "still exists") || !strings.Contains(body, "could not be confirmed") {
		t.Errorf("a KEEP arm must say the workspace exists and ownership is unconfirmed, got: %s", body)
	}
	if strings.Contains(body, "was removed") || strings.Contains(body, "was kept and is yours") || strings.Contains(body, "not reachable") {
		t.Errorf("message claims an outcome the read cannot establish: %s", body)
	}
	if live, _ := dest.store.GetWorkspaceBySlug(name); live == nil {
		t.Errorf("the helper's KEEP arm must leave the workspace live")
	}
}

// A NUL inside attachments/manifest.json takes the keep path, and the client
// body keeps the authored note and the remedy (--repair-nul, pad db
// repair-nul) without any wrapper text (TASK-4278).
func TestImportBundle_KeepPath_ManifestNULNamesTheRemedy(t *testing.T) {
	bundle := withManifestBody(t, realBundleWithBlob(t), []byte(`{"version":1,"attachments":[{"filename":"a`+textguard.EscNUL+`b"}]}`))
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)

	rr := importAs(dest, "nulmanifest", bundle, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, need := range []string{"--repair-nul", "pad db repair-nul", "attachments not restored"} {
		if !strings.Contains(body, need) {
			t.Errorf("body lost %q: %s", need, body)
		}
	}
	for _, leak := range []string{"manifest decode", "import workspace", "SQLSTATE", "Scan error"} {
		if strings.Contains(body, leak) {
			t.Errorf("body carries wrapper text %q: %s", leak, body)
		}
	}
}

// The manifest keep-path remedy must stay fixed text through the REAL
// call site. A manifest carrying a NUL and a repeated member, imported with
// the repair flag, makes the repair decline with a reason built from the
// document (the repeated member's name). That name must not reach the body.
// Reverting the manifest site to nulRepairRemedy(repair) fails this test.
func TestImportBundle_KeepPath_ManifestNULRemedyCarriesNoDocumentText(t *testing.T) {
	manifest := []byte(`{"version":1,"attachments":[{"filename":"a` + textguard.EscNUL + `b"}],"zzdocmember":1,"zzdocmember":2}`)
	bundle := withManifestBody(t, realBundleWithBlob(t), manifest)
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)

	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=nulrepairdecl&"+NULRepairQueryParam+"=1", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: tok})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	rr := httptest.NewRecorder()
	dest.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "zzdocmember") {
		t.Errorf("keep-path body carries document text: %s", body)
	}
	if !strings.Contains(body, "pad db repair-nul") {
		t.Errorf("body lost the remedy: %s", body)
	}
	// Proves the repair DID decline on this input (so the leak check above is
	// not vacuous) and that fixedTextRepairTally rewrote its reason.
	if !strings.Contains(body, "the manifest could not be repaired safely") {
		t.Errorf("the declined branch did not run or was not rewritten: %s", body)
	}
}
