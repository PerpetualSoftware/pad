package server

import (
	"bytes"
	"context"
	"image"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4280: every store and backend call made while holding the process-wide
// in-flight lock must carry a deadline, because every upload takes that lock
// to mark its hash. The two paths TASK-4279 left unbounded are the rowless-blob
// sweep and the thumbnail refusal cleanup.

// deleteDeadlineSpy is an FS backend that records how long the context handed
// to each Delete may live. It keeps the Lister capability of the embedded store.
type deleteDeadlineSpy struct {
	*attachments.FSStore
	mu        sync.Mutex
	remaining []time.Duration // time left on each Delete's context; -1 = no deadline
}

func (s *deleteDeadlineSpy) Delete(ctx context.Context, key string) error {
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	s.mu.Lock()
	s.remaining = append(s.remaining, left)
	s.mu.Unlock()
	return s.FSStore.Delete(ctx, key)
}

func testServerWithDeleteSpy(t *testing.T) (*Server, string, *deleteDeadlineSpy) {
	t.Helper()
	srv := testServer(t)
	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	spy := &deleteDeadlineSpy{FSStore: fs}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, spy)
	srv.SetAttachments(reg, 0)
	wireTestImageProcessor(srv)
	return srv, createWSForTest(t, srv), spy
}

func requireBoundedDelete(t *testing.T, spy *deleteDeadlineSpy, what string) {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.remaining) == 0 {
		t.Fatalf("%s: the backend Delete never ran, so the bound was not exercised", what)
	}
	for _, left := range spy.remaining {
		if left < 0 {
			t.Errorf("%s: Delete ran under the in-flight lock with no deadline", what)
		} else if left > refusedBlobDeleteTimeout {
			t.Errorf("%s: Delete deadline %v exceeds the %v bound", what, left, refusedBlobDeleteTimeout)
		}
	}
}

