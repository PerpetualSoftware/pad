package collab

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TASK-1273: a subscriber whose buffer is full when an op it needs arrives is
// signalled once and receives nothing after the gap, so no later op's cursor
// can carry the peer past the dropped row; the room closes its socket and its
// reconnect replays the gap. Awareness drops stay silent.

func fillSubscriber(b *MemoryOpBus, item string, typ string) {
	for i := 0; i < subscriberBufSize; i++ {
		b.Publish(OpEvent{ItemID: item, Type: typ, Data: []byte{byte(i)}, OpLogID: int64(i + 1)})
	}
}

func TestTASK1273_ASyncDropSignalsOnceAndEndsDelivery(t *testing.T) {
	b := NewMemoryOpBus()
	defer b.Close()
	var calls atomic.Int32
	signalled := make(chan struct{}, 4)
	ch := b.SubscribeWithOverflow("item", func() {
		calls.Add(1)
		signalled <- struct{}{}
	})
	fillSubscriber(b, "item", OpTypeSync)
	b.Publish(OpEvent{ItemID: "item", Type: OpTypeSync, OpLogID: 1000}) // the dropped op
	select {
	case <-signalled:
	case <-time.After(2 * time.Second):
		t.Fatal("no overflow signal for a dropped sync op")
	}
	// Room frees up, and later ops arrive: none may be delivered, or its
	// cursor would jump the peer past op 1000.
	for i := 0; i < 10; i++ {
		<-ch
	}
	b.Publish(OpEvent{ItemID: "item", Type: OpTypeSync, OpLogID: 1001})
	b.Publish(OpEvent{ItemID: "item", Type: OpTypeSync, OpLogID: 1002})
	got := 0
	for {
		select {
		case ev := <-ch:
			if ev.OpLogID > subscriberBufSize {
				t.Fatalf("op %d was delivered after the gap", ev.OpLogID)
			}
			got++
			continue
		default:
		}
		break
	}
	if got != subscriberBufSize-10 {
		t.Fatalf("drained %d queued ops, want the %d queued before the gap", got, subscriberBufSize-10)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("overflow signalled %d times, want once", n)
	}
}

func TestTASK1273_AnAwarenessDropIsSilentAndDeliveryContinues(t *testing.T) {
	b := NewMemoryOpBus()
	defer b.Close()
	var calls atomic.Int32
	ch := b.SubscribeWithOverflow("item", func() { calls.Add(1) })
	fillSubscriber(b, "item", OpTypeAwareness)
	b.Publish(OpEvent{ItemID: "item", Type: OpTypeAwareness}) // dropped, silently
	<-ch
	b.Publish(OpEvent{ItemID: "item", Type: OpTypeSync, OpLogID: 5000})
	// Bounded: a subscriber whose delivery stopped would block here forever.
	found := false
	for !found {
		select {
		case ev := <-ch:
			found = ev.OpLogID == 5000
		case <-time.After(2 * time.Second):
			t.Fatal("delivery stopped after an awareness drop")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("an awareness drop signalled overflow %d times", n)
	}
}

func TestTASK1273_OtherSubscribersKeepTheirOps(t *testing.T) {
	b := NewMemoryOpBus()
	defer b.Close()
	b.SubscribeWithOverflow("item", func() {})
	healthy := b.Subscribe("item")
	for i := 0; i < subscriberBufSize+5; i++ {
		b.Publish(OpEvent{ItemID: "item", Type: OpTypeSync, OpLogID: int64(i + 1)})
		<-healthy
	}
}

// recordingBus captures each subscription's overflow callback so a test can
// fire it, standing in for a buffer that filled.
type recordingBus struct {
	*MemoryOpBus
	mu        sync.Mutex
	overflows []func()
}

func (r *recordingBus) SubscribeWithOverflow(itemID string, onOverflow func()) chan OpEvent {
	r.mu.Lock()
	r.overflows = append(r.overflows, onOverflow)
	r.mu.Unlock()
	return r.MemoryOpBus.SubscribeWithOverflow(itemID, onOverflow)
}

type overflowCounter struct {
	resumeCounter
	closes atomic.Int32
}

func (o *overflowCounter) OverflowClosed() { o.closes.Add(1) }

type resumeCounter struct{}

func (resumeCounter) ResumeJoined()               {}
func (resumeCounter) ResumeForceRefreshed(string) {}

func TestTASK1273_TheRoomClosesAPeerTheBusOverflowed(t *testing.T) {
	bus := &recordingBus{MemoryOpBus: NewMemoryOpBus()}
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	obs := &overflowCounter{}
	mgr.SetObserver(obs)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	c := dialWS(t, srv, "item-overflow")
	defer c.Close()
	initialCursor(t, c)

	bus.mu.Lock()
	if len(bus.overflows) != 1 || bus.overflows[0] == nil {
		bus.mu.Unlock()
		t.Fatalf("the join subscribed %d overflow callbacks, want one", len(bus.overflows))
	}
	fire := bus.overflows[0]
	bus.mu.Unlock()
	fire()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		// A deadline would end this loop too; only a close counts.
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("the room did not close the overflowed peer")
		}
		break
	}
	if n := obs.closes.Load(); n != 1 {
		t.Fatalf("overflow closes counted %d, want 1", n)
	}
}

// End to end through a real overflow (codex): the peer's writeLoop is held
// on its write lock, the bus fills its buffer and drops the next op, and the
// room closes the socket, counted once.
func TestTASK1273_ARealOverflowClosesThePeerOnce(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	obs := &overflowCounter{}
	mgr.SetObserver(obs)
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	const item = "item-real-overflow"
	c := dialWS(t, srv, item)
	defer c.Close()
	initialCursor(t, c)

	mgr.mu.Lock()
	room := mgr.rooms[item]
	mgr.mu.Unlock()
	if room == nil {
		t.Fatal("no room for the joined item")
	}
	room.mu.Lock()
	var rc *roomConn
	for _, x := range room.conns {
		rc = x
	}
	room.mu.Unlock()
	if rc == nil {
		t.Fatal("no conn in the room")
	}

	// Stall the writeLoop: it takes one event and waits on writeMu.
	rc.writeMu.Lock()
	for i := 0; i < subscriberBufSize+10; i++ {
		bus.Publish(OpEvent{ItemID: item, Type: OpTypeSync, Data: []byte{yMessageSync, 2, 1, byte(i)}, OpLogID: int64(i + 1), ClientID: 999})
	}
	rc.writeMu.Unlock()

	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("a real overflow did not close the peer")
		}
		break
	}
	if n := obs.closes.Load(); n != 1 {
		t.Fatalf("overflow closes counted %d, want 1", n)
	}
}
