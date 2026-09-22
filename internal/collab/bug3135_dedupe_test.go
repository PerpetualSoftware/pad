package collab

import (
	"testing"
	"time"
)

// BUG-3135: a byte-identical re-send is not stored, but the room must still
// broadcast it and acknowledge it exactly as it would a stored frame. The ack
// id is the item's MAX(id), and every row at or below it has already been
// delivered to each peer, so the cursor cannot run ahead of a binary.
func TestRoomBroadcastsAndAcksASkippedDuplicate(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-a")
	defer a.Close()
	b := dialWS(t, srv, "item-a")
	defer b.Close()
	_ = readControlWithin(t, a, time.Second)
	_ = readControlWithin(t, b, time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && bus.SubscriberCount("item-a") != 2 {
		time.Sleep(5 * time.Millisecond)
	}

	expectCursor := func(who string, _ any, got ControlMessage, want int64) {
		t.Helper()
		if got.Type != ControlMessageOpLogCursor || got.OpLogID != want {
			t.Fatalf("%s: want op_log_cursor id=%d, got type=%q id=%d", who, want, got.Type, got.OpLogID)
		}
	}

	// Row 1 from A, row 2 from B.
	sendSync(t, a, 0x42)
	expectCursor("A ack of row 1", a, readControlWithin(t, a, time.Second), 1)
	_ = readBinaryWithin(t, b, time.Second)
	expectCursor("B cursor for row 1", b, readControlWithin(t, b, time.Second), 1)
	sendSync(t, b, 0x43)
	expectCursor("B ack of row 2", b, readControlWithin(t, b, time.Second), 2)
	_ = readBinaryWithin(t, a, time.Second)
	expectCursor("A cursor for row 2", a, readControlWithin(t, a, time.Second), 2)

	// A re-sends row 1's exact bytes: not stored, still broadcast, still acked.
	sendSync(t, a, 0x42)
	expectCursor("A ack of the skipped duplicate", a, readControlWithin(t, a, time.Second), 2)
	gotB := readBinaryWithin(t, b, time.Second)
	if len(gotB) != 2 || gotB[1] != 0x42 {
		t.Fatalf("B must still receive the duplicate's bytes; got %v", gotB)
	}
	expectCursor("B cursor after the duplicate", b, readControlWithin(t, b, time.Second), 2)

	store.mu.Lock()
	rows := len(store.rows)
	store.mu.Unlock()
	if rows != 2 {
		t.Fatalf("op-log rows = %d, want 2: the duplicate must not be stored", rows)
	}

	// The skip must not advance the sender's durable high-water: the restore
	// finalization reads lastPersistedOpID as "this conn's frame is in the
	// op-log", and the duplicate is not. A persisted row 1 and B row 2, so the
	// two conns must still read 1 and 2 — a skip that advanced A would make both 2.
	mgr.mu.Lock()
	room := mgr.rooms["item-a"]
	mgr.mu.Unlock()
	room.mu.Lock()
	seen := map[int64]int{}
	for _, rc := range room.conns {
		seen[rc.lastPersistedOpID.Load()]++
	}
	room.mu.Unlock()
	if seen[1] != 1 || seen[2] != 1 {
		t.Fatalf("lastPersistedOpID per conn = %v, want one conn at 1 and one at 2", seen)
	}
}