func TestRowlessBlobSweep_DeleteUnderTheLockIsBounded(t *testing.T) {
	srv, _, spy := testServerWithDeleteSpy(t)
	_, key := putRowlessBlob(t, srv, []byte("rowless-bounded"))

	res, err := srv.runRowlessBlobSweep(context.Background(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rowless sweep: %v", err)
	}
	if res.Reclaimed != 1 {
		t.Fatalf("Reclaimed=%d, want 1 (the blob is rowless and past the cutoff)", res.Reclaimed)
	}
	requireBoundedDelete(t, spy, "rowless sweep")
	if err := statBlob(t, srv, key); err == nil {
		t.Error("rowless blob still on disk after a reclaim")
	}
}

// A row re-check that cannot answer under the lock keeps the blob for the next
// sweep; it must never fall through to the delete on an unknown.
func TestRowlessBlobSweep_KeepsBlobWhenTheRowRecheckCannotAnswer(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	hash, key := putRowlessBlob(t, srv, []byte("rowless-recheck-unanswered"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hookRan := false
	srv.rowlessPreDeleteHook = func(h string) {
		if h != hash {
			return
		}
		hookRan = true
		cancel()
	}
	defer func() { srv.rowlessPreDeleteHook = nil }()

	res, _ := srv.runRowlessBlobSweep(ctx, time.Now().Add(time.Hour))
	if !hookRan {
		t.Fatal("pre-delete hook never ran: the unanswered re-check leg was not exercised")
	}
	if res.Reclaimed != 0 {
		t.Errorf("Reclaimed=%d, want 0: the delete-time row re-check could not answer", res.Reclaimed)
	}
	if err := statBlob(t, srv, key); err != nil {
		t.Errorf("blob deleted although the delete-time row re-check failed: %v", err)
	}
}

func TestThumbnails_RefusalCleanupUnderTheLockIsBounded(t *testing.T) {
	srv, slug, spy := testServerWithDeleteSpy(t)
	if srv.imageProcessor == nil {
		t.Skip("no image processor in this build")
	}
	if rr := doMultipartUpload(srv, slug, "tiny.png", realPNG()); rr.Code != 201 {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	wsID := workspaceIDForSlug(t, srv, slug)
	parent, err := srv.store.GetAttachment(getOnlyAttachmentID(t, srv, wsID))
	if err != nil || parent == nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if err := srv.store.SoftDeleteAttachment(parent.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment: %v", err)
	}
	spy.mu.Lock()
	spy.remaining = nil // forget deletes made while setting up
	spy.mu.Unlock()

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if err := srv.persistThumbnail(context.Background(), parent, img, "thumb-md", "png"); err != nil {
		t.Fatalf("persistThumbnail should skip, not error: %v", err)
	}
	requireBoundedDelete(t, spy, "thumbnail refusal cleanup")

	// A bounded cleanup still cleans up: the refused thumbnail's blob is gone.
	var buf bytes.Buffer
	if err := srv.imageProcessor.Encode(img, "png", &buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	key := attachments.FSPrefix + ":" + sha256Hex(buf.Bytes())
	if err := statBlob(t, srv, key); err == nil {
		t.Error("refused thumbnail's blob still on disk")
	}
}

func TestSetOrphanGCConfig_FloorsTheGraceAndSaysSo(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	srv := testServer(t)
	srv.SetOrphanGCConfig(0, time.Minute)
	if got := srv.orphanGCGraceConfigured(); got != orphanGCRefStaleWindow {
		t.Fatalf("grace under the floor = %v, want %v", got, orphanGCRefStaleWindow)
	}
	out := logs.String()
	if !strings.Contains(out, "raised") || !strings.Contains(out, "1m0s") || !strings.Contains(out, "15m0s") {
		t.Errorf("warning must name the configured and the effective value, got %q", out)
	}

	// At and above the floor the value stands, silently.
	logs.Reset()
	srv.SetOrphanGCConfig(0, orphanGCRefStaleWindow)
	if got := srv.orphanGCGraceConfigured(); got != orphanGCRefStaleWindow {
		t.Errorf("grace at the floor = %v, want %v", got, orphanGCRefStaleWindow)
	}
	srv.SetOrphanGCConfig(0, 48*time.Hour)
	if got := srv.orphanGCGraceConfigured(); got != 48*time.Hour {
		t.Errorf("grace above the floor = %v, want 48h", got)
	}
	if logs.Len() != 0 {
		t.Errorf("no warning expected at or above the floor, got %q", logs.String())
	}
}

// Dev lead's cheap check on TASK-4280 part 2: a copy out of a soft-deleted
// workspace is refused, so a workspace purge (which runs only past its own
// grace) cannot overlap one.
func TestCopyItem_RefusesASoftDeletedSourceWorkspace(t *testing.T) {
	srv := testServer(t)
	var slugs []string
	for _, name := range []string{"CopySrc", "CopyDst"} {
		rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name})
		var w models.Workspace
		parseJSON(t, rr, &w)
		slugs = append(slugs, w.Slug)
	}
	src, _ := srv.store.GetWorkspaceBySlug(slugs[0])
	coll, _ := srv.store.GetCollectionBySlug(src.ID, "tasks")
	it, err := srv.store.CreateItem(src.ID, coll.ID, models.ItemCreate{Title: "x", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	body := `{"target_workspace":"` + slugs[1] + `","target_collection":"tasks"}`

	// Control: the copy works while the source is live.
	if rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/"+slugs[0]+"/items/"+it.Slug+"/copy", body); rr.Code != http.StatusCreated {
		t.Fatalf("control copy: %d %s", rr.Code, rr.Body.String())
	}

	softDeleteWorkspaceAt(t, srv, src.ID, time.Now())
	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/"+slugs[0]+"/items/"+it.Slug+"/copy", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("copy from a soft-deleted workspace: %d %s, want 404", rr.Code, rr.Body.String())
	}
}
