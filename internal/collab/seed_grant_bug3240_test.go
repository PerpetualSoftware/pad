package collab

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3240: the room elects ONE seeder, the only peer allowed to run the
// client's lazy seed. Two tabs deciding by awareness both seeded a fresh
// doc (measured 10/20 on simultaneous opens), duplicating the stored body.

func initialCursor(t *testing.T, c *websocket.Conn) ControlMessage {
	t.Helper()
	msg := readControlWithin(t, c, time.Second)
	if msg.Type != ControlMessageOpLogCursor {
		t.Fatalf("first control frame: want %q, got %q", ControlMessageOpLogCursor, msg.Type)
	}
	return msg
}

func TestSeedGrantGoesToExactlyOneWritableConn(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(&fakeOpLog{}, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-seed")
	defer a.Close()
	if !initialCursor(t, a).Seed {
		t.Fatal("the first writable connection was not granted the seed")
	}
	b := dialWS(t, srv, "item-seed")
	defer b.Close()
	if initialCursor(t, b).Seed {
		t.Fatal("a second connection was granted the seed while the first holds it")
	}
}

func TestSeedGrantNeverGoesToReadOnlyConn(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(&fakeOpLog{}, bus))
	defer srv.Close()

	ro := dialWSReadOnly(t, srv, "item-ro")
	defer ro.Close()
	if initialCursor(t, ro).Seed {
		t.Fatal("a read-only connection was granted the seed")
	}
	w := dialWS(t, srv, "item-ro")
	defer w.Close()
	if !initialCursor(t, w).Seed {
		t.Fatal("the first WRITABLE connection was not granted the seed")
	}
}

// The seeder leaves: a live peer is granted, and the grant arrives AFTER
// the op the seeder sent before leaving, so the peer's "is my doc still
// empty" check sees that op first.
func TestSeedGrantPassesToLivePeerAfterSeedersLastOp(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(&fakeOpLog{}, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-handoff")
	if !initialCursor(t, a).Seed {
		t.Fatal("premise: a is the seeder")
	}
	b := dialWS(t, srv, "item-handoff")
	defer b.Close()
	if initialCursor(t, b).Seed {
		t.Fatal("premise: b is not the seeder")
	}

	op := []byte{yMessageSync, 0x02, 0x03, 0x0a, 0x0b, 0x0c}
	if err := a.WriteMessage(websocket.BinaryMessage, op); err != nil {
		t.Fatalf("a write: %v", err)
	}
	_ = a.Close()

	sawOp := false
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mt, data, err := b.ReadMessage()
		if err != nil {
			t.Fatalf("b never received seed_grant (saw op: %v): %v", sawOp, err)
		}
		if mt == websocket.BinaryMessage && bytes.Equal(data, op) {
			sawOp = true
			continue
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg ControlMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != ControlMessageSeedGrant {
			continue
		}
		if !sawOp {
			t.Fatal("seed_grant overtook the op the departed seeder sent before leaving")
		}
		return
	}
}

// The seeder leaves with nobody else in the room: the NEXT connection to
// set up is granted (the lead's condition 1).
func TestSeedGrantReleasedToNextConnWhenSeederLeavesAlone(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-release")
	if !initialCursor(t, a).Seed {
		t.Fatal("premise: a is the seeder")
	}
	_ = a.Close()
	// Wait for the server to process a's departure.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		room := mgr.rooms["item-release"]
		mgr.mu.Unlock()
		if room == nil {
			break
		}
		room.mu.Lock()
		n := len(room.conns)
		room.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	b := dialWS(t, srv, "item-release")
	defer b.Close()
	if !initialCursor(t, b).Seed {
		t.Fatal("the seed was not released when the seeder left")
	}
}
