package collab

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
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
//
// TASK-4062: the counter has no scraper and every restart zeroes it, so its
// week is read from the log instead: each counted failure is also exactly one
// "collab: append op-log" line, and a clean append is none.
func TestTASK3574_AFailedAppendIsCounted(t *testing.T) {
	logs := captureCollabLogs(t)
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
	if got := logs.count(`msg="collab: append op-log"`); got != 0 {
		t.Fatalf("a clean append logged %d failure lines, want 0", got)
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
	if got := logs.count(`msg="collab: append op-log"`); got != 1 {
		t.Fatalf("a failed append logged %d failure lines, want 1 (one per count):\n%s", got, logs.String())
	}
	if !logs.has(`item_id=item-append-fail`) {
		t.Fatalf("the failure line does not name the item:\n%s", logs.String())
	}
}

// collabLogs is the default slog output while a test runs. Locked: the room
// logs from its connection goroutines.
type collabLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *collabLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *collabLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *collabLogs) count(sub string) int { return strings.Count(c.String(), sub) }

func (c *collabLogs) has(sub string) bool { return strings.Contains(c.String(), sub) }

func captureCollabLogs(t *testing.T) *collabLogs {
	t.Helper()
	c := &collabLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}
