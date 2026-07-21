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

// TestApplyExternalContentCleansPendingAcksOnSuccess ensures the
// per-room pendingAcks map doesn't leak entries across successful
// applies — without the cancel-on-success cleanup, every external
// update would retain a request_id + channel + conn pointer for the
// rest of the room's lifetime.
func TestApplyExternalContentCleansPendingAcksOnSuccess(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	stop := runApplierEcho(t, conn)
	defer stop()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	for i := 0; i < 5; i++ {
		if err := mgr.ApplyExternalContent("item-a", "# rev"); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	// Drill into the room to count pending entries — this is a
	// white-box assertion against the cleanup contract.
	mgr.mu.Lock()
	room := mgr.rooms["item-a"]
	mgr.mu.Unlock()
	if room == nil {
		t.Fatal("room missing after applies")
	}
	room.pendingMu.Lock()
	left := len(room.pendingAcks)
	room.pendingMu.Unlock()
	if left != 0 {
		t.Fatalf("pendingAcks leaked across applies: %d entries left", left)
	}
}

// TestApplyExternalContentSendsExpiresAt confirms the server stamps
// the applier_request with an expires_at_millis. Defends against
// a regression where the field is silently dropped — the browser
// (TASK-1263) needs it to refuse late-arriving requests.
func TestApplyExternalContentSendsExpiresAt(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()

	captured := make(chan ControlMessage, 1)
	go func() {
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
			if ctl.Type == ControlMessageApplierRequest {
				select {
				case captured <- ctl:
				default:
				}
				// Honest ack so ApplyExternalContent returns.
				ack := ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID}
				payload, _ := json.Marshal(ack)
				_ = conn.WriteMessage(websocket.TextMessage, payload)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := mgr.ApplyExternalContent("item-a", "# new"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	select {
	case msg := <-captured:
		if msg.ExpiresAtMillis == 0 {
			t.Errorf("expires_at_millis must be set on applier_request")
		}
		// Sanity: must be in the future.
		if msg.ExpiresAtMillis < time.Now().UnixMilli() {
			t.Errorf("expires_at_millis must be in the future, got %d (now=%d)",
				msg.ExpiresAtMillis, time.Now().UnixMilli())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not capture applier_request")
	}
}

// TestApplyExternalContentRejectsAckFromUnexpectedConn confirms the
// expectedConn check in resolveApplierAck. We wire two conns; the FIRST
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

// TestApplyExternalContentSkipsReadOnlyApplier is a TASK-265
// regression: a read-only participant must NEVER be elected applier.
// If it were, it would ack a no-op — its resulting Y.Doc sync frames
// are dropped by the read-only gate — and ApplyExternalContent would
// falsely return nil, making the PATCH handler skip its direct-write
// fallback and silently lose the external edit. With the only conn
// read-only, pickApplier finds no candidate → ErrNoApplierAvailable,
// so the caller falls back to a direct write (no loss). The conn even
// runs an honest ack-echo to prove that a *willing* but unauthorized
// applier is still never asked.
func TestApplyExternalContentSkipsReadOnlyApplier(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSReadOnly(t, srv, "item-a")
	stop := runApplierEcho(t, conn)
	defer stop()

	// Wait for the room to register the read-only conn.
	for i := 0; i < 200; i++ {
		if mgr.RoomCount() == 1 && bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	err := mgr.ApplyExternalContent("item-a", "# new content")
	if !errors.Is(err, ErrNoApplierAvailable) {
		t.Fatalf("read-only-only room: want ErrNoApplierAvailable (so the caller direct-writes), got %v", err)
	}
}

// TestHandleControlMessageIgnoresAckFromReadOnlyConn is the
// defence-in-depth half of the TASK-265 applier fix: even if a
// read-only conn somehow holds a pending-ack slot (e.g. an editor
// demoted after being elected), an applier_ack from it must be
// ignored so ApplyExternalContent doesn't report a phantom success.
func TestHandleControlMessageIgnoresAckFromReadOnlyConn(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	room := mgr.getOrCreate("item-a")

	// Read-only conn: canWrite defaults to false. The conn pointer is
	// only used for identity (pending-ack expectedConn match); no I/O
	// happens on it because the ack is rejected before resolveApplierAck.
	rc := &roomConn{id: 1, conn: &websocket.Conn{}}

	reqID := "req-readonly"
	ch, err := room.registerPendingAck(reqID, rc)
	if err != nil {
		t.Fatalf("registerPendingAck: %v", err)
	}

	payload, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: reqID})
	room.handleControlMessage(rc, payload)

	select {
	case <-ch:
		t.Fatal("applier_ack from a read-only conn must be ignored, but it resolved the round-trip")
	case <-time.After(50 * time.Millisecond):
		// Correct: the ack was dropped before resolveApplierAck.
	}
}

// TestApplierAckFromFrozenConnResolvesFromDurableState is the BUG-2276 residual-2
// replacement for the old "frozen conn ack is ignored" test. A frozen conn's ack is
// now PROCESSED, but it resolves the round-trip from DURABLE op-log state, not a
// phantom success: if a frame in the apply bracket was frozen-dropped
// (frozenDropSeq advanced past the apply_start baseline), the ack resolves to FALSE
// (retry) — never a phantom success while the setContent was lost to the freeze.
func TestApplierAckFromFrozenConnResolvesFromDurableState(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	room := mgr.getOrCreate("item-a")

	rc := &roomConn{id: 1, conn: &websocket.Conn{}}
	rc.canWrite.Store(true)
	rc.frozen.Store(true)

	reqID := "req-frozen"
	ch, err := room.registerPendingAck(reqID, rc)
	if err != nil {
		t.Fatalf("registerPendingAck: %v", err)
	}

	// Open the apply bracket (captures the baseline), then simulate the setContent
	// frame being frozen-dropped by readLoop while this conn is frozen.
	room.startApplierApply(reqID, rc)
	rc.frozenDropSeq.Add(1)

	payload, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: reqID})
	room.handleControlMessage(rc, payload)

	select {
	case success := <-ch:
		if success {
			t.Fatal("frozen-dropped setContent must resolve to FALSE (retry), not a phantom success")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("frozen conn ack must resolve the round-trip from durable state, but nothing was delivered")
	}
}

// TestApplierAckSucceedsWhenFramePersisted confirms the positive durable case: a
// setContent frame that persisted (no frozen drop in the bracket) resolves the ack
// to SUCCESS — including the no-op-diff case where setContent persists nothing.
func TestApplierAckSucceedsWhenFramePersisted(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	room := mgr.getOrCreate("item-a")
	rc := &roomConn{id: 1, conn: &websocket.Conn{}}
	rc.canWrite.Store(true)

	reqID := "req-ok"
	ch, err := room.registerPendingAck(reqID, rc)
	if err != nil {
		t.Fatalf("registerPendingAck: %v", err)
	}
	room.startApplierApply(reqID, rc)
	// setContent persisted a frame (op-log id 7); no frozen drop.
	rc.lastPersistedOpID.Store(7)

	payload, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: reqID})
	room.handleControlMessage(rc, payload)

	select {
	case success := <-ch:
		if !success {
			t.Fatal("a persisted setContent (no frozen drop) must resolve the ack to success")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("ack must resolve the round-trip, but nothing was delivered")
	}
}

// TestPruneAndApplyAllowsReadOnlyRoom verifies the load-bearing
// writer-aware guard (TASK-265): a room whose only peers are read-only
// must NOT block the no-applier direct write. PruneAndApply runs applyFn
// (prune + write) rather than returning ErrRoomActiveDuringPrune, so the
// op-log is safely pruned even with viewers attached and a later editor
// lazy-seeds from the fresh items.content. (Read-only peers keep a
// possibly-stale Y.Doc until they reconnect/refresh — an accepted
// best-effort residual tracked in BUG-2103.)
func TestPruneAndApplyAllowsReadOnlyRoom(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSReadOnly(t, srv, "item-a")
	defer conn.Close()

	for i := 0; i < 200; i++ {
		if mgr.RoomCount() == 1 && bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ran := false
	if err := mgr.PruneAndApply("item-a", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("read-only-only room: want nil (prune allowed), got %v", err)
	}
	if !ran {
		t.Fatal("applyFn did not run — a read-only peer wrongly blocked the prune")
	}
}

// TestPruneAndApplyBlockedByLiveWriter confirms the complementary
// invariant: a live WRITER still blocks the prune (ErrRoomActiveDuringPrune)
// so the caller routes the external update through the applier protocol
// rather than clobbering the writer's in-memory Y.Doc.
func TestPruneAndApplyBlockedByLiveWriter(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a") // writer (canWrite=true)
	defer conn.Close()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ran := false
	err := mgr.PruneAndApply("item-a", func() error { ran = true; return nil })
	if !errors.Is(err, ErrRoomActiveDuringPrune) {
		t.Fatalf("want ErrRoomActiveDuringPrune with a live writer, got %v", err)
	}
	if ran {
		t.Fatal("applyFn must not run while a live writer is present")
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
