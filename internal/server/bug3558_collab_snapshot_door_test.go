package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func newBug3558AuthedFixture(t *testing.T) (bug3133Fixture, string, string) {
	t.Helper()
	f := newBug3133Fixture(t)
	ws, err := f.srv.store.GetWorkspaceBySlug(f.slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	user, err := f.srv.store.CreateUser(models.UserCreate{
		Email:    "editor@example.com",
		Name:     "Editor",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := f.srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "bug3558-pat",
		WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	sessTok, err := f.srv.store.CreateSession(user.ID, "cli-test", "127.0.0.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !strings.HasPrefix(sessTok, "padsess_") {
		t.Fatalf("session token %q does not start with padsess_", sessTok)
	}
	return f, tok.Token, sessTok
}

func readContentOpCursor(t *testing.T, srv *Server, itemID string) int64 {
	t.Helper()
	var cursor int64
	if err := srv.store.DB().QueryRow(`SELECT content_op_cursor FROM items WHERE id = ?`, itemID).Scan(&cursor); err != nil {
		t.Fatalf("read content_op_cursor: %v", err)
	}
	return cursor
}

// Case 1: PAT + flag + expected_seq, no cursor, pending op-log rows -> 409 content_pending_flush.
func TestBug3558_PATWithFlagAndPendingRowsRefused(t *testing.T) {
	t.Parallel()
	f, pat, _ := newBug3558AuthedFixture(t)
	f.seedPending(t)
	before, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}

	rr := doRequestWithBearer(f.srv, "PATCH", f.path+"?source=collab-snapshot", pat, map[string]any{
		"content":      "overwritten by PAT",
		"expected_seq": before.Seq,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 content_pending_flush, got %d: %s", rr.Code, rr.Body.String())
	}
	code, _ := decodeErrorCode(t, rr)
	if code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != before.Content {
		t.Fatalf("content changed from %q to %q on refused write", before.Content, after.Content)
	}
}

// Case 2: PAT + flag on a clean item -> 200, request-door version source (not collab-snapshot),
// warnings.unguarded_content_write true.
func TestBug3558_PATWithFlagOnCleanItemUsesDoorSourceAndWarns(t *testing.T) {
	t.Parallel()
	f, pat, _ := newBug3558AuthedFixture(t)
	initial := "initial body"
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{
		Content:        &initial,
		LastModifiedBy: "user",
		Source:         "web",
	}); err != nil {
		t.Fatalf("seed initial content: %v", err)
	}

	rr := doRequestWithBearer(f.srv, "PATCH", f.path+"?source=collab-snapshot", pat, map[string]any{
		"content": "pat body",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || !got.Warnings.UnguardedContentWrite {
		t.Fatalf("warnings = %+v, want unguarded_content_write=true", got.Warnings)
	}
	versions, err := f.srv.store.ListItemVersions(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatal("expected at least one version row")
	}
	if versions[0].Source == "collab-snapshot" {
		t.Fatalf("newest version source = %q, must not be collab-snapshot", versions[0].Source)
	}
	if versions[0].Source != "cli" {
		t.Fatalf("newest version source = %q, want cli (request door)", versions[0].Source)
	}
}

// Case 3: Case 1 with a padsess_ bearer token instead of a PAT.
func TestBug3558_SessionBearerWithFlagAndPendingRowsRefused(t *testing.T) {
	t.Parallel()
	f, _, sessTok := newBug3558AuthedFixture(t)
	f.seedPending(t)
	before, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}

	rr := doRequestWithBearer(f.srv, "PATCH", f.path+"?source=collab-snapshot", sessTok, map[string]any{
		"content":      "overwritten by session bearer",
		"expected_seq": before.Seq,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 content_pending_flush, got %d: %s", rr.Code, rr.Body.String())
	}
	code, _ := decodeErrorCode(t, rr)
	if code != "content_pending_flush" {
		t.Fatalf("code = %q, want content_pending_flush", code)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != before.Content {
		t.Fatalf("content changed from %q to %q on refused write", before.Content, after.Content)
	}
}

// Case 4: PAT + flag on an electable-applier fixture -> content_outcome applied_pending_flush,
// items.content unchanged.
func TestBug3558_PATWithFlagRoutesThroughElectableApplier(t *testing.T) {
	f, pat, sessTok := newBug3558AuthedFixture(t)
	initial := "before applier write"
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{
		Content:        &initial,
		LastModifiedBy: "user",
		Source:         "web",
	}); err != nil {
		t.Fatalf("seed initial content: %v", err)
	}

	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	conn, resp, err := dialCollab(t, ts.URL, f.item.ID, []*http.Cookie{{Name: "pad_session", Value: sessTok}}, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, f.srv, f.slug, f.item.Slug, f.item.ID)

	rr := doRequestWithBearer(f.srv, "PATCH", f.path+"?source=collab-snapshot", pat, map[string]any{
		"content": "from PAT via applier",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeWrite(t, rr)
	if got.Warnings == nil || got.Warnings.ContentOutcome != models.ContentOutcomeAppliedPendingFlush {
		t.Fatalf("warnings = %+v, want content_outcome=%q", got.Warnings, models.ContentOutcomeAppliedPendingFlush)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != initial {
		t.Fatalf("items.content = %q, want %q unchanged on applier path", after.Content, initial)
	}
}

// Case 5: Web door, pending rows, no cursor -> 409 stale_collab_snapshot, content and version count
// unchanged; then a request with a valid cursor at the same (and lower) client_write n succeeds.
func TestBug3558_WebDoorNoCursorRefusedWithoutMovingClientWriteMark(t *testing.T) {
	t.Parallel()
	f := newBug3133Fixture(t)
	f.seedPending(t)
	maxID, ok, err := f.srv.store.MaxOpLogID(f.item.ID)
	if err != nil || !ok {
		t.Fatalf("MaxOpLogID: maxID=%d ok=%v err=%v", maxID, ok, err)
	}
	before, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	versionsBefore, err := f.srv.store.ListItemVersions(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}

	rr := doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":      "cursorless flush",
		"client_write": map[string]any{"tab": "tab-1", "n": 5},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 stale_collab_snapshot, got %d: %s", rr.Code, rr.Body.String())
	}
	code, _ := decodeErrorCode(t, rr)
	if code != "stale_collab_snapshot" {
		t.Fatalf("code = %q, want stale_collab_snapshot", code)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != before.Content {
		t.Fatalf("content changed from %q to %q", before.Content, after.Content)
	}
	versionsAfter, err := f.srv.store.ListItemVersions(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versionsAfter) != len(versionsBefore) {
		t.Fatalf("version count changed %d -> %d", len(versionsBefore), len(versionsAfter))
	}

	// Prove no mark moved to 5: a lower n succeeds first, and the same n succeeds next.
	rr = doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "valid flush at n=4",
		"op_log_cursor": maxID,
		"client_write":  map[string]any{"tab": "tab-1", "n": 4},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("valid cursor at n=4: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "valid flush at n=5",
		"op_log_cursor": maxID,
		"client_write":  map[string]any{"tab": "tab-1", "n": 5},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("valid cursor at n=5: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	final, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Content != "valid flush at n=5" {
		t.Fatalf("content = %q, want %q", final.Content, "valid flush at n=5")
	}
}

// Case 6: Web door, cursor equal to MAX -> 200, content written.
func TestBug3558_WebDoorCursorEqualMaxSucceeds(t *testing.T) {
	t.Parallel()
	f := newBug3133Fixture(t)
	f.seedPending(t)
	maxID, ok, err := f.srv.store.MaxOpLogID(f.item.ID)
	if err != nil || !ok {
		t.Fatalf("MaxOpLogID: maxID=%d ok=%v err=%v", maxID, ok, err)
	}

	rr := doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "flushed at max",
		"op_log_cursor": maxID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != "flushed at max" {
		t.Fatalf("content = %q, want %q", after.Content, "flushed at max")
	}
}

// Case 7: Web door, cursor MAX+1000 -> 409 stale_collab_snapshot, content_op_cursor and content unchanged.
func TestBug3558_WebDoorCursorAboveMaxRefused(t *testing.T) {
	t.Parallel()
	f := newBug3133Fixture(t)
	f.seedPending(t)
	maxID, ok, err := f.srv.store.MaxOpLogID(f.item.ID)
	if err != nil || !ok {
		t.Fatalf("MaxOpLogID: maxID=%d ok=%v err=%v", maxID, ok, err)
	}
	before, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCursor := readContentOpCursor(t, f.srv, f.item.ID)

	rr := doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "forged ahead",
		"op_log_cursor": maxID + 1000,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409 stale_collab_snapshot, got %d: %s", rr.Code, rr.Body.String())
	}
	code, _ := decodeErrorCode(t, rr)
	if code != "stale_collab_snapshot" {
		t.Fatalf("code = %q, want stale_collab_snapshot", code)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != before.Content {
		t.Fatalf("content changed from %q to %q", before.Content, after.Content)
	}
	if afterCursor := readContentOpCursor(t, f.srv, f.item.ID); afterCursor != beforeCursor {
		t.Fatalf("content_op_cursor changed from %d to %d", beforeCursor, afterCursor)
	}
}

// Case 8: BUG-3744 delayed older snapshot over HTTP -> 409 superseded_write with no details.content_op_cursor.
func TestBug3558_DelayedOlderSnapshotOmitsCursorDetails(t *testing.T) {
	t.Parallel()
	f := newBug3133Fixture(t)
	id1, err := f.srv.store.AppendYjsUpdate(f.item.ID, []byte{0x00, 0x02, 0x02, 0x01, 0x01}, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate 1: %v", err)
	}
	id2, err := f.srv.store.AppendYjsUpdate(f.item.ID, []byte{0x00, 0x02, 0x02, 0x01, 0x02}, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate 2: %v", err)
	}

	rr := doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "newer body",
		"op_log_cursor": id2,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("newer snapshot: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(f.srv, "PATCH", f.path+"?source=collab-snapshot", map[string]any{
		"content":       "older body",
		"op_log_cursor": id1,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("older snapshot: want 409 superseded_write, got %d: %s", rr.Code, rr.Body.String())
	}
	code, details := decodeErrorCode(t, rr)
	if code != "superseded_write" {
		t.Fatalf("code = %q, want superseded_write", code)
	}
	if details != nil {
		if _, has := details["content_op_cursor"]; has {
			t.Fatalf("superseded_write response leaked details.content_op_cursor: %v", details)
		}
		t.Fatalf("superseded_write response should have no details map, got %v", details)
	}
	after, err := f.srv.store.GetItem(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Content != "newer body" {
		t.Fatalf("content = %q, want %q", after.Content, "newer body")
	}
}
