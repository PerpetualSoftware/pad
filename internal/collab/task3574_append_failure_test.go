package collab

import (
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

type appendFailureCounter struct {
	resumeCounter
	fails atomic.Int32
}

func (o *appendFailureCounter) AppendFailed() { o.fails.Add(1) }

// TASK-3574: a failed op-log append reaches the manager's observer, once per
// failed batch, and a clean append does not. Driven through a real Join so the
// count vouches for the Room's binding to the manager, not only the callback
// (CONVE-19). The barrier ack orders each read after its frame's append.
func TestTASK3574_AFailedAppendIsCounted(t *testing.T) {
	fake := &fakeOpLog{}
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(fake, bus)
	obs := &appendFailureCounter{}
	mgr.SetObserver(obs)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	a := dialWS(t, srv, "item-append-fail")
	defer a.Close()
	initialCursor(t, a)

	if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0x01, 0x7f}); err != nil {
		t.Fatal(err)
	}
	sendBarrier(t, a, 1)
	readBarrierAck(t, a)
	if got := obs.fails.Load(); got != 0 {
		t.Fatalf("a clean append counted %d failures, want 0", got)
	}

	fake.mu.Lock()
	fake.failOn = "db down"
	fake.mu.Unlock()
	if err := a.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0x02, 0x02, 0x02, 0x7f}); err != nil {
		t.Fatal(err)
	}
	sendBarrier(t, a, 2)
	if ack := readBarrierAck(t, a); ack.OK == nil || *ack.OK {
		t.Fatalf("premise: the append did not fail (ack ok %s)", okString(ack.OK))
	}
	if got := obs.fails.Load(); got != 1 {
		t.Fatalf("a failed append counted %d failures, want 1", got)
	}
}
