package collab

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-2103: a no-applier PruneAndApply replaces items.content and prunes the
// op-log while read-only peers stay attached. Their Y.Doc is then backed by
// nothing: they go on displaying the old body, and a viewer promoted before it
// reconnects would push that stale state back. After a SUCCESSFUL write every
// such peer is marked evicted (terminal, not `frozen`) under appendMu and
// force-refreshed outside it.

func bug2103Room(t *testing.T) (*RoomManager, *fakeOpLog, *httptest.Server) {
	t.Helper()
	bus := NewMemoryOpBus()
	t.Cleanup(func() { bus.Close() })
	op := &fakeOpLog{}
	mgr := NewRoomManager(op, bus)
	t.Cleanup(mgr.Close)
	srv := newCollabTestServer(t, mgr)
	t.Cleanup(srv.Close)
	return mgr, op, srv
}

// bug2103WaitConns waits until item's room holds n replay-done conns, so a
// frame the test sends is read by a registered readLoop.
func bug2103WaitConns(t *testing.T, mgr *RoomManager, itemID string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c := len(bug2103Conns(mgr, itemID)); c >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("room %s did not reach %d conns", itemID, n)
}

func bug2103Conns(mgr *RoomManager, itemID string) []*roomConn {
	mgr.mu.Lock()
	room := mgr.rooms[itemID]
	mgr.mu.Unlock()
	if room == nil {
		return nil
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	var out []*roomConn
	for _, rc := range room.conns {
		if rc.replayDone.Load() {
			out = append(out, rc)
		}
	}
	return out
}

// bug2103DrainForForceRefresh reads c until it closes or d elapses and reports
// whether a force_refresh control frame arrived. The initial op_log_cursor frame
// and any other traffic are skipped.
func bug2103DrainForForceRefresh(c *websocket.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return false
		}
		if mt != websocket.TextMessage {
			continue
		}
		var ctl ControlMessage
		if json.Unmarshal(data, &ctl) == nil && ctl.Type == ControlMessageForceRefresh {
			return true
		}
	}
}

// (a) A read-only peer is force-refreshed after a successful direct write, and
// every conn in the room at write time is marked evicted.
func TestPruneAndApplyRefreshesReadOnlyPeers(t *testing.T) {
	mgr, _, srv := bug2103Room(t)
	viewer := dialWSReadOnly(t, srv, "item-a")
	defer viewer.Close()
	bug2103WaitConns(t, mgr, "item-a", 1)
	rc := bug2103Conns(mgr, "item-a")[0]

	if err := mgr.PruneAndApply("item-a", func() error { return nil }); err != nil {
		t.Fatalf("PruneAndApply: %v", err)
	}
	if !rc.evicted.Load() {
		t.Fatal("the read-only conn was not marked evicted")
	}
	if !bug2103DrainForForceRefresh(viewer, 2*time.Second) {
		t.Fatal("the read-only peer was not force-refreshed after the direct write")
	}
}

