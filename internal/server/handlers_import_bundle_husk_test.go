package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3094: the bundle import's validation-reject rollback soft-deleted only,
// the fourth member of the class BUG-3087 closed on the cloud auto-create
// door. ListDeletedWorkspaces is scoped by owner_id, not membership, and
// ImportWorkspace writes no member row (the owner row lands only after
// success), so the rejected import sat in the importer's deleted-workspaces
// list for 30 days and restoring it returned a workspace the importer could
// neither read nor delete (measured on the trail: 403 on both, for a member;
// an admin reaches it through the admin bypass).
//
// What this door has that the cloud door did not: the rollback fires AFTER
// attachment blobs may have been rehydrated. The orphan GC discovers blobs
// through TOMBSTONED ROWS (OrphanedAttachments: deleted_at IS NOT NULL), and
// PurgeWorkspaceData hard-deletes those rows without touching bytes — so the
// shared removal, applied as-is, would have freed the husk and leaked the
// blob for good. removeUnusableWorkspace therefore now reclaims blobs before
// it purges, with the retention sweeper's own function and ordering, and on a
// refused reclaim stops after the soft delete: the tombstones plus the
// soft-deleted row are exactly what the sweeper consumes later.
//
// THE CONTROL. Leg (a) was run against the pre-fix helper: the blob was still
// on disk after the rollback and the husk was listed — red on both lines.
//
// Not parallel: these capture the process-global slog default.

// attachmentsServerOn is testServerWithAttachments on a chosen dialect, with
// no workspace pre-created and no cloud mode.
func attachmentsServerOn(t *testing.T, driver store.DriverType) *Server {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips when PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })
	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
	wireTestImageProcessor(srv)
	return srv
}

