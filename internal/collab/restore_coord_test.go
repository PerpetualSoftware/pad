package collab

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// waitRestoreActive polls until the room reports a restore in progress (beginRestore
// has set restoreActive). White-box: the test lives in package collab.
func waitRestoreActive(t *testing.T, room *Room, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		room.restoreMu.Lock()
		active := room.restoreActive
		room.restoreMu.Unlock()
		if active {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("restore did not become active within deadline")
}

func roomFor(t *testing.T, mgr *RoomManager, itemID string) *Room {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		room := mgr.rooms[itemID]
		mgr.mu.Unlock()
		if room != nil {
			return room
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("room not created within deadline")
	return nil
}

// waitItemRows polls until the store holds at least n op-log rows for itemID.
func waitItemRows(t *testing.T, store *fakeOpLog, itemID string, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		c := 0
		for _, r := range store.rows {
			if r.ItemID == itemID {
				c++
			}
		}
		store.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("store did not reach %d rows for %s within deadline", n, itemID)
}

// TestApplierGateFastPathAddsNoLatency is the hard-constraint-1 guard: a NORMAL
// applier round-trip — one with NO restore in progress — must be completely
// unaffected by the restore gate. The echoing applier acks immediately; the round-
// trip must return nil promptly.
func TestApplierGateFastPathAddsNoLatency(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	stop := runApplierEcho(t, conn)
	defer stop()

	waitElectable(t, mgr, "item-a", 1)

	for i := 0; i < 5; i++ {
		start := time.Now()
		if err := mgr.ApplyExternalContent("item-a", "# rev"); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("apply %d took %v; the gate must add no measurable latency on the no-restore path", i, elapsed)
		}
	}

	// No pending-ack entries leak across applies.
	room := roomFor(t, mgr, "item-a")
	room.pendingMu.Lock()
	left := len(room.pendingAcks)
	room.pendingMu.Unlock()
	if left != 0 {
		t.Fatalf("pendingAcks leaked: %d", left)
	}
}

// TestRestoreFinalizesPersistedApplierAsSuccess_DelayedAck is the durable-correlation
// replacement for the old timing-grace sub-case (A) test. An in-flight applier
// PERSISTS its setContent frame (op-log advances) but its ack is DELAYED past any
// timer. A restore then freezes and FINALIZES the round-trip from durable op-log
// state: because the frame landed BEFORE the freeze, the round-trip resolves to
// SUCCESS — it does NOT re-apply, so no clobber. The rollback keeps the frame.
func TestRestoreFinalizesPersistedApplierAsSuccess_DelayedAck(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()

	var requestCount int32
	gotFirstRequest := make(chan struct{}, 1)
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
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			if atomic.AddInt32(&requestCount, 1) == 1 {
				// Open the durable bracket, then send the setContent frame (persists
				// while unfrozen). DELIBERATELY never send the ack — the durable
				// finalization must decide from op-log state, not the ack.
				start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
				_ = conn.WriteMessage(websocket.TextMessage, start)
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0xA1})
				select {
				case gotFirstRequest <- struct{}{}:
				default:
				}
			}
		}
	}()

	waitElectable(t, mgr, "item-a", 1)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest
	// Ensure the setContent frame has DURABLY persisted before the restore freezes,
	// so finalization observes the high-water advance.
	waitItemRows(t, store, "item-a", 1, 2*time.Second)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") },
			nil)
	}()

	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("persisted-before-freeze round-trip must resolve to SUCCESS, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyExternalContent did not return (finalization should have delivered success)")
	}
	select {
	case err := <-restoreDone:
		if err == nil {
			t.Fatal("restore was supposed to roll back")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}

	if n := atomic.LoadInt32(&requestCount); n != 1 {
		t.Fatalf("persisted round-trip must NOT re-apply (no clobber); want 1 request, got %d", n)
	}
	store.mu.Lock()
	frameFound := false
	for _, r := range store.rows {
		if r.ItemID == "item-a" && len(r.UpdateData) >= 2 && r.UpdateData[1] == 0xA1 {
			frameFound = true
		}
	}
	store.mu.Unlock()
	if !frameFound {
		t.Fatal("the applier's persisted frame must survive the rollback")
	}
}

