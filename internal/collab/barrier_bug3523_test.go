package collab

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// BUG-3523: a client's barrier is answered in readLoop order, after every
// sync frame it sent before the barrier, with OK false exactly when one of
// those appends failed since the previous barrier.

func sendBarrier(t *testing.T, c *websocket.Conn, n int64) {
	t.Helper()
	payload, _ := json.Marshal(ControlMessage{Type: ControlMessageBarrier, N: n})
	if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write barrier: %v", err)
	}
}

func readBarrierAck(t *testing.T, c *websocket.Conn) ControlMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg := readControlWithin(t, c, time.Until(deadline))
		if msg.Type == ControlMessageBarrierAck {
			return msg
		}
	}
	t.Fatal("no barrier_ack")
	return ControlMessage{}
}

// okString renders a barrier_ack's OK for a failure message.
func okString(ok *bool) string {
	if ok == nil {
		return "absent"
	}
	if *ok {
		return "true"
	}
	return "false"
}

func TestBUG3523_BarrierAckFollowsThePersistOfEarlierFrames(t *testing.T) {
	fake := &fakeOpLog{}
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(fake, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-barrier")
	defer a.Close()
	initialCursor(t, a)

	for i := 0; i < 5; i++ {
		if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, byte(i), 0x7f}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	sendBarrier(t, a, 1)
	ack := readBarrierAck(t, a)
	if ack.N != 1 || ack.OK == nil || !*ack.OK {
		t.Fatalf("ack = n %d ok %s, want n 1 ok true", ack.N, okString(ack.OK))
	}
	fake.mu.Lock()
	stored := len(fake.rows)
	fake.mu.Unlock()
	if stored != 5 {
		t.Fatalf("the ack arrived with %d of 5 frames stored", stored)
	}
}

func TestBUG3523_BarrierAckReportsAFailedAppendOnce(t *testing.T) {
	fake := &fakeOpLog{}
	bus := NewMemoryOpBus()
	defer bus.Close()
	srv := newCollabTestServer(t, NewRoomManager(fake, bus))
	defer srv.Close()

	a := dialWS(t, srv, "item-barrier-fail")
	defer a.Close()
	initialCursor(t, a)

	fake.mu.Lock()
	fake.failOn = "db down"
	fake.mu.Unlock()
	if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0x01, 0x7f}); err != nil {
		t.Fatal(err)
	}
	sendBarrier(t, a, 7)
	ack := readBarrierAck(t, a)
	if ack.N != 7 || ack.OK == nil || *ack.OK {
		t.Fatalf("ack after a failed append = n %d ok %s, want n 7 ok false", ack.N, okString(ack.OK))
	}

	fake.mu.Lock()
	fake.failOn = ""
	fake.mu.Unlock()
	if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0x02, 0x7f}); err != nil {
		t.Fatal(err)
	}
	sendBarrier(t, a, 8)
	ack = readBarrierAck(t, a)
	if ack.N != 8 || ack.OK == nil || !*ack.OK {
		t.Fatalf("ack after a clean append = n %d ok %s, want n 8 ok true", ack.N, okString(ack.OK))
	}
}