// (b) The race the fence exists for: a viewer promoted after the writer scan,
// whose frame is already read and queued on appendMu while the write runs. It
// must not persist. The promotion is stored directly because SetConnWritable
// needs appendMu, which PruneAndApply holds here; that is exactly the ordering
// a real promotion landing just after the scan produces.
func TestPruneAndApplyEvictedPromotedViewerCannotPersist(t *testing.T) {
	mgr, op, srv := bug2103Room(t)
	viewer := dialWSReadOnly(t, srv, "item-a")
	defer viewer.Close()
	bug2103WaitConns(t, mgr, "item-a", 1)
	rc := bug2103Conns(mgr, "item-a")[0]

	err := mgr.PruneAndApply("item-a", func() error {
		rc.canWrite.Store(true)
		sendSync(t, viewer, 0xD3)
		// Let readLoop read the frame and queue on appendMu. There is no
		// observable for "blocked on the mutex"; the red-on-main run is what
		// shows this wait is long enough for the frame to be in flight.
		time.Sleep(150 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("PruneAndApply: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := op.rowCount(); n != 0 {
		t.Fatalf("an evicted conn persisted %d frame(s) after a promotion", n)
	}
}

// (c) A failed write rolled back, so the viewers are still current: nobody is
// evicted or refreshed.
func TestPruneAndApplyFailedWriteRefreshesNobody(t *testing.T) {
	mgr, _, srv := bug2103Room(t)
	viewer := dialWSReadOnly(t, srv, "item-a")
	defer viewer.Close()
	bug2103WaitConns(t, mgr, "item-a", 1)
	rc := bug2103Conns(mgr, "item-a")[0]

	boom := errors.New("write failed")
	if err := mgr.PruneAndApply("item-a", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("want the applyFn error back, got %v", err)
	}
	if rc.evicted.Load() {
		t.Fatal("a failed write evicted a viewer")
	}
	if bug2103DrainForForceRefresh(viewer, 300*time.Millisecond) {
		t.Fatal("a failed write force-refreshed a viewer")
	}
}

// (d) Control: a live writer still routes the caller to the applier path, and
// the viewer beside it is left alone.
func TestPruneAndApplyWriterPresentRefreshesNobody(t *testing.T) {
	mgr, _, srv := bug2103Room(t)
	writer := dialWS(t, srv, "item-a")
	defer writer.Close()
	viewer := dialWSReadOnly(t, srv, "item-a")
	defer viewer.Close()
	bug2103WaitConns(t, mgr, "item-a", 2)

	ran := false
	err := mgr.PruneAndApply("item-a", func() error { ran = true; return nil })
	if !errors.Is(err, ErrRoomActiveDuringPrune) || ran {
		t.Fatalf("a live writer must block the prune: err=%v ran=%v", err, ran)
	}
	for _, rc := range bug2103Conns(mgr, "item-a") {
		if rc.evicted.Load() {
			t.Fatal("a blocked prune evicted a conn")
		}
	}
	if bug2103DrainForForceRefresh(viewer, 300*time.Millisecond) {
		t.Fatal("a blocked prune force-refreshed the viewer")
	}
}

// (e) A version restore whose commit fails un-freezes every conn. It must not
// thaw an eviction, which is why eviction is its own flag: the conn stays
// unable to persist even after a promotion.
func TestRestoreRollbackDoesNotThawEviction(t *testing.T) {
	mgr, op, srv := bug2103Room(t)
	viewer := dialWSReadOnly(t, srv, "item-a")
	defer viewer.Close()
	bug2103WaitConns(t, mgr, "item-a", 1)
	rc := bug2103Conns(mgr, "item-a")[0]

	// The window between an eviction and its close, held open.
	rc.evicted.Store(true)

	commitErr := errors.New("commit rolled back")
	err := mgr.ForceRefreshRoom("item-a", func() (int64, int64, error) { return 0, 0, commitErr }, nil)
	if !errors.Is(err, commitErr) {
		t.Fatalf("want the commit error back, got %v", err)
	}
	if !rc.evicted.Load() {
		t.Fatal("a restore rollback thawed an evicted conn")
	}
	mgr.SetConnWritable("item-a", rc.conn, true)
	sendSync(t, viewer, 0xE5)
	time.Sleep(150 * time.Millisecond)
	if n := op.rowCount(); n != 0 {
		t.Fatalf("an evicted conn persisted %d frame(s) after a rollback and a promotion", n)
	}
}

// (f) An evicted conn is never elected applier, even promoted: its Y.Doc is
// not the document, so content applied through it would land on stale state.
func TestPickApplierSkipsEvictedConn(t *testing.T) {
	mgr, _, srv := bug2103Room(t)
	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)
	if !mgr.HasElectableApplier("item-a") {
		t.Fatal("premise: a live writer must be electable")
	}
	bug2103Conns(mgr, "item-a")[0].evicted.Store(true)
	if mgr.HasElectableApplier("item-a") {
		t.Fatal("an evicted conn was electable as applier")
	}
}