// TestRestoreFinalizesUnpersistedApplierAsRetry is sub-case (B): an in-flight applier
// that opened its bracket but whose setContent did NOT persist (no frame) is
// finalized as NOT-persisted → it re-elects after the restore rolls back and
// re-applies (a SECOND request), then succeeds. No false success.
func TestRestoreFinalizesUnpersistedApplierAsRetry(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(2*time.Second, 1*time.Second)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()

	var requestCount int32
	gotFirstRequest := make(chan struct{}, 1)
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
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			n := atomic.AddInt32(&requestCount, 1)
			if n == 1 {
				// Open the bracket but persist NOTHING (setContent did not land).
				start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
				_ = conn.WriteMessage(websocket.TextMessage, start)
				select {
				case gotFirstRequest <- struct{}{}:
				default:
				}
				continue
			}
			// Re-elected after the rollback: honestly ack (no frozen drop → success).
			start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, start)
			ack, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
		}
	}()

	waitElectable(t, mgr, "item-a", 1)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") },
			nil)
	}()

	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("unpersisted round-trip must re-apply + succeed, got %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("ApplyExternalContent did not return")
	}
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}

	if n := atomic.LoadInt32(&requestCount); n != 2 {
		t.Fatalf("unpersisted round-trip must re-apply after rollback; want 2 requests, got %d", n)
	}
}

// TestRestoreCommitForceClosesThenFallsBack: an in-flight (unpersisted) applier
// finalized by a COMMITTING restore re-elects against the force-closed room, finds no
// applier, and returns ErrNoApplierAvailable (→ caller's direct-write fallback).
func TestRestoreCommitForceClosesThenFallsBack(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(2*time.Second, 1*time.Second)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()

	gotFirstRequest := make(chan struct{}, 1)
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
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			start, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierApplyStart, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, start)
			select {
			case gotFirstRequest <- struct{}{}:
			default:
			}
		}
	}()

	waitElectable(t, mgr, "item-a", 1)
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a", func() (int64, int64, error) {
			maxID, _, merr := store.MaxOpLogID("item-a")
			if merr != nil {
				return 0, 0, merr
			}
			if _, perr := store.PruneYjsUpdatesBefore("item-a", distantFuture); perr != nil {
				return 0, 0, perr
			}
			return maxID, 7, nil
		}, nil)
	}()

	select {
	case err := <-applyDone:
		if !errors.Is(err, ErrNoApplierAvailable) {
			t.Fatalf("commit case: re-election must find no applier → ErrNoApplierAvailable, got %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("commit case: ApplyExternalContent did not return")
	}
	select {
	case err := <-restoreDone:
		if err != nil {
			t.Fatalf("commit was supposed to succeed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}
	if b, ok := mgr.RestoreBoundary("item-a"); !ok || b <= 0 {
		t.Fatalf("commit must publish a restore boundary, got (%d,%v)", b, ok)
	}
}

// TestApplierWaitsForRestoreInProgress: a NEW round-trip that starts while a restore
// holds the room WAITS for it to resolve before electing, then applies on rollback.
func TestApplierWaitsForRestoreInProgress(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()
	stop := runApplierEcho(t, conn)
	defer stop()

	waitElectable(t, mgr, "item-a", 1)

	room := roomFor(t, mgr, "item-a")

	commitGate := make(chan struct{})
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a", func() (int64, int64, error) {
			<-commitGate
			return 0, 0, errors.New("commit: rolled back")
		}, nil)
	}()

	waitRestoreActive(t, room, 2*time.Second)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	select {
	case err := <-applyDone:
		t.Fatalf("applier round-trip must wait for the in-progress restore, but returned early: %v", err)
	case <-time.After(150 * time.Millisecond):
		// good — still blocked in the gate
	}

	close(commitGate)

	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("after rollback the waiting round-trip must apply + succeed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyExternalContent did not return after the restore resolved")
	}
	<-restoreDone
}

// TestForceRefreshDoesNotStallOnInFlightApplier is the P1(a) guard: ForceRefreshRoom
// must NOT wait for an in-flight applier round-trip (the old drain could stall
// forever on a blocked applier write). With the finalization redesign there is no
// drain — the restore finalizes the round-trip and proceeds. Here the applier never
// acks; ForceRefreshRoom must still complete promptly.
func TestForceRefreshDoesNotStallOnInFlightApplier(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(500*time.Millisecond, 250*time.Millisecond)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()

	gotFirstRequest := make(chan struct{}, 1)
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
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			// Never respond — keep the round-trip in-flight.
			select {
			case gotFirstRequest <- struct{}{}:
			default:
			}
		}
	}()

	waitElectable(t, mgr, "item-a", 1)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest

	start := time.Now()
	err := mgr.ForceRefreshRoom("item-a",
		func() (int64, int64, error) { return 0, 0, errors.New("rollback") }, nil)
	if err == nil {
		t.Fatal("restore was supposed to roll back")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("ForceRefreshRoom stalled on an in-flight applier (%v); the drain-stall regression is back", elapsed)
	}
	<-applyDone // the round-trip re-elects/falls back after the rollback
}

