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

// swapApplierPreemptGrace tunes the preempt-grace window for a test. Mirrors
// swapApplierTimeouts. Guarded by the same applierTimeoutMu since the package's
// tests run serially by default.
func swapApplierPreemptGrace(d time.Duration) time.Duration {
	applierTimeoutMu.Lock()
	defer applierTimeoutMu.Unlock()
	prev := applierPreemptGraceVar
	applierPreemptGraceVar = d
	return prev
}

// waitRestoreActive polls until the room reports a restore in progress (beginRestore
// has set restoreActive + closed the preempt channel). White-box: the test lives in
// package collab.
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

// TestApplierGateFastPathAddsNoLatency is the hard-constraint-1 guard: a NORMAL
// applier round-trip — one with NO restore in progress — must be completely
// unaffected by the restore gate. The echoing applier acks immediately; the round-
// trip must return nil promptly (the gate is a single leaf-mutex bump, its preempt
// channel never fires), NOT stalled by any of the residual-2 machinery.
func TestApplierGateFastPathAddsNoLatency(t *testing.T) {
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

	// Run several applies and assert each returns quickly. A regression that
	// blocked normal acks under the gate/lock (the reverted round-4 attempt) would
	// blow the per-call budget.
	for i := 0; i < 5; i++ {
		start := time.Now()
		if err := mgr.ApplyExternalContent("item-a", "# rev"); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("apply %d took %v; the gate must add no measurable latency on the no-restore path", i, elapsed)
		}
	}

	// The gate must be perfectly balanced: no leaked in-flight count.
	room := roomFor(t, mgr, "item-a")
	room.restoreMu.Lock()
	inFlight := room.applierInFlight
	room.restoreMu.Unlock()
	if inFlight != 0 {
		t.Fatalf("applierInFlight leaked: %d (want 0)", inFlight)
	}
}

