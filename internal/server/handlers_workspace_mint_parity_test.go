package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2809: the two workspace-mint doors — POST /workspaces and
// POST /workspaces/import, the latter with a tar.gz body shape behind it —
// enforce the same preconditions, from one place.
//
// Two had already diverged and been fixed one at a time, each found by a
// reviewer rather than by the door that lacked it: the OAuth consent grant
// (IDEA-2756) and the user-scoped plan limit (BUG-2793). This file pins the
// remainder so a third cannot be found the same way.
//
// The empty-name leg is a live defect, not a symmetry argument. Measured on
// the unfixed tip: an import with no workspace name CREATED a workspace with
// `name="" slug=""`, and a second one landed on slug `"-2"` — the first had
// taken the empty slug, which is a routing key, globally.

func mintTestUser(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: "Mint", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func importJSON(t *testing.T, srv *Server, path string, export *models.WorkspaceExport, sessionToken string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithCookie(srv, "POST", path, export, sessionToken)
}

// THE LIVE DEFECT: an import with no effective name is refused, rather than
// minting a workspace whose slug is the empty string.
func TestImportWorkspace_EmptyNameIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-empty@example.com")
	tok := loginUser(t, srv, "mint-empty@example.com", "correct-horse-battery-staple")

	rr := importJSON(t, srv, "/api/v1/workspaces/import", &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "", Slug: ""},
	}, tok)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("import with an empty workspace name: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	// The CREATE door's own answer to the same input, so the two doors are
	// pinned to one message and not merely to one status.
	if got := errorCode(t, rr); got != "bad_request" {
		t.Errorf("error code = %q, want bad_request; body=%s", got, rr.Body.String())
	}
}

// CONTROL — the check is on the EFFECTIVE name. A bundle with no name is
// importable when ?name= supplies one, because that override is what
// becomes the slug. Without this leg the rule above is indistinguishable
// from "reject any bundle whose payload name is empty", which would break a
// legitimate rename-on-import.
func TestImportWorkspace_EmptyNameRescuedByOverride(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-override@example.com")
	tok := loginUser(t, srv, "mint-override@example.com", "correct-horse-battery-staple")

	rr := importJSON(t, srv, "/api/v1/workspaces/import?name=Renamed+On+Import", &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "", Slug: ""},
	}, tok)

	if rr.Code != http.StatusCreated {
		t.Fatalf("import with ?name= override: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if ws.Slug == "" {
		t.Fatalf("import produced an EMPTY SLUG despite the override; ws=%+v", ws)
	}
}

// Malformed settings answer 400 at both doors. The store normalizes too, so
// this never reached the database — but a store failure surfaces as 500
// `import_failed`, and the create door answers 400 for the same input. Same
// input, same answer, whichever door it arrives at.
func TestImportWorkspace_MalformedSettingsIs400NotA500(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-settings@example.com")
	tok := loginUser(t, srv, "mint-settings@example.com", "correct-horse-battery-staple")

	rr := importJSON(t, srv, "/api/v1/workspaces/import", &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "Bad Settings", Slug: "bad-settings", Settings: "not-json"},
	}, tok)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("import with malformed settings: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// An imported workspace is ATTRIBUTED, the same way a created one is
// (BUG-1557). It previously got no source at all, so the dashboard could not
// tell an imported workspace's origin from a web-created one's.
func TestImportWorkspace_AttributesTheCreationSurface(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-source@example.com")
	tok := loginUser(t, srv, "mint-source@example.com", "correct-horse-battery-staple")

	rr := importJSON(t, srv, "/api/v1/workspaces/import", &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "Attributed", Slug: "attributed"},
	}, tok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	stored, err := srv.store.GetWorkspaceBySlug(ws.Slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	// A cookie-session caller is "web" on the create door; the import door
	// now derives it from the same helper rather than leaving it empty.
	if stored.Source != "web" {
		t.Errorf("imported workspace source = %q, want %q — an import over a cookie session is a web mint", stored.Source, "web")
	}
}

// The BUNDLE door is the second body shape behind the same route and mints
// through the same store call, so it answers the same way — in its own
// envelope (`bad_bundle`), which is the reason the shared step returns an
// error instead of writing one.
func TestImportBundle_EmptyNameIsRefused(t *testing.T) {
	t.Parallel()
	srv, _ := testServerWithAttachments(t)
	mintTestUser(t, srv, "mint-bundle@example.com")
	tok := loginUser(t, srv, "mint-bundle@example.com", "correct-horse-battery-staple")

	export := models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "", Slug: ""},
	}
	payload, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write export: %v", err)
	}
	tw.Close()
	gzw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/import", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: tok})
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bundle import with an empty workspace name: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// CONTROL — the create door still refuses an empty name after the
// refactor. The rule moved into a shared function; a mutant that drops the
// call from the create side must not pass because the import side kept it.
func TestCreateWorkspace_EmptyNameStillRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-create@example.com")
	tok := loginUser(t, srv, "mint-create@example.com", "correct-horse-battery-staple")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]any{"name": ""}, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("create with an empty name: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// CONTROL — an ordinary import still works. Every refusal above is
// meaningless if the door refuses everything.
func TestImportWorkspace_OrdinaryImportStillWorks(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	mintTestUser(t, srv, "mint-ok@example.com")
	tok := loginUser(t, srv, "mint-ok@example.com", "correct-horse-battery-staple")

	rr := importJSON(t, srv, "/api/v1/workspaces/import", &models.WorkspaceExport{
		Version: 1, ExportedAt: "2026-09-07T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{Name: "Ordinary", Slug: "ordinary"},
	}, tok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("ordinary import: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}