// TestWriteApplierRequestBoundedOnWedgedWrite is the P1(a) bounded-write guard: a
// conn whose writeMu is held by a genuinely blocked WriteMessage — a real peer that
// never drains, so a large frame fills the socket buffer and blocks — must not wedge
// writeApplierRequest forever. The independent close-timer closes the conn, the
// wedged write errors, writeMu releases, and writeApplierRequest returns an error
// promptly (not after the applier timeout).
func TestWriteApplierRequestBoundedOnWedgedWrite(t *testing.T) {
	prev := applierWriteDeadlineVar
	applierWriteDeadlineVar = 200 * time.Millisecond
	defer func() { applierWriteDeadlineVar = prev }()

	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// Client that NEVER reads: its receive buffer (and the server's send buffer)
	// will fill, so a large server-side write blocks in conn.Write.
	client := dialWSBracket(t, srv, "item-a")
	defer client.Close()

	room := roomFor(t, mgr, "item-a")
	var rc *roomConn
	for i := 0; i < 200 && rc == nil; i++ {
		room.mu.Lock()
		for _, c := range room.conns {
			rc = c
		}
		room.mu.Unlock()
		if rc == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if rc == nil {
		t.Fatal("server-side roomConn never registered")
	}

	// Wedge writeMu with a real, blocking WriteMessage (8 MiB to a non-draining
	// peer). rc.writeMessage holds writeMu across the write; Close interrupts it.
	huge := make([]byte, 8<<20)
	huge[0] = yMessageSync
	wedged := make(chan struct{})
	go func() {
		close(wedged)
		_ = rc.writeMessage(websocket.BinaryMessage, huge)
	}()
	<-wedged
	// Give the wedging write time to acquire writeMu and block on the full buffer.
	time.Sleep(100 * time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- writeApplierRequest(rc, []byte(`{"type":"applier_request"}`)) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writeApplierRequest to a wedged/closed conn must return an error")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("bounded write took %v; the close-timer must bound it near applierWriteDeadline", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeApplierRequest wedged forever; the bounded-write close-timer did not fire")
	}
}

// TestConcurrentAppliersAndRestoreNoDeadlock stress-drives several applier round-trips
// interleaved with restores to shake out lock-order / finalization deadlocks under -race.
func TestConcurrentAppliersAndRestoreNoDeadlock(t *testing.T) {
	origFirst, origRetry := applierFirstTimeoutVar, applierRetryTimeoutVar
	swapApplierTimeouts(120*time.Millisecond, 60*time.Millisecond)
	defer swapApplierTimeouts(origFirst, origRetry)

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSBracket(t, srv, "item-a")
	defer conn.Close()
	stop := runApplierEcho(t, conn)
	defer stop()

	waitElectable(t, mgr, "item-a", 1)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mgr.ApplyExternalContent("item-a", "# c")
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mgr.ForceRefreshRoom("item-a",
				func() (int64, int64, error) { return 0, 0, errors.New("rollback") }, nil)
		}()
	}

	fin := make(chan struct{})
	go func() { wg.Wait(); close(fin) }()
	select {
	case <-fin:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: concurrent appliers + restores did not all return")
	}
}

// TestPickApplierSkipsUnanchoredConn is the P1(1) guard: a conn that is in the room
// but NOT yet anchored (replayDone false — its client still buffers Y.Doc updates
// until it receives the initial cursor) must NEVER be elected applier, else its
// setContent frame would be buffered/unsent yet still acked → a false success for a
// never-persisted write. Once anchored, it is electable.
func TestPickApplierSkipsUnanchoredConn(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	room := mgr.getOrCreate("item-a")

	rc := &roomConn{id: 1, conn: &websocket.Conn{}}
	rc.canWrite.Store(true)
	rc.bracketCapable.Store(true)
	// replayDone NOT set → unanchored.
	if err := room.addConn(rc); err != nil {
		t.Fatalf("addConn: %v", err)
	}
	defer func() {
		room.mu.Lock()
		delete(room.conns, rc.conn)
		room.mu.Unlock()
	}()

	if got := room.pickApplier(map[*websocket.Conn]struct{}{}); got != nil {
		t.Fatal("pickApplier must NOT elect an unanchored (replayDone=false) conn")
	}

	rc.replayDone.Store(true) // client has now received its cursor
	if got := room.pickApplier(map[*websocket.Conn]struct{}{}); got != rc {
		t.Fatal("pickApplier must elect an anchored, writable, bracket-capable conn")
	}
}