// TestRestoreDrainsInFlightApplier_AckDeliveredIsSuccess covers sub-case (A): an
// in-flight applier round-trip whose content already landed (a sync frame persisted
// while the room was UNFROZEN) and whose ack is delivered during the preempt grace
// must resolve as SUCCESS — the round-trip does NOT retry, so the external content
// is NOT re-applied over peer edits. The restore then rolls back; the persisted
// frame survives (rollback prunes nothing).
func TestRestoreDrainsInFlightApplier_AckDeliveredIsSuccess(t *testing.T) {
	// Generous grace so the ack we release after the preempt lands inside it.
	defer swapApplierPreemptGrace(swapApplierPreemptGrace(2 * time.Second))

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()

	var requestCount int32
	gotFirstRequest := make(chan struct{}, 1)
	ackNow := make(chan struct{})
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
				// Simulate setContent(markdown): a Y.Doc sync frame that persists to
				// the op-log while the room is still unfrozen (sub-case A landing).
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte{yMessageSync, 0xA1})
				select {
				case gotFirstRequest <- struct{}{}:
				default:
				}
				<-ackNow // hold the ack until the restore has preempted us
				ack, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID})
				_ = conn.WriteMessage(websocket.TextMessage, ack)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest // the applier holds its ack; round-trip is in-flight

	room := roomFor(t, mgr, "item-a")
	// Start a rolling-back restore. beginRestore preempts the in-flight round-trip
	// and blocks draining it until we release the ack.
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") },
			nil)
	}()

	waitRestoreActive(t, room, 2*time.Second) // preempt channel is now closed
	close(ackNow)                             // ack delivered while still unfrozen

	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("sub-case A: round-trip whose ack landed during preempt must SUCCEED, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sub-case A: ApplyExternalContent did not return")
	}

	select {
	case err := <-restoreDone:
		if err == nil {
			t.Fatal("restore was supposed to roll back (commit error)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}

	// No re-apply: the applier was asked exactly once.
	if n := atomic.LoadInt32(&requestCount); n != 1 {
		t.Fatalf("sub-case A: applier must be asked exactly once (no re-apply/clobber), got %d requests", n)
	}
	// The persisted frame survived the rollback (op-log was never pruned).
	store.mu.Lock()
	frameFound := false
	for _, r := range store.rows {
		if r.ItemID == "item-a" && len(r.UpdateData) >= 2 && r.UpdateData[1] == 0xA1 {
			frameFound = true
		}
	}
	store.mu.Unlock()
	if !frameFound {
		t.Fatal("sub-case A: the applier's persisted frame must survive a rollback")
	}
}

// TestRestorePreemptsInFlightApplier_ReAppliesOnRollback covers sub-case (B): an
// in-flight applier round-trip whose content did NOT land (no ack, nothing
// persisted) is preempted by a restore. It must NOT be told it succeeded; after the
// restore ROLLS BACK it re-elects and re-applies (a SECOND applier_request), then
// succeeds. Distinguishing assertion vs (A): the applier is asked TWICE.
func TestRestorePreemptsInFlightApplier_ReAppliesOnRollback(t *testing.T) {
	// Short grace: no ack arrives, so the preempt handler abandons quickly.
	defer swapApplierPreemptGrace(swapApplierPreemptGrace(60 * time.Millisecond))

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

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
			n := atomic.AddInt32(&requestCount, 1)
			if n == 1 {
				// First request: do NOT ack, do NOT persist a frame — the content
				// never lands (sub-case B). Just signal the test and let the grace
				// expire so the round-trip is preempted + parked.
				select {
				case gotFirstRequest <- struct{}{}:
				default:
				}
				continue
			}
			// Re-elected after the rollback: honestly ack this time.
			ack, _ := json.Marshal(ControlMessage{Type: ControlMessageApplierAck, RequestID: ctl.RequestID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) { return 0, 0, errors.New("commit: rolled back") },
			nil)
	}()

	// The restore rolls back (un-freezes the conn); the parked round-trip then
	// re-elects the now-live applier and succeeds.
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("sub-case B: round-trip must eventually SUCCEED via re-apply, got %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("sub-case B: ApplyExternalContent did not return")
	}
	select {
	case <-restoreDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}

	if n := atomic.LoadInt32(&requestCount); n != 2 {
		t.Fatalf("sub-case B: applier must be re-asked (retry after rollback); want 2 requests, got %d", n)
	}
}

// TestRestoreCommitForceClosesThenFallsBack covers the COMMIT case: an in-flight
// applier round-trip preempted by a restore that COMMITS finds, on re-election, that
// the peer was force-closed (frozen + socket closed). No applier is available, so
// ApplyExternalContent returns ErrNoApplierAvailable — the caller then direct-writes,
// and the collab-snapshot restore boundary rejects any stale in-flight flush
// (existing, unchanged behavior). It must NOT falsely report success against the
// frozen/closed conn.
func TestRestoreCommitForceClosesThenFallsBack(t *testing.T) {
	defer swapApplierPreemptGrace(swapApplierPreemptGrace(60 * time.Millisecond))

	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
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
			// Never ack — force the preempt/park path.
			select {
			case gotFirstRequest <- struct{}{}:
			default:
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Seed an op-log row so the commit callback has a MAX to capture.
	if _, err := store.AppendYjsUpdate("item-a", []byte{yMessageSync, 0x01}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	<-gotFirstRequest

	restoreDone := make(chan error, 1)
	go func() {
		// A CLEAN commit: prune the op-log, return (MAX, seq). ForceRefreshRoom then
		// force-closes the conns.
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
			t.Fatalf("commit case: re-election must find no applier (force-closed) → ErrNoApplierAvailable, got %v", err)
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

	// The commit published the restore fences the direct-write fallback / snapshot
	// gate rely on.
	if b, ok := mgr.RestoreBoundary("item-a"); !ok || b <= 0 {
		t.Fatalf("commit must publish a restore boundary, got (%d,%v)", b, ok)
	}
}

// TestApplierWaitsForRestoreInProgress covers the enter-gate direction: a NEW applier
// round-trip that starts WHILE a restore already holds the room must WAIT for the
// restore to resolve before electing — it must not race the frozen conn. On rollback
// it then applies against the unfrozen room and succeeds.
func TestApplierWaitsForRestoreInProgress(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	store := &fakeOpLog{}
	mgr := NewRoomManager(store, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	stop := runApplierEcho(t, conn)
	defer stop()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	room := roomFor(t, mgr, "item-a")

	// Hold the restore in its commit until we release it, so it is DEFINITELY in
	// progress when ApplyExternalContent starts.
	commitGate := make(chan struct{})
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a", func() (int64, int64, error) {
			<-commitGate
			return 0, 0, errors.New("commit: rolled back") // rollback → un-freeze
		}, nil)
	}()

	waitRestoreActive(t, room, 2*time.Second)

	applyDone := make(chan error, 1)
	go func() { applyDone <- mgr.ApplyExternalContent("item-a", "# external") }()

	// While the restore is in progress, the round-trip must be BLOCKED in the gate.
	select {
	case err := <-applyDone:
		t.Fatalf("applier round-trip must wait for the in-progress restore, but returned early: %v", err)
	case <-time.After(150 * time.Millisecond):
		// good — still blocked
	}

	// Resolve the restore (rollback). The gate releases; the applier round-trip
	// re-elects against the unfrozen room and the echo acks it.
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

// TestBeginRestoreDrainsPromptlyWithNoAppliers is a white-box guard that beginRestore
// returns immediately when nothing is in flight, and that the gate's counters/preempt
// channel cycle correctly across begin/resolve.
func TestBeginRestoreDrainsPromptlyWithNoAppliers(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()
	room := mgr.getOrCreate("item-a")

	preemptBefore := room.restorePreempt

	done := make(chan struct{})
	go func() {
		room.beginRestore()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("beginRestore blocked with no in-flight appliers")
	}

	// A new applier gate entry now blocks until resolve.
	entered := make(chan struct{})
	go func() {
		_, waited := room.enterApplierGate()
		if !waited {
			t.Error("enterApplierGate during an active restore must report waited=true")
		}
		room.exitApplierGate()
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("enterApplierGate must block while a restore is active")
	case <-time.After(100 * time.Millisecond):
	}

	room.resolveRestore()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("enterApplierGate did not unblock after resolveRestore")
	}

	// resolveRestore installed a fresh preempt channel and cleared active state.
	room.restoreMu.Lock()
	active := room.restoreActive
	freshPreempt := room.restorePreempt
	room.restoreMu.Unlock()
	if active {
		t.Fatal("restoreActive must be false after resolveRestore")
	}
	if freshPreempt == preemptBefore {
		t.Fatal("resolveRestore must install a fresh preempt channel")
	}
	// The old preempt channel must be closed (it preempted in-flight round-trips).
	select {
	case <-preemptBefore:
	default:
		t.Fatal("beginRestore must have closed the captured preempt channel")
	}
}

// TestConcurrentAppliersAndRestoreNoDeadlock stress-drives several applier round-trips
// interleaved with restores to shake out lock-order / drain deadlocks under -race.
func TestConcurrentAppliersAndRestoreNoDeadlock(t *testing.T) {
	defer swapApplierPreemptGrace(swapApplierPreemptGrace(30 * time.Millisecond))
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

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	stop := runApplierEcho(t, conn)
	defer stop()

	for i := 0; i < 200; i++ {
		if bus.SubscriberCount("item-a") == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

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
			// Rolling-back restores so the conn stays alive across the run.
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
