package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244 x BUG-3240: after a bumped rebuild sets the unflushed rows aside,
// the op-log is empty, so the room's elector grants the next writable open
// `seed: true` and that tab seeds its empty doc from items.content, the body
// WITHOUT the set-aside edits. That is intended (the editor has to show the
// stored body; the set-aside rows are kept for TASK-3246), but the seed must
// neither clear the set-aside rows nor the state that names them, and a second
// open must be neither granted nor rebuilt again.
func TestBUG3244RebuildThenSeedGrantKeepsTheItemSuperseded(t *testing.T) {
	f := newBUG3244Fixture(t)
	f.append(t, bug3244Frame, "1")

	rm := collab.NewRoomManagerWithConfig(f.srv.store, f.bus, collab.RoomManagerConfig{SchemaVersion: "2"})
	t.Cleanup(rm.Close)
	f.srv.SetCollabRoomManager(rm)
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)

	dial := func() (*websocket.Conn, collab.ControlMessage) {
		t.Helper()
		c, resp, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(
			"ws://"+u.Host+"/api/v1/collab/"+f.itemID+"?schema_version=2", nil)
		if err != nil {
			st := ""
			if resp != nil {
				st = resp.Status
			}
			t.Fatalf("dial: %v %s", err, st)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("no op_log_cursor frame after join: %v", err)
			}
			if mt != websocket.TextMessage {
				continue
			}
			var msg collab.ControlMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal control frame: %v", err)
			}
			if msg.Type == collab.ControlMessageOpLogCursor {
				_ = c.SetReadDeadline(time.Time{})
				return c, msg
			}
		}
	}
	setAside := func() int {
		t.Helper()
		rows, err := f.srv.store.ListYjsSetAside(f.itemID)
		if err != nil {
			t.Fatalf("ListYjsSetAside: %v", err)
		}
		return len(rows)
	}

	// The first open at the bumped version rebuilds, and is the seeder.
	a, cursor := dial()
	defer a.Close()
	if !cursor.Seed {
		t.Fatal("the first open after the rebuild was not granted the seed")
	}
	if n := setAside(); n != 1 {
		t.Fatalf("set-aside rows after the rebuild = %d, want 1", n)
	}
	if got := f.contentState(t); got != models.ContentStateSetAside {
		t.Fatalf("content_state after the rebuild = %q, want %q", got, models.ContentStateSetAside)
	}

	// It seeds: its first update is a content-bearing frame at the new version.
	seed := []byte{0x00, 0x02, 0x03, 0x07, 0x08, 0x09}
	if err := a.WriteMessage(websocket.BinaryMessage, seed); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.opLogRows(t) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the seed was not persisted: op-log rows = %d", f.opLogRows(t))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A second open while the seeder is live: not granted, and no second
	// rebuild (the op-log now holds a current-version row).
	b, cursor := dial()
	defer b.Close()
	if cursor.Seed {
		t.Fatal("a second open was granted the seed while the seeder is live")
	}
	if n := f.opLogRows(t); n != 1 {
		t.Fatalf("op-log rows after the second open = %d, want 1 (the seed; no second rebuild)", n)
	}
	if n := setAside(); n != 1 {
		t.Fatalf("set-aside rows after the seed and a second open = %d, want 1", n)
	}
	if got := f.contentState(t); got != models.ContentStateSetAside {
		t.Fatalf("content_state after the seed = %q, want %q (set-aside wins over pending)", got, models.ContentStateSetAside)
	}
}