// TestBeginRestoreDrainsAdmissionGap is the P1(2) guard: a round-trip admitted by
// enterApplierGate BEFORE beginRestore, but which registers its pending entry AFTER,
// must NOT be missed by the freeze scan. beginRestore must WAIT (drain) for the
// admitted-but-unregistered round-trip to register, then finalize it — so it gets a
// durable outcome (here NotPersisted), never a hang or a false success.
func TestBeginRestoreDrainsAdmissionGap(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	room := mgr.getOrCreate("item-a")

	rc := &roomConn{id: 1, conn: &websocket.Conn{}}
	rc.canWrite.Store(true)
	rc.bracketCapable.Store(true)
	rc.replayDone.Store(true)
	if err := room.addConn(rc); err != nil {
		t.Fatalf("addConn: %v", err)
	}
	defer func() {
		room.mu.Lock()
		delete(room.conns, rc.conn)
		room.mu.Unlock()
	}()

	// Admit a round-trip (enter the gate) but DON'T register yet — the enter→register
	// gap the drain must cover.
	if room.enterApplierGate() {
		t.Fatal("unexpected wait: no restore is in progress")
	}

	restoreDone := make(chan struct{})
	go func() {
		room.beginRestore() // blocks in the admission drain until we register + finish
		room.freezeAndFinalizePending()
		room.resolveRestore()
		close(restoreDone)
	}()

	// beginRestore sets restoreActive then blocks draining the admission gap.
	waitRestoreActive(t, room, time.Second)
	select {
	case <-restoreDone:
		t.Fatal("beginRestore proceeded to finalize BEFORE the admitted round-trip registered (P1)")
	case <-time.After(100 * time.Millisecond):
		// good — still draining
	}

	// Complete the admission span: register, then finish. The drain unblocks and the
	// freeze scan finalizes our now-registered entry.
	resultCh, err := room.registerPendingAck("req-1", rc)
	if err != nil {
		t.Fatalf("registerPendingAck: %v", err)
	}
	room.finishAdmission()

	select {
	case outcome := <-resultCh:
		if outcome != applierNotPersisted {
			t.Fatalf("admitted-then-registered round-trip: want NotPersisted, got %v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("admitted-then-registered round-trip was MISSED by the freeze scan (would hang the applier timeout)")
	}
	<-restoreDone
}

// TestRestoreLegacyPersistedIsAmbiguousNoReapply is the P1(3) mixed-deploy guard: a
// LEGACY (non-bracket-capable) applier persists its setContent before a restore
// freeze, then the restore ROLLS BACK. Because the server can't confirm persistence
// without the apply_start bracket, the round-trip resolves AMBIGUOUS →
// ErrApplierAmbiguous (retryable), and it does NOT re-apply — so a peer edit between
// rollback and a would-be retry is never clobbered. The applier is asked exactly once.
func TestRestoreLegacyPersistedIsAmbiguousNoReapply(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	// LEGACY conn: dialWS does NOT announce ?applier_bracket=1.
	conn := dialWS(t, srv, "item-a")
	defer conn.Close()

	var requestCount int32
	gotFirstRequest := make(chan struct{}, 1)
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
			if json.Unmarshal(data, &ctl) != nil || ctl.Type != ControlMessageApplierRequest {
				continue
			}
			if atomic.AddInt32(&requestCount, 1) == 1 {
				// Legacy behavior: persist a setContent frame, but send NO apply_start
				// bracket (and no ack).
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0xB2})
				select {
				case gotFirstRequest <- struct{}{}:
				default:
				}
			}
		}
	}()

	waitElectable(t, mgr, "item-a", 1)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest
	waitItemRows(t, store, "item-a", 1, 2*time.Second) // frame persisted before the freeze

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") }, nil)
	}()

	select {
	case err := <-applyDone:
		if !errors.Is(err, ErrApplierAmbiguous) {
			t.Fatalf("legacy persisted round-trip during a restore must fail-safe with ErrApplierAmbiguous, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyExternalContent did not return")
	}
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}

	if n := atomic.LoadInt32(&requestCount); n != 1 {
		t.Fatalf("ambiguous legacy round-trip must NOT re-apply (no clobber); want 1 request, got %d", n)
	}
}
