package server

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244: the first join at a new schema version used to prune the item's
// whole op-log, unflushed content-bearing rows included, and the pending
// marker went with them, so the stale body read as current afterwards.
// Measured on main 3f75c2fe before the fix: rows 1 -> 0, content_state
// "applied_pending_flush" -> "".
//
// Each case runs the SAME fixture at the old version (the control: nothing
// may move) and at a bumped one, so an assertion that holds for both legs is
// not mistaken for evidence about the rebuild.

// A Sync Update frame with a payload: content-bearing under the BUG-3124
// classifier, the shape a tab's unflushed keystrokes are persisted in.
var bug3244Frame = []byte{0x00, 0x02, 0x03, 0x01, 0x02, 0x03}

type bug3244Fixture struct {
	srv    *Server
	bus    *collab.MemoryOpBus
	itemID string
}

func newBUG3244Fixture(t *testing.T) *bug3244Fixture {
	t.Helper()
	srv := testServer(t)
	bus := collab.NewMemoryOpBus()
	t.Cleanup(bus.Close)
	itemID := seedCollabFixture(t, srv, "Bug3244")
	body := "stored body"
	if _, err := srv.store.UpdateItem(itemID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	return &bug3244Fixture{srv: srv, bus: bus, itemID: itemID}
}

func (f *bug3244Fixture) append(t *testing.T, frame []byte, version string) int64 {
	t.Helper()
	id, err := f.srv.store.AppendYjsUpdate(f.itemID, frame, version)
	if err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	return id
}

// joinAt installs a RoomManager at serverVersion (the upgraded binary, or the
// same one for the control) and makes one collab join announcing it. It
// returns once the join has passed the rebuild: the rebuild runs under the
// item's setup lock before the room replays, so the op_log_cursor text frame
// the server sends after replay is the barrier.
func (f *bug3244Fixture) joinAt(t *testing.T, serverVersion string) {
	t.Helper()
	rm := collab.NewRoomManagerWithConfig(f.srv.store, f.bus, collab.RoomManagerConfig{SchemaVersion: serverVersion})
	t.Cleanup(rm.Close)
	f.srv.SetCollabRoomManager(rm)
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)

	u, _ := url.Parse(ts.URL)
	c, resp, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(
		"ws://"+u.Host+"/api/v1/collab/"+f.itemID+"?schema_version="+serverVersion, nil)
	if err != nil {
		st := ""
		if resp != nil {
			st = resp.Status
		}
		t.Fatalf("dial: %v %s", err, st)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, _, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("no op_log_cursor frame after join: %v", err)
		}
		if mt == websocket.TextMessage {
			return
		}
	}
}

func (f *bug3244Fixture) contentState(t *testing.T) string {
	t.Helper()
	it, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if it.Content != "stored body" {
		t.Fatalf("items.content changed: %q", it.Content)
	}
	return it.ContentState
}

func (f *bug3244Fixture) opLogRows(t *testing.T) int {
	t.Helper()
	rows, err := f.srv.store.LoadYjsUpdatesSince(f.itemID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	return len(rows)
}

func TestBUG3244SchemaRebuildKeepsUnflushedEditsMarked(t *testing.T) {
	t.Run("control: same version, nothing moves", func(t *testing.T) {
		f := newBUG3244Fixture(t)
		f.append(t, bug3244Frame, "1")
		f.joinAt(t, "1")
		if n := f.opLogRows(t); n != 1 {
			t.Fatalf("op-log rows = %d, want 1", n)
		}
		if got := f.contentState(t); got != models.ContentStatePendingFlush {
			t.Fatalf("content_state = %q, want %q", got, models.ContentStatePendingFlush)
		}
	})

	t.Run("bumped: the op-log is rebuilt and the item stays marked", func(t *testing.T) {
		f := newBUG3244Fixture(t)
		f.append(t, bug3244Frame, "1")
		f.joinAt(t, "2")
		if n := f.opLogRows(t); n != 0 {
			t.Fatalf("op-log rows = %d, want 0: old-era rows must never replay into a new-schema doc", n)
		}
		if got := f.contentState(t); got != models.ContentStateSetAside {
			t.Fatalf("content_state = %q, want %q: the unflushed edit is gone from the op-log "+
				"and the body is still stale, so it must not read as current", got, models.ContentStateSetAside)
		}
	})

	// Only edits the row never received are kept: a row at or below the flush
	// watermark is already in items.content, and a SyncStep1 frame cannot
	// change the document. What is kept is the frame byte for byte, with its
	// era and original op-log id, because those are what recovery needs.
	t.Run("bumped: only unflushed content-bearing rows are set aside, verbatim", func(t *testing.T) {
		f := newBUG3244Fixture(t)
		flushed := f.append(t, []byte{0x00, 0x02, 0x03, 0x09, 0x08, 0x07}, "1")
		if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(f.itemID, flushed); err != nil {
			t.Fatalf("set watermark: %v", err)
		}
		f.append(t, []byte{0x00, 0x00, 0x01, 0x00}, "1") // SyncStep1: never content
		kept := f.append(t, bug3244Frame, "1")
		f.joinAt(t, "2")

		rows, err := f.srv.store.ListYjsSetAside(f.itemID)
		if err != nil {
			t.Fatalf("ListYjsSetAside: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("set-aside rows = %d, want 1 (only the unflushed content-bearing frame): %+v", len(rows), rows)
		}
		r := rows[0]
		if r.OpLogID != kept || string(r.UpdateData) != string(bug3244Frame) || r.SchemaVersion != "1" {
			t.Fatalf("set-aside row = {op_log_id %d, data %v, era %q}, want {%d, %v, %q}",
				r.OpLogID, r.UpdateData, r.SchemaVersion, kept, bug3244Frame, "1")
		}
		if n := f.opLogRows(t); n != 0 {
			t.Fatalf("op-log rows = %d, want 0", n)
		}
	})

	// Nothing unflushed: the rebuild has nothing to keep, and the item must not
	// be marked, or every item on the instance would read stale after a bump.
	t.Run("bumped: a fully flushed item is not marked", func(t *testing.T) {
		f := newBUG3244Fixture(t)
		id := f.append(t, bug3244Frame, "1")
		if err := f.srv.store.SetItemContentFlushedOpLogIDForTesting(f.itemID, id); err != nil {
			t.Fatalf("set watermark: %v", err)
		}
		f.joinAt(t, "2")
		if got := f.contentState(t); got != "" {
			t.Fatalf("content_state = %q, want empty", got)
		}
		rows, err := f.srv.store.ListYjsSetAside(f.itemID)
		if err != nil || len(rows) != 0 {
			t.Fatalf("set-aside rows = %d (err %v), want 0", len(rows), err)
		}
	})
}