// realBundleWithBlob uploads one PNG to a scratch source server and exports it
// as a tar bundle, so the bundle carries a real blob for the import to
// rehydrate before the reject fires.
func realBundleWithBlob(t *testing.T) []byte {
	t.Helper()
	src, srcSlug := testServerWithAttachments(t)
	if rr := doMultipartUpload(src, srcSlug, "logo.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export?format=tar", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

// withDuplicateManifest appends a second attachments/manifest.json, which is
// the duplicate-manifest reject — one of the four *importStatusError sites
// that return the minted workspace and reach the rollback, and the one that
// fires after every blob ahead of it has been rehydrated.
func withDuplicateManifest(t *testing.T, realBundle []byte) []byte {
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
		if err := outTw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := outTw.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	dupe := []byte(`{"version":1,"entries":[]}`)
	if err := outTw.WriteHeader(&tar.Header{Name: "attachments/manifest.json", Mode: 0o644, Size: int64(len(dupe))}); err != nil {
		t.Fatalf("write dupe header: %v", err)
	}
	if _, err := outTw.Write(dupe); err != nil {
		t.Fatalf("write dupe: %v", err)
	}
	outTw.Close()
	outGz.Close()
	return out.Bytes()
}

// importAs posts a gzip bundle through the real door as the given session.
func importAs(srv *Server, name string, bundle []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name="+name, bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// memberImporter bootstraps an admin (so auth is on) and returns a PLAIN user
// with a session — the importer whose restored husk answered 403.
func memberImporter(t *testing.T, srv *Server) (*models.User, string) {
	t.Helper()
	_ = bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
	u := realUser(t, srv, "importer@pad.test")
	if u.Role == "admin" {
		t.Fatalf("importer must not be an admin (admin bypass would mask the husk), got role %q", u.Role)
	}
	tok, err := srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return u, tok
}

func attachmentRowsFor(t *testing.T, srv *Server, workspaceID string) (total, tombstoned int) {
	t.Helper()
	if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN deleted_at IS NOT NULL THEN 1 ELSE 0 END),0) FROM attachments WHERE workspace_id = ?`),
		workspaceID).Scan(&total, &tombstoned); err != nil {
		t.Fatalf("count attachment rows: %v", err)
	}
	return total, tombstoned
}

// --- (a) rejected import with a real blob: no husk, no rows, blob gone ------

func TestImportBundle_RejectRollback_LeavesNoHuskAndReclaimsBlobs_SQLite(t *testing.T) {
	rejectRollbackLeavesNoHuskAndReclaimsBlobs(t, store.DriverSQLite)
}

func TestImportBundle_RejectRollback_LeavesNoHuskAndReclaimsBlobs_Postgres(t *testing.T) {
	rejectRollbackLeavesNoHuskAndReclaimsBlobs(t, store.DriverPostgres)
}

func rejectRollbackLeavesNoHuskAndReclaimsBlobs(t *testing.T, driver store.DriverType) {
	bundle := withDuplicateManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)
	logs := captureLogs(t)

	rr := importAs(dest, "Husk", bundle, tok)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("expected the duplicate-manifest 400, got %d: %s", rr.Code, rr.Body.String())
	}

	if husks := restorableHusks(t, dest, u.ID); len(husks) != 0 {
		t.Errorf("rejected import is RESTORABLE by the importer (%d row(s) in ListDeletedWorkspaces); want none", len(husks))
	}
	if live, _ := dest.store.GetWorkspaceBySlug("Husk"); live != nil {
		t.Errorf("rejected import's workspace is still live")
	}
	var rows int
	if err := dest.store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&rows); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if rows != 0 {
		t.Errorf("rejected import left %d attachment row(s); want 0 (purged)", rows)
	}
	// The blob: the FS backend holds every key under one root, and the only
	// upload this server ever saw is the rehydrated one, so an empty backend
	// IS "the blob is gone". Walk it rather than trusting a key read from a
	// row that no longer exists.
	if keys := fsBackendKeys(t, dest); len(keys) != 0 {
		t.Errorf("blob(s) still on disk after the rollback: %v — the purge deleted the tombstoned rows the orphan GC needed, so nothing will ever reclaim these", keys)
	}
	if !strings.Contains(logs.String(), "import bundle") {
		t.Errorf("expected the door-labelled removal log, got: %s", logs.String())
	}
}

// fsBackendKeys lists every object the test's FS backend currently holds.
func fsBackendKeys(t *testing.T, srv *Server) []string {
	t.Helper()
	backend, err := srv.attachments.Resolve(attachments.FSPrefix + ":x")
	if err != nil {
		t.Fatalf("resolve fs backend: %v", err)
	}
	lister, ok := backend.(interface {
		ListBlobs(context.Context) ([]attachments.BlobInfo, error)
	})
	if !ok {
		t.Fatalf("fs backend does not implement ListBlobs")
	}
	blobs, err := lister.ListBlobs(context.Background())
	if err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	keys := make([]string, 0, len(blobs))
	for _, b := range blobs {
		keys = append(keys, b.Key)
	}
	return keys
}

// --- (b) reclaim refused: stop after the soft delete; the sweeper finishes ---

func TestImportBundle_RejectRollback_ReclaimRefusedLeavesSweepableHusk_SQLite(t *testing.T) {
	rejectRollbackReclaimRefused(t, store.DriverSQLite)
}

func TestImportBundle_RejectRollback_ReclaimRefusedLeavesSweepableHusk_Postgres(t *testing.T) {
	rejectRollbackReclaimRefused(t, store.DriverPostgres)
}

func rejectRollbackReclaimRefused(t *testing.T, driver store.DriverType) {
	bundle := withDuplicateManifest(t, realBundleWithBlob(t))
	dest := attachmentsServerOn(t, driver)
	u, tok := memberImporter(t, dest)
	logs := captureLogs(t)

	// Mark the blob's content hash as an upload in flight: reclaimWorkspaceBlobs
	// defers on exactly that condition (a Put done, row not yet inserted, for
	// the same bytes in another workspace), which is the refusal leg.
	sum := sha256.Sum256(realPNG())
	unmark := dest.markUploadInFlight(hex.EncodeToString(sum[:]))
	defer unmark()

	rr := importAs(dest, "Held", bundle, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	husks := restorableHusks(t, dest, u.ID)
	if len(husks) != 1 {
		t.Fatalf("refused reclaim must stop after the soft delete (today's posture), want 1 restorable row, got %d", len(husks))
	}
	total, tomb := attachmentRowsFor(t, dest, husks[0].ID)
	if total != 1 || tomb != 1 {
		t.Errorf("refusal leg must keep the tombstones the sweeper reads: rows total=%d tombstoned=%d, want 1/1", total, tomb)
	}
	if keys := fsBackendKeys(t, dest); len(keys) != 1 {
		t.Errorf("refusal leg must leave the blob for the sweeper, backend holds %d key(s)", len(keys))
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "restorable until") {
		t.Errorf("expected an ERROR naming the residual (restorable until the sweep), got: %s", logs.String())
	}

	// The sweeper finishes it once the upload is no longer in flight.
	unmark()
	res, err := dest.runWorkspacePurgeSweep(context.Background(), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Purged != 1 || res.BlobsReclaimed != 1 {
		t.Errorf("sweep should purge the held workspace and reclaim its blob, got purged=%d blobs=%d skipped=%d", res.Purged, res.BlobsReclaimed, res.Skipped)
	}
	if husks := restorableHusks(t, dest, u.ID); len(husks) != 0 {
		t.Errorf("husk still restorable after the sweep: %d", len(husks))
	}
	if keys := fsBackendKeys(t, dest); len(keys) != 0 {
		t.Errorf("blob still on disk after the sweep: %v", keys)
	}
}
