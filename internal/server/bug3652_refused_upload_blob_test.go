package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3652: an upload refused at its row insert (the storage cap, TASK-3619)
// used to leave its blob on disk with no row, uncharged, until the rowless
// sweep's 30-day grace. A refused upload must leave no blob of its own
// behind, and must never take a blob that a live row shares.

func refusedBlobServer(t *testing.T) (*Server, *models.Workspace, string) {
	t.Helper()
	srv := testServer(t)
	srv.cloudMode = true
	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
	user, token := loginTestUser(t, srv)
	if _, err := srv.store.SetUserPlan(user.ID, store.PlanWrite{Plan: "pro", Source: store.PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetUserPlanOverrides(user.ID, `{"storage_bytes":40}`); err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Refused Blob", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	return srv, ws, token
}

func TestBUG3652_RefusedUploadLeavesNoBlob(t *testing.T) {
	srv, ws, token := refusedBlobServer(t)
	if rr := capUpload(srv, ws.Slug, token, []byte(strings.Repeat("a", 30))); rr.Code != http.StatusCreated {
		t.Fatalf("under the limit: %d %s", rr.Code, rr.Body.String())
	}
	if rr := capUpload(srv, ws.Slug, token, []byte(strings.Repeat("b", 20))); rr.Code != http.StatusForbidden {
		t.Fatalf("past the limit: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if keys := fsBackendKeys(t, srv); len(keys) != 1 {
		t.Fatalf("blobs on disk after a refused upload: %d %v, want 1 (the admitted file only)", len(keys), keys)
	}
}

// The refused bytes are identical to a file that already has a live row:
// Put deduplicated onto that blob, so it is not the refused upload's to
// delete, and the live file must still download.
func TestBUG3652_RefusedUploadKeepsASharedBlob(t *testing.T) {
	srv, ws, token := refusedBlobServer(t)
	same := []byte(strings.Repeat("s", 25))
	if rr := capUpload(srv, ws.Slug, token, same); rr.Code != http.StatusCreated {
		t.Fatalf("first upload: %d %s", rr.Code, rr.Body.String())
	}
	if rr := capUpload(srv, ws.Slug, token, same); rr.Code != http.StatusForbidden {
		t.Fatalf("same bytes past the limit: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if keys := fsBackendKeys(t, srv); len(keys) != 1 {
		t.Fatalf("blobs on disk: %d %v, want the shared blob kept", len(keys), keys)
	}
	// The live, non-derived rows. Not the export manifest query, which since
	// BUG-3654 runs only inside the export's snapshot transaction.
	atts, _, err := srv.store.WorkspaceAttachments(ws.ID, store.AttachmentListFilters{})
	if err != nil || len(atts) != 1 {
		t.Fatalf("live rows = %d (%v), want 1", len(atts), err)
	}
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/attachments/"+atts[0].ID, nil, token)
	if rr.Code != http.StatusOK || rr.Body.String() != string(same) {
		t.Fatalf("download of the live file: %d, %d bytes, want 200 and the file", rr.Code, rr.Body.Len())
	}
}

// A transform refused at the storage cap leaves no blob of its own. The
// limit is set to exactly what the workspace holds after the image and its
// thumbnails, so any transform output is past it.
func TestBUG3652_RefusedTransformLeavesNoBlob(t *testing.T) {
	srv, ws, token := refusedBlobServer(t)
	wireTestImageProcessor(srv)
	if err := srv.store.SetUserPlanOverrides(ws.OwnerID, `{"storage_bytes":100000000}`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "shot.png")
	part.Write(makeIntegrationPNG(t, 64, 32))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+ws.Slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("image upload: %d %s", rr.Code, rr.Body.String())
	}
	var up struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	srv.Stop() // drain thumbnail derivation, as the transform tests do

	used, err := srv.store.WorkspaceStorageUsage(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetUserPlanOverrides(ws.OwnerID, fmt.Sprintf(`{"storage_bytes":%d}`, used)); err != nil {
		t.Fatal(err)
	}
	before := fsBackendKeys(t, srv)

	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/attachments/"+up.ID+"/transform",
		map[string]any{"operation": "rotate", "degrees": 90}, token)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("transform past the limit: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if after := fsBackendKeys(t, srv); len(after) != len(before) {
		t.Fatalf("blobs on disk: %d before the refused transform, %d after", len(before), len(after))
	}
}

// Another writer is between its Put and its insert of the same bytes when
// this upload is refused: the blob is theirs too, so it stays.
func TestBUG3652_RefusedUploadKeepsABlobAnotherWriterHasInFlight(t *testing.T) {
	srv, ws, token := refusedBlobServer(t)
	if rr := capUpload(srv, ws.Slug, token, []byte(strings.Repeat("a", 30))); rr.Code != http.StatusCreated {
		t.Fatalf("under the limit: %d %s", rr.Code, rr.Body.String())
	}
	body := []byte(strings.Repeat("b", 20))
	release := srv.markUploadInFlight(sha256Hex(body))
	defer release()
	if rr := capUpload(srv, ws.Slug, token, body); rr.Code != http.StatusForbidden {
		t.Fatalf("past the limit: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if keys := fsBackendKeys(t, srv); len(keys) != 2 {
		t.Fatalf("blobs on disk: %d %v, want 2 (the in-flight writer's blob kept)", len(keys), keys)
	}
}

// A bundle import refused at the storage cap (BUG-3653) takes the blob it
// rehydrated with it: the importer is left with no workspace and no bytes.
func TestBUG3652_RefusedImportLeavesNoBlob(t *testing.T) {
	bundle := realBundleWithBlob(t)
	srv := attachmentsServerOn(t, store.DriverSQLite)
	srv.cloudMode = true
	user, tok := memberImporter(t, srv)
	capPlan(t, srv, user.ID, "free", 10)
	if rr := importAs(srv, "refused-import", bundle, tok); rr.Code != http.StatusForbidden {
		t.Fatalf("import past the limit: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if keys := fsBackendKeys(t, srv); len(keys) != 0 {
		t.Fatalf("blobs on disk after a refused import: %v, want none", keys)
	}
}
