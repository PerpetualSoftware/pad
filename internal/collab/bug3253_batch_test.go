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
	// Each keystroke is a sync update followed by the cursor's awareness
	// update, as the browser sends them.
	for i := 0; i < n; i++ {
		if err := a.WriteMessage(websocket.BinaryMessage, frame(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageAwareness, byte(i)}); err != nil {
			t.Fatalf("write awareness %d: %v", i, err)
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
		if mt != websocket.BinaryMessage || data[0] != yMessageSync {
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

// A run carries the awareness frame every keystroke sends behind its sync
// update (the first version stopped at it, so runs stayed one frame long,
// measured). The sync frames persist together; every frame publishes in
// arrival order; a refused run still relays its awareness.
func TestBUG3253_MixedRunPublishesInOrderAndRelaysAwarenessWhenRefused(t *testing.T) {
	sync1 := []byte{yMessageSync, 0x02, 0x02, 0x01, 0x7f}
	aw1 := []byte{yMessageAwareness, 0x01}
	sync2 := []byte{yMessageSync, 0x02, 0x02, 0x02, 0x7f}
	aw2 := []byte{yMessageAwareness, 0x02}
	run := [][]byte{sync1, aw1, sync2, aw2}

	setup := func(itemID string) (*Room, *roomConn, *fakeOpLog, chan OpEvent) {
		fake := &fakeOpLog{}
		bus := NewMemoryOpBus()
		t.Cleanup(func() { bus.Close() })
		ch := bus.Subscribe(itemID)
		r := &Room{itemID: itemID, store: fake, bus: bus, schemaVersion: "1"}
		rc := &roomConn{id: 7}
		return r, rc, fake, ch
	}
	drain := func(ch chan OpEvent, n int) []OpEvent {
		var out []OpEvent
		for len(out) < n {
			select {
			case ev := <-ch:
				out = append(out, ev)
			case <-time.After(2 * time.Second):
				t.Fatalf("got %d of %d events", len(out), n)
			}
		}
		return out
	}

	t.Run("writable", func(t *testing.T) {
		r, rc, fake, ch := setup("item-mixed")
		rc.canWrite.Store(true)
		r.persistSyncFrames(rc, run)
		evs := drain(ch, 4)
		wantType := []string{OpTypeSync, OpTypeAwareness, OpTypeSync, OpTypeAwareness}
		for i, ev := range evs {
			if ev.Type != wantType[i] || !bytes.Equal(ev.Data, run[i]) {
				t.Fatalf("event %d = %v %v, want %v %v", i, ev.Type, ev.Data, wantType[i], run[i])
			}
		}
		if evs[0].OpLogID == 0 || evs[2].OpLogID <= evs[0].OpLogID {
			t.Fatalf("sync ids %d, %d: each frame needs its own increasing id", evs[0].OpLogID, evs[2].OpLogID)
		}
		if len(fake.batchSizes) != 1 || fake.batchSizes[0] != 2 {
			t.Fatalf("batch sizes %v, want one batch of the 2 sync frames", fake.batchSizes)
		}
	})

	t.Run("read-only", func(t *testing.T) {
		r, rc, fake, ch := setup("item-ro")
		r.persistSyncFrames(rc, run) // canWrite false
		evs := drain(ch, 2)
		if !bytes.Equal(evs[0].Data, aw1) || !bytes.Equal(evs[1].Data, aw2) {
			t.Fatalf("relayed %v, want the two awareness frames in order", evs)
		}
		if len(fake.rows) != 0 {
			t.Fatalf("a read-only run stored %d rows", len(fake.rows))
		}
	})

	t.Run("frozen", func(t *testing.T) {
		r, rc, _, ch := setup("item-frozen")
		rc.canWrite.Store(true)
		rc.frozen.Store(true)
		r.persistSyncFrames(rc, run)
		drain(ch, 2)
		if got := rc.frozenDropSeq.Load(); got != 2 {
			t.Fatalf("frozenDropSeq = %d, want 2 (one per dropped sync frame)", got)
		}
	})
}
