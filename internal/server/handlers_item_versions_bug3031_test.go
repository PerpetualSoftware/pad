package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3031: a version restore prunes the whole op-log and mints its undo point
// from items.content. Content-bearing op-log rows above the flush watermark are
// edits no row holds, so a restore that proceeds over them leaves them in no
// version at all. The restore now refuses with content_pending_flush unless the
// caller sends overwrite_pending_edits. These are the legs the lead's ruling
// names: pending → 409 with history intact; override → today's behaviour; no
// pending → unchanged; and the web initiator's own flush (BUG-2271) clears it.

func (f *restoreCollabFixture) restoreOverride(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(f.srv, "POST",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"/versions/"+f.v1VersionID+"/restore",
		map[string]any{"overwrite_pending_edits": true})
}

// seedPendingEdit leaves the item in the BUG-3000 state and proves it, so no leg
// below measures a clean item by accident.
func (f *restoreCollabFixture) seedPendingEdit(t *testing.T) int64 {
	t.Helper()
	id, err := f.srv.store.AppendYjsUpdate(f.itemID, pendingEditFrame, "1")
	if err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	got, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Fatalf("premise: seeded item must read pending; got %q", got.ContentState)
	}
	return id
}

func (f *restoreCollabFixture) versionCount(t *testing.T) int {
	t.Helper()
	got, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := f.srv.store.ListItemVersionsResolved(f.itemID, got.Content)
	if err != nil {
		t.Fatalf("ListItemVersionsResolved: %v", err)
	}
	return len(vs)
}

func (f *restoreCollabFixture) itemSeq(t *testing.T) int64 {
	t.Helper()
	got, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Seq
}

// assertRefusedIntact checks a refusal wrote nothing: body, seq, version count
// and the pending op-log row are all where they were.
func (f *restoreCollabFixture) assertRefusedIntact(t *testing.T, rr *httptest.ResponseRecorder, seqBefore int64, versionsBefore int, pendingID int64) {
	t.Helper()
	if rr.Code != http.StatusConflict {
		t.Fatalf("restore over pending edits: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	code, details := decodeErrorCode(t, rr)
	if code != "content_pending_flush" {
		t.Fatalf("code: want content_pending_flush, got %q", code)
	}
	if n, _ := details["pending_rows"].(float64); n != 1 {
		t.Fatalf("details.pending_rows: want 1, got %v", details["pending_rows"])
	}
	assertItemContent(t, f.srv, f.itemID, "v2")
	if got := f.itemSeq(t); got != seqBefore {
		t.Fatalf("seq moved on a refusal: %d → %d", seqBefore, got)
	}
	if got := f.versionCount(t); got != versionsBefore {
		t.Fatalf("version count moved on a refusal: %d → %d", versionsBefore, got)
	}
	maxID, ok, err := f.srv.store.MaxOpLogID(f.itemID)
	if err != nil || !ok || maxID != pendingID {
		t.Fatalf("the pending op-log row must survive a refusal: max=%d ok=%v err=%v (want %d)", maxID, ok, err, pendingID)
	}
}

func TestRestoreRefusesOverPendingEdits_NoRoom(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031NoRoom")
	pendingID := f.seedPendingEdit(t)
	seq, versions := f.itemSeq(t), f.versionCount(t)

	f.assertRefusedIntact(t, f.restore(t), seq, versions, pendingID)
}

// With a live room, a refusal must also leave the peers alone: no force_refresh
// (which would discard their document) and no stuck freeze (which would drop
// every later edit). The first conn proves the former; the override that follows
// proves the latter, because it can only succeed through an unfrozen room.
func TestRestoreRefusesOverPendingEdits_LiveRoom(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031LiveRoom")
	quiet := f.dialReady(t)
	later := f.dialReady(t)
	pendingID := f.seedPendingEdit(t)
	seq, versions := f.itemSeq(t), f.versionCount(t)

	f.assertRefusedIntact(t, f.restore(t), seq, versions, pendingID)

	_ = quiet.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := quiet.ReadMessage(); err == nil {
		t.Fatalf("a refused restore sent the peer a frame: %s", data)
	}

	rr := f.restoreOverride(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("override after a refusal: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	assertGotForceRefresh(t, later)
	assertItemContent(t, f.srv, f.itemID, "v1")
}

// The override is today's restore: the pending rows are pruned, the content is
// restored, and the undo point brackets the saved body.
func TestRestoreOverrideDiscardsPendingEdits(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031Override")
	f.seedPendingEdit(t)
	versions := f.versionCount(t)

	rr := f.restoreOverride(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("override: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
	assertOpLogEmpty(t, f.srv, f.itemID)
	if got := f.versionCount(t); got != versions+1 {
		t.Fatalf("override must mint the undo point: versions %d → %d", versions, got)
	}
}

// No pending edits: a bare POST restores exactly as before, with no body at all.
func TestRestoreWithoutPendingEditsIsUnchanged(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031Clean")
	rr := f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("clean restore: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
}

// Condition (1) of the ruling: the web initiator drains its own editor through
// a collab-snapshot flush before it restores (BUG-2271). That flush advances the
// watermark past the pending row, so the restore proceeds without the override,
// and its undo point holds what the flush saved.
func TestRestoreProceedsAfterInitiatorFlush(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031Flushed")
	pendingID := f.seedPendingEdit(t)

	rr := doRequest(f.srv, "PATCH",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"?source=collab-snapshot",
		map[string]any{"content": "v2 plus live typing", "op_log_cursor": pendingID})
	if rr.Code != http.StatusOK {
		t.Fatalf("flush: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	got, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentState != "" {
		t.Fatalf("premise: the flush must clear pending; content_state=%q", got.ContentState)
	}

	rr = f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore after the flush: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
	vs, err := f.srv.store.ListItemVersionsResolved(f.itemID, "v1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vs {
		if v.Content == "v2 plus live typing" {
			found = true
		}
	}
	if !found {
		t.Fatal("the flushed typing must be in history after the restore")
	}
}

func TestRestoreRejectsMalformedBody(t *testing.T) {
	f := newRestoreCollabFixture(t, "Bug3031BadBody")
	req := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"/versions/"+f.v1VersionID+"/restore",
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: want 400, got %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v2")
}
