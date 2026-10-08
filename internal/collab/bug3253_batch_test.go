package collab

import (
	"bytes"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3253: frames of a burst that are already waiting persist in one
// transaction, and every frame is still stored and relayed in order, each
// with its own op-log id. The first append is held so the rest of the burst
// queues behind it, which is the backlog the bug measured.
func TestBUG3253_WaitingFramesPersistAsOneBatchInOrder(t *testing.T) {
	gate := make(chan struct{})
	fake := &fakeOpLog{appendGate: gate}
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(fake, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-burst")
	defer a.Close()
	initialCursor(t, a)
	b := dialWS(t, srv, "item-burst")
	defer b.Close()
	initialCursor(t, b)

	const n = 50
	frame := func(i int) []byte { return []byte{yMessageSync, 0x02, 0x02, byte(i), 0x7f} }
	for i := 0; i < n; i++ {
		if err := a.WriteMessage(websocket.BinaryMessage, frame(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// Let the burst reach the server while the first append is held.
	time.Sleep(200 * time.Millisecond)
	close(gate)

	// The peer receives every frame, in the order typed.
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := 0
	for got < n {
		mt, data, err := b.ReadMessage()
		if err != nil {
			t.Fatalf("peer received %d of %d frames: %v", got, n, err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		if !bytes.Equal(data, frame(got)) {
			t.Fatalf("peer frame %d = %v, want %v", got, data, frame(got))
		}
		got++
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var rows [][]byte
	for _, r := range fake.rows {
		if r.ItemID == "item-burst" {
			rows = append(rows, r.UpdateData)
		}
	}
	if len(rows) != n {
		t.Fatalf("stored %d rows, want %d", len(rows), n)
	}
	for i, r := range rows {
		if !bytes.Equal(r, frame(i)) {
			t.Fatalf("row %d = %v, want frame %d", i, r, i)
		}
	}
	maxBatch := 0
	for _, s := range fake.batchSizes {
		if s > maxBatch {
			maxBatch = s
		}
	}
	if maxBatch < 2 {
		t.Fatalf("batch sizes %v: the queued frames were not persisted together", fake.batchSizes)
	}
	if len(fake.batchSizes) >= n {
		t.Fatalf("%d appends for %d frames: no batching", len(fake.batchSizes), n)
	}
}
