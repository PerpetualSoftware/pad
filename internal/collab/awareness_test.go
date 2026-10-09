package collab

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TASK-2206: awareness removal on disconnect. The fixtures are produced by
// y-protocols itself (web/node_modules, y-protocols/awareness + lib0): a client
// 3000000123 announcing {"user":{"name":"Ada","color":"#f00"},"cursor":null}
// at clock 1, and the frame removeAwarenessStates sends for it (clock 2, null).
const (
	fixtureClientID = 3000000123
	fixtureLive     = "013c01fbbcc1960b01347b2275736572223a7b226e616d65223a22416461222c22636f6c6f72223a2223663030227d2c22637572736f72223a6e756c6c7d"
	fixtureRemoval  = "010c01fbbcc1960b02046e756c6c"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAwarenessDecodesWhatYProtocolsWrites(t *testing.T) {
	got, err := decodeAwarenessFrame(mustHex(t, fixtureLive))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (awarenessEntry{clientID: fixtureClientID, clock: 1, live: true}) {
		t.Fatalf("live frame decoded as %+v", got)
	}
	got, err = decodeAwarenessFrame(mustHex(t, fixtureRemoval))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (awarenessEntry{clientID: fixtureClientID, clock: 2, live: false}) {
		t.Fatalf("removal frame decoded as %+v", got)
	}
}

// The synthesized removal is byte-for-byte the frame y-protocols' own
// removeAwarenessStates sends, so every client applies it.
func TestAwarenessRemovalIsWhatYProtocolsSends(t *testing.T) {
	got := encodeAwarenessRemoval([]awarenessEntry{{clientID: fixtureClientID, clock: 1, live: true}})
	if !bytes.Equal(got, mustHex(t, fixtureRemoval)) {
		t.Fatalf("removal frame\n got %x\nwant %s", got, fixtureRemoval)
	}
}

func TestAwarenessRefusesMalformedFrames(t *testing.T) {
	live := mustHex(t, fixtureLive)
	for name, frame := range map[string][]byte{
		"empty":            {},
		"sync type":        {yMessageSync, 0x00},
		"truncated length": live[:1],
		"truncated update": live[:len(live)-3],
		"overlong varuint": append([]byte{yMessageAwareness}, bytes.Repeat([]byte{0xff}, 11)...),
		"count too large":  {yMessageAwareness, 0x02, 0x7f, 0x00},
	} {
		if _, err := decodeAwarenessFrame(frame); err == nil {
			t.Errorf("%s: decoded a malformed frame", name)
		}
	}
}

func liveFrame(clientID, clock uint64) []byte {
	update := appendVarUint(nil, 1)
	update = appendVarUint(update, clientID)
	update = appendVarUint(update, clock)
	update = appendVarUint(update, 2)
	update = append(update, "{}"...)
	frame := appendVarUint(nil, yMessageAwareness)
	frame = appendVarUint(frame, uint64(len(update)))
	return append(frame, update...)
}

func TestAwarenessTrackerFirstSenderOwns(t *testing.T) {
	tr := newAwarenessTracker()
	tr.observe(1, liveFrame(100, 1)) // conn 1 announces client 100
	tr.observe(2, liveFrame(200, 1)) // conn 2 announces client 200
	tr.observe(2, liveFrame(100, 1)) // conn 2 ECHOES client 100 (pre-TASK-2206 provider)

	gone := tr.release(2)
	if len(gone) != 1 || gone[0].clientID != 200 {
		t.Fatalf("conn 2 leaving removed %+v; want only its own client 200, never the echoed 100", gone)
	}
	gone = tr.release(1)
	if len(gone) != 1 || gone[0] != (awarenessEntry{clientID: 100, clock: 1, live: true}) {
		t.Fatalf("conn 1 leaving removed %+v; want client 100 at its last clock", gone)
	}
}

func TestAwarenessTrackerKeepsLatestClockAndLiveness(t *testing.T) {
	tr := newAwarenessTracker()
	tr.observe(1, liveFrame(100, 5))
	tr.observe(1, liveFrame(100, 3)) // overtaken in flight
	if gone := tr.release(1); len(gone) != 1 || gone[0].clock != 5 {
		t.Fatalf("got %+v; want the latest clock, 5", gone)
	}

	// A client that already said it is gone needs no removal.
	tr.observe(1, liveFrame(100, 1))
	tr.observe(1, encodeAwarenessRemoval([]awarenessEntry{{clientID: 100, clock: 1}}))
	if gone := tr.release(1); len(gone) != 0 {
		t.Fatalf("got %+v; a client whose last state was null needs no removal", gone)
	}
}

func TestAwarenessTrackerIgnoresMalformedAndCapsOwnership(t *testing.T) {
	tr := newAwarenessTracker()
	tr.observe(1, []byte{yMessageAwareness, 0x05, 0x01})
	for id := uint64(0); id < maxOwnedPerConn+10; id++ {
		tr.observe(1, liveFrame(1000+id, 1))
	}
	if gone := tr.release(1); len(gone) != maxOwnedPerConn {
		t.Fatalf("owned %d; want the cap, %d", len(gone), maxOwnedPerConn)
	}
}

// readAwarenessWithin returns the next awareness frame c receives.
func readAwarenessWithin(t *testing.T, c *websocket.Conn, d time.Duration) ([]byte, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		c.SetReadDeadline(deadline)
		mt, data, err := c.ReadMessage()
		if err != nil {
			return nil, false
		}
		if mt == websocket.BinaryMessage && len(data) > 0 && data[0] == yMessageAwareness {
			return data, true
		}
	}
}

