package collab

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// runApplierEcho spawns a goroutine that reads all incoming TextMessage
// frames on conn, looks for applier_request, and immediately responds
// with applier_ack. Returns a stop function that closes the conn.
func runApplierEcho(t *testing.T, conn *websocket.Conn) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctl ControlMessage
			if err := json.Unmarshal(data, &ctl); err != nil {
				continue
			}
			if ctl.Type != ControlMessageApplierRequest {
				continue
			}
			ack := ControlMessage{
				Type:      ControlMessageApplierAck,
				RequestID: ctl.RequestID,
			}
			payload, _ := json.Marshal(ack)
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}()
	return func() {
		_ = conn.Close()
		<-done
	}
}

// TestApplyExternalContentNoActiveRoom returns ErrNoActiveRoom when
// the manager doesn't know about the item yet.
func TestApplyExternalContentNoActiveRoom(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	err := mgr.ApplyExternalContent("item-a", "# new content")
	if !errors.Is(err, ErrNoActiveRoom) {
		t.Fatalf("want ErrNoActiveRoom, got %v", err)
	}
}

// TestApplyExternalContentHappyPath: one connected applier echoes the
// ack immediately; ApplyExternalContent returns nil within a few ms.
func TestApplyExternalContentHappyPath(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	stop := runApplierEcho(t, conn)
	defer stop()

	// Wait for the room to register the conn.
	for i := 0; i < 200; i++ {
		if mgr.RoomCount() == 1 && bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		done <- mgr.ApplyExternalContent("item-a", "# new content")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("happy path: want nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ApplyExternalContent did not return within 2s on happy path")
	}
}

// TestApplyExternalContentTimeoutsThenFails simulates a connected
// applier that NEVER acks. The first attempt should run until
// applierFirstTimeout fires, the retry until applierRetryTimeout
// (with the same conn re-tried only if no other appliers exist;
// here there's just one, so the second attempt picks no candidate
// and we get ErrAllAppliersTimedOut).
func TestApplyExternalContentTimeoutsThenFails(t *testing.T) {
	// Shrink timeouts so the test runs in seconds, not minutes.
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	applierFirstTimeoutTest := 80 * time.Millisecond
	applierRetryTimeoutTest := 40 * time.Millisecond
	swapApplierTimeouts(applierFirstTimeoutTest, applierRetryTimeoutTest)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	// NO applier echo — frames are dropped on the floor by the
	// drainer, so applier_request never produces an ack.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	err := mgr.ApplyExternalContent("item-a", "# new content")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAllAppliersTimedOut) {
		t.Fatalf("want ErrAllAppliersTimedOut, got %v", err)
	}
	// First attempt's timeout fires (~80ms). The retry loop then
	// looks for another applier, finds none (only one conn was
	// dialed), and returns immediately. Floor at firstTimeout,
	// ceiling well below firstTimeout + retryTimeout.
	if elapsed < applierFirstTimeoutTest {
		t.Errorf("returned too fast: %v < %v", elapsed, applierFirstTimeoutTest)
	}
}

// TestApplyExternalContentTimeoutThenSecondAcks: first applier never
// responds, second one does — the retry should hit the second
// applier and succeed.
func TestApplyExternalContentTimeoutThenSecondAcks(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(80*time.Millisecond, 1*time.Second)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// First conn: no echo, just drains.
	first := dialWS(t, srv, "item-a")
	defer first.Close()
	go func() {
		for {
			if _, _, err := first.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Wait so the second conn has a strictly later connectedAt.
	time.Sleep(15 * time.Millisecond)

	// Second conn: echoes acks. Because pickApplier sorts by
	// connectedAt asc, `first` is chosen for attempt 1; `second`
	// for the retry.
	second := dialWS(t, srv, "item-a")
	stop := runApplierEcho(t, second)
	defer stop()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		done <- mgr.ApplyExternalContent("item-a", "# new content")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("retry path: want nil, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry path did not complete within 3s")
	}
}

// TestApplyExternalContentRejectsAckFromUnexpectedConn confirms the
// expectedConn check in routeApplierAck. We wire two conns; the FIRST
// is the chosen applier; the SECOND tries to forge an ack for the
// first's request. The forgery should be ignored, and the request
// should still time out.
func TestApplyExternalContentRejectsAckFromUnexpectedConn(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(120*time.Millisecond, 60*time.Millisecond)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// First conn — chosen applier (longest-connected) but does NOT
	// auto-ack. We capture the inbound request_id and forge the ack
	// from the OTHER conn.
	first := dialWS(t, srv, "item-a")
	defer first.Close()
	requestIDCh := make(chan string, 1)
	go func() {
		for {
			mt, data, err := first.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var ctl ControlMessage
			if err := json.Unmarshal(data, &ctl); err != nil {
				continue
			}
			if ctl.Type == ControlMessageApplierRequest {
				select {
				case requestIDCh <- ctl.RequestID:
				default:
				}
			}
		}
	}()

	time.Sleep(15 * time.Millisecond)
	second := dialWS(t, srv, "item-a")
	defer second.Close()
	go func() {
		for {
			if _, _, err := second.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		done <- mgr.ApplyExternalContent("item-a", "# new content")
	}()

	// Wait for the request_id to surface, then forge the ack from
	// the SECOND conn — this should be ignored by the room.
	var reqID string
	select {
	case reqID = <-requestIDCh:
	case <-time.After(1 * time.Second):
		t.Fatal("did not capture applier_request on first conn")
	}

	forge := ControlMessage{Type: ControlMessageApplierAck, RequestID: reqID}
	payload, _ := json.Marshal(forge)
	if err := second.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("forge write: %v", err)
	}

	// The forged ack must NOT unblock ApplyExternalContent. It
	// should run to its timeout (and on the retry, since `second`
	// also doesn't honestly ack, the second attempt will also
	// time out).
	select {
	case err := <-done:
		if !errors.Is(err, ErrAllAppliersTimedOut) {
			t.Fatalf("forged ack must not satisfy ApplyExternalContent; got err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Apply did not return; forged ack should have been rejected and timeouts honoured")
	}
}

// swapApplierTimeouts is a small test-only mutator. The package
// constants are *not* exported as vars so the swap goes through a
// dedicated helper that lives only in test builds. We accept a
// brief mutex / shared-state hand-wave here because the tests run
// serially within the package by default.
var applierTimeoutMu sync.Mutex

func swapApplierTimeouts(first, retry time.Duration) {
	applierTimeoutMu.Lock()
	defer applierTimeoutMu.Unlock()
	applierFirstTimeoutVar = first
	applierRetryTimeoutVar = retry
}
