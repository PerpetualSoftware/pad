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
		"clock past 2^53":  liveFrame(100, 1<<53),
		"client past 2^53": liveFrame(1<<53, 1),
		// A client ID of 1 with a bit past 2^64 that a uint64 would drop.
		"overflowing varuint": {yMessageAwareness, 0x0f, 0x01, 0x81, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02, 0x01, 0x02, '{', '}'},
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

// A client that reconnects announces at a newer clock (the provider advances
// it on every connect) on its new connection before the old one has closed:
// ownership moves, so the old connection's close removes nobody (codex r1).
func TestAwarenessTrackerReconnectOverlapMovesOwnership(t *testing.T) {
	tr := newAwarenessTracker()
	tr.observe(1, liveFrame(100, 3)) // the old connection
	tr.observe(2, liveFrame(100, 4)) // the same client, reconnected
	if gone := tr.release(1); len(gone) != 0 {
		t.Fatalf("the old connection's close removed %+v; the client is live on the new one", gone)
	}
	if gone := tr.release(2); len(gone) != 1 || gone[0] != (awarenessEntry{clientID: 100, clock: 4, live: true}) {
		t.Fatalf("the new connection's close removed %+v; want client 100 at clock 4", gone)
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

// The interleaving codex r2 found: the old connection has released its client,
// and the reconnected client's first frame arrives before the old connection
// has published the removal. Peers must see the removal FIRST: a removal at
// the same clock arriving after the live state would be applied, hiding a
// live client.
func TestRoomRemovalCannotOvertakeAReconnectedClient(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	r := &Room{itemID: "item-race", bus: bus}
	watcher := bus.Subscribe("item-race")
	defer bus.Unsubscribe(watcher)

	oldConn := &roomConn{id: 1}
	newConn := &roomConn{id: 2}
	r.relayAwareness(oldConn, liveFrame(100, 3))
	<-watcher // the old connection's announcement

	inWindow := make(chan struct{})
	proceed := make(chan struct{})
	r.afterAwarenessRelease = func() { close(inWindow); <-proceed }
	done := make(chan struct{})
	go func() { r.releaseAwareness(oldConn); close(done) }()
	<-inWindow
	// The same client, reconnected, announces while the removal is pending.
	atLock := make(chan struct{})
	r.beforeAwarenessRelay = func() { close(atLock) }
	relayed := make(chan struct{})
	go func() { r.relayAwareness(newConn, liveFrame(100, 4)); close(relayed) }()
	<-atLock // the racing frame has reached the lock (codex r3)
	select {
	case <-relayed:
		t.Fatal("the reconnected client's frame was published while the removal was pending")
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	<-done
	<-relayed

	first := <-watcher
	second := <-watcher
	if !bytes.Equal(first.Data, encodeAwarenessRemoval([]awarenessEntry{{clientID: 100, clock: 3}})) {
		t.Fatalf("first frame %x; want the old connection's removal", first.Data)
	}
	if !bytes.Equal(second.Data, liveFrame(100, 4)) {
		t.Fatalf("second frame %x; want the reconnected client's live state, last", second.Data)
	}
}