// The room: a hard-closed connection's client is removed from its peers at
// once, by the frame y-protocols would send; an echoing (old-behaviour) peer
// in the room does not take ownership of it, and its own departure removes
// only its own client.
func TestRoomBroadcastsAwarenessRemovalOnDisconnect(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(&fakeOpLog{}, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-aw")
	initialCursor(t, a)
	b := dialWS(t, srv, "item-aw")
	initialCursor(t, b)
	c := dialWS(t, srv, "item-aw")
	defer c.Close()
	initialCursor(t, c)

	aState := liveFrame(100, 3)
	if err := a.WriteMessage(websocket.BinaryMessage, aState); err != nil {
		t.Fatal(err)
	}
	if got, ok := readAwarenessWithin(t, b, 2*time.Second); !ok || !bytes.Equal(got, aState) {
		t.Fatalf("b did not get a's state relayed unchanged: %x", got)
	}
	if got, ok := readAwarenessWithin(t, c, 2*time.Second); !ok || !bytes.Equal(got, aState) {
		t.Fatalf("c did not get a's state relayed unchanged: %x", got)
	}
	// b runs the old provider: it echoes a's entry, then announces its own.
	if err := b.WriteMessage(websocket.BinaryMessage, aState); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteMessage(websocket.BinaryMessage, liveFrame(200, 1)); err != nil {
		t.Fatal(err)
	}
	readAwarenessWithin(t, c, 2*time.Second) // the echo
	readAwarenessWithin(t, c, 2*time.Second) // b's own

	// b leaves: c is told b's client is gone, and NOT a's.
	_ = b.Close()
	got, ok := readAwarenessWithin(t, c, 2*time.Second)
	if !ok {
		t.Fatal("c heard nothing when b left")
	}
	if want := encodeAwarenessRemoval([]awarenessEntry{{clientID: 200, clock: 1}}); !bytes.Equal(got, want) {
		t.Fatalf("b leaving sent %x; want the removal of b's client 200 only, %x", got, want)
	}

	// a hard-closes: c is told a's client is gone, at a's last clock + 1.
	_ = a.Close()
	got, ok = readAwarenessWithin(t, c, 2*time.Second)
	if !ok {
		t.Fatal("c heard nothing when a left")
	}
	if want := encodeAwarenessRemoval([]awarenessEntry{{clientID: 100, clock: 3}}); !bytes.Equal(got, want) {
		t.Fatalf("a leaving sent %x; want %x", got, want)
	}
}

// A frame that does not decode is still relayed, as before, and gives its
// sender nothing to remove.
func TestRoomRelaysMalformedAwarenessUntracked(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(&fakeOpLog{}, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-aw-bad")
	initialCursor(t, a)
	b := dialWS(t, srv, "item-aw-bad")
	defer b.Close()
	initialCursor(t, b)

	bad := []byte{yMessageAwareness, 0x05, 0x01}
	if err := a.WriteMessage(websocket.BinaryMessage, bad); err != nil {
		t.Fatal(err)
	}
	if got, ok := readAwarenessWithin(t, b, 2*time.Second); !ok || !bytes.Equal(got, bad) {
		t.Fatalf("a malformed frame was not relayed unchanged: %x", got)
	}
	_ = a.Close()
	if got, ok := readAwarenessWithin(t, b, 500*time.Millisecond); ok {
		t.Fatalf("a's departure sent %x; it owned no client", got)
	}
}
