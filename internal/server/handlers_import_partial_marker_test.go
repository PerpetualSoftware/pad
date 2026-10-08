package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-896: a bundle import KEPT after a data error past the point of no
// return carries a durable marker. The response that says "kept" is the only
// other record, and a client may never read it. The marker is surfaced on
// GET /workspaces/{slug} (with a SANITIZED note, to owners and admins only)
// and on the list (status only), cleared by an owner's PATCH, and absent on
// every rollback arm and on a clean import.

func TestImportPartialMarker_KeepPathMarks_SQLite(t *testing.T) {
	importPartialMarkerKeepPath(t, store.DriverSQLite)
}

func TestImportPartialMarker_KeepPathMarks_Postgres(t *testing.T) {
	importPartialMarkerKeepPath(t, store.DriverPostgres)
}

func decodeWorkspace(t *testing.T, body []byte) models.Workspace {
	t.Helper()
	var ws models.Workspace
	if err := json.Unmarshal(body, &ws); err != nil {
		t.Fatalf("decode workspace: %v (%s)", err, body)
	}
	return ws
}

func importPartialMarkerKeepPath(t *testing.T, driver store.DriverType) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, driver)
	_, tok := memberImporter(t, dest)

	rr := importAs(dest, "Kept", bundle, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "kept") {
		t.Fatalf("expected the keep path's 400, got %d: %s", rr.Code, rr.Body.String())
	}
	live, err := dest.store.GetWorkspaceBySlug("Kept")
	if err != nil || live == nil {
		t.Fatalf("kept workspace missing: %v %v", live, err)
	}

	st, err := dest.store.GetWorkspaceImportStatus(live.ID)
	if err != nil || st == nil {
		t.Fatalf("the keep path must record the partial-import marker: st=%v err=%v", st, err)
	}
	if st.Status != store.ImportStatusPartial {
		t.Errorf("status = %q, want %q", st.Status, store.ImportStatusPartial)
	}
	// Sanitized: the category and a correlation id, nothing of the raw error.
	if !strings.Contains(st.Note, "attachment manifest") || !strings.Contains(st.Note, "Reference imp-") {
		t.Errorf("note lacks its category or correlation id: %q", st.Note)
	}
	for _, raw := range []string{"{not json", "manifest decode", "invalid character"} {
		if strings.Contains(st.Note, raw) {
			t.Errorf("note carries raw error text %q: %q", raw, st.Note)
		}
	}

	// The owner reads the note on GET.
	rr = doRequestWithCookie(dest, "GET", "/api/v1/workspaces/Kept", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeWorkspace(t, rr.Body.Bytes())
	if got.ImportStatus == nil || got.ImportStatus.Status != "partial" || got.ImportStatus.Note != st.Note {
		t.Errorf("owner GET import_status = %+v, want status partial and the stored note", got.ImportStatus)
	}

	// The list carries the status, never the note.
	rr = doRequestWithCookie(dest, "GET", "/api/v1/workspaces", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var list []models.Workspace
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ImportStatus == nil || list[0].ImportStatus.Status != "partial" || list[0].ImportStatus.Note != "" {
		t.Errorf("list entry import_status = %+v, want status only", list)
	}

	// A non-owner member sees the status without the note, and cannot clear.
	editor := realUser(t, dest, "editor@pad.test")
	if err := dest.store.AddWorkspaceMember(live.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	etok, err := dest.store.CreateSession(editor.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rr = doRequestWithCookie(dest, "GET", "/api/v1/workspaces/Kept", nil, etok)
	if rr.Code != http.StatusOK {
		t.Fatalf("editor GET: %d %s", rr.Code, rr.Body.String())
	}
	if eg := decodeWorkspace(t, rr.Body.Bytes()); eg.ImportStatus == nil || eg.ImportStatus.Note != "" {
		t.Errorf("editor GET import_status = %+v, want the status without the note", eg.ImportStatus)
	}
	rr = doRequestWithCookie(dest, "PATCH", "/api/v1/workspaces/Kept", map[string]any{"clear_import_status": true}, etok)
	if rr.Code != http.StatusForbidden {
		t.Errorf("editor clear: %d, want 403", rr.Code)
	}
	if st, _ := dest.store.GetWorkspaceImportStatus(live.ID); st == nil {
		t.Fatalf("a refused clear removed the marker")
	}

	// The owner clears it.
	rr = doRequestWithCookie(dest, "PATCH", "/api/v1/workspaces/Kept", map[string]any{"clear_import_status": true}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner clear: %d %s", rr.Code, rr.Body.String())
	}
	if cg := decodeWorkspace(t, rr.Body.Bytes()); cg.ImportStatus != nil {
		t.Errorf("PATCH answer still carries import_status %+v", cg.ImportStatus)
	}
	if strings.Contains(rr.Body.String(), "import_status") {
		t.Errorf("a cleared marker must be absent from the answer (omitempty): %s", rr.Body.String())
	}
	if st, _ := dest.store.GetWorkspaceImportStatus(live.ID); st != nil {
		t.Errorf("marker survives the owner's clear: %+v", st)
	}
}

// A PATCH without the member leaves the marker alone.
func TestImportPartialMarker_OrdinaryPatchKeepsMarker(t *testing.T) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)
	if rr := importAs(dest, "Kept", bundle, tok); rr.Code != http.StatusBadRequest {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	rr := doRequestWithCookie(dest, "PATCH", "/api/v1/workspaces/Kept", map[string]any{"description": "x"}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeWorkspace(t, rr.Body.Bytes()); got.ImportStatus == nil {
		t.Errorf("an ordinary PATCH dropped the marker from its answer")
	}
}

// Rollback arms and a clean import leave no marker behind.
func TestImportPartialMarker_RollbackAndSuccessUnmarked(t *testing.T) {
	real := realBundleWithBlob(t)
	dest := attachmentsServerOn(t, store.DriverSQLite)
	u, tok := memberImporter(t, dest)

	if rr := importAs(dest, "Dup", withDuplicateManifest(t, real), tok); rr.Code == http.StatusCreated {
		t.Fatalf("duplicate manifest must be refused, got 201")
	}
	if rr := importAs(dest, "Clean", real, tok); rr.Code != http.StatusCreated {
		t.Fatalf("clean import: %d %s", rr.Code, rr.Body.String())
	}
	mine, err := dest.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	ids := []string{}
	for _, ws := range mine {
		ids = append(ids, ws.ID)
	}
	if dup, _ := dest.store.GetWorkspaceBySlug("Dup"); dup != nil {
		ids = append(ids, dup.ID)
	}
	marks, err := dest.store.ListWorkspaceImportStatuses(ids)
	if err != nil {
		t.Fatalf("ListWorkspaceImportStatuses: %v", err)
	}
	if len(marks) != 0 {
		t.Errorf("rollback or clean import left a marker: %+v", marks)
	}
	if rr := doRequestWithCookie(dest, "GET", "/api/v1/workspaces/Clean", nil, tok); strings.Contains(rr.Body.String(), "import_status") {
		t.Errorf("a clean import's GET carries import_status: %s", rr.Body.String())
	}
}

// The marker is instance-local: an export of a marked workspace does not
// carry it.
func TestImportPartialMarker_NotExported(t *testing.T) {
	bundle := withUndecodableManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, dest)
	if rr := importAs(dest, "Kept", bundle, tok); rr.Code != http.StatusBadRequest {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	rr := doRequestWithCookie(dest, "GET", "/api/v1/workspaces/Kept/export", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "import_status") || strings.Contains(rr.Body.String(), "Reference imp-") {
		t.Errorf("export carries the partial-import marker")
	}
}
