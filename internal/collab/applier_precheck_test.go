package collab

import (
	"errors"
	"testing"
	"time"
)

// pickApplierOnly is the NEGATIVE CONTROL for HasElectableApplier: the formulation
// this unit rejected, which asks the conns directly and omits the restore clause.
// It exists so the table below can demonstrate that it DISCRIMINATES — a table both
// implementations pass would be evidence about neither (CONVE-30).
func pickApplierOnly(m *RoomManager, itemID string) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	room := m.rooms[itemID]
	m.mu.Unlock()
	if room == nil {
		return false
	}
	return room.pickApplier(nil) != nil
}

// waitNoConns polls until the room for itemID has zero connections but is still
// registered with the manager — the grace-TTL window, which is the state that
// produces ErrNoApplierAvailable rather than ErrNoActiveRoom.
func waitNoConns(t *testing.T, mgr *RoomManager, itemID string, d time.Duration) *Room {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		room := mgr.rooms[itemID]
		mgr.mu.Unlock()
		if room != nil {
			room.mu.Lock()
			n := len(room.conns)
			room.mu.Unlock()
			if n == 0 {
				return room
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("room %s did not reach zero conns within deadline", itemID)
	return nil
}

// waitConnCount polls until the room holds exactly n connections.
func waitConnCount(t *testing.T, room *Room, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		c := len(room.conns)
		room.mu.Unlock()
		if c == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("room did not reach %d conns within deadline", n)
}

// waitAllFrozen polls until the room holds at least one conn and EVERY conn is
// frozen — the state ForceRefreshRoom establishes before it runs its transaction,
// and the state in which pickApplier alone answers false.
func waitAllFrozen(t *testing.T, room *Room, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		n := len(room.conns)
		all := n > 0
		for _, rc := range room.conns {
			if !rc.frozen.Load() {
				all = false
			}
		}
		room.mu.Unlock()
		if all {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("conns did not all reach frozen within deadline")
}

// TestHasElectableApplierNoRoom: the manager has never seen the item.
func TestHasElectableApplierNoRoom(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	if mgr.HasElectableApplier("item-a") {
		t.Fatal("no room for the item must answer false (the caller's direct write is the right path)")
	}
}

// TestHasElectableApplierManagerClosed: a closed manager elects nobody, so the
// answer must be false rather than a stale true read off the rooms map.
func TestHasElectableApplierManagerClosed(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)

	if !mgr.HasElectableApplier("item-a") {
		t.Fatal("precondition: a healthy writer must answer true before the manager closes")
	}
	mgr.Close()
	if mgr.HasElectableApplier("item-a") {
		t.Fatal("a closed manager must answer false")
	}
}

// TestHasElectableApplierClosedFlagAloneAnswersFalse pins the m.closed guard itself.
//
// It exists because the guard is UNREACHABLE as a distinct answer through the public
// API: Close sets m.closed and replaces m.rooms with an empty map inside ONE m.mu
// critical section, so after a real Close the room lookup already answers nil and the
// test above passes with the guard deleted (measured — that mutant survived).
//
// So this constructs a state Close does not currently produce: closed set, rooms map
// left populated. That is not a state to defend against today; it is the coupling the
// guard exists to break. Without it, HasElectableApplier's correctness depends on an
// invariant living in Close that nothing enforces, and a future Close that stops
// clearing the map would silently hand out appliers from a dead manager.
func TestHasElectableApplierClosedFlagAloneAnswersFalse(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)

	mgr.mu.Lock()
	mgr.closed = true
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		mgr.closed = false
		mgr.mu.Unlock()
	}()

	if mgr.HasElectableApplier("item-a") {
		t.Fatal("a manager whose closed flag is set must answer false even while its rooms map still holds an electable conn")
	}
}

// TestHasElectableApplierGraceTTLNoConns: the room outlives its last conn for
// graceTTL. ApplyExternalContent answers ErrNoApplierAvailable there, so the hint
// must answer false — this is the path whose direct write also prunes the op-log.
func TestHasElectableApplierGraceTTLNoConns(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManagerWithConfig(&fakeOpLog{}, bus, RoomManagerConfig{GraceTTL: 30 * time.Second})
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	waitElectable(t, mgr, "item-a", 1)
	_ = conn.Close()

	room := waitNoConns(t, mgr, "item-a", 2*time.Second)
	if room == nil {
		t.Fatal("room went away entirely; this test needs the grace-TTL window")
	}
	if mgr.HasElectableApplier("item-a") {
		t.Fatal("a room inside its grace TTL with zero conns must answer false")
	}
}

// TestHasElectableApplierReadOnlyOnly: a view-only participant is never an eligible
// applier (its sync frames are dropped by the read-only gate), so a room holding
// only viewers must answer false.
func TestHasElectableApplierReadOnlyOnly(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWSReadOnly(t, srv, "item-a")
	defer conn.Close()

	room := roomFor(t, mgr, "item-a")
	waitConnCount(t, room, 1, 2*time.Second)

	if mgr.HasElectableApplier("item-a") {
		t.Fatal("a room holding only read-only participants must answer false")
	}
}

// TestHasElectableApplierUnanchoredOnly: a conn in the room map whose replay has not
// finished buffers Y.Doc updates client-side and would ack a frame that never went
// out, so pickApplier excludes it — and so must the hint.
func TestHasElectableApplierUnanchoredOnly(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)

	room := roomFor(t, mgr, "item-a")
	room.mu.Lock()
	for _, rc := range room.conns {
		rc.replayDone.Store(false)
	}
	room.mu.Unlock()

	if mgr.HasElectableApplier("item-a") {
		t.Fatal("an unanchored conn is not electable, so the hint must answer false")
	}
}

// TestHasElectableApplierHealthyWriter is the positive leg: one anchored writer.
func TestHasElectableApplierHealthyWriter(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)

	if !mgr.HasElectableApplier("item-a") {
		t.Fatal("one anchored writable conn must answer true")
	}
}

// TestHasElectableApplierDuringRestoreAnswersTrue is the DISCRIMINATING leg, and the
// reason the restore clause exists.
//
// ForceRefreshRoom freezes every conn for the duration of its transaction, and
// pickApplier skips frozen conns. So while a restore holds the room, the conns look
// un-electable — yet on a ROLLBACK they are unfrozen and an applier IS elected. A
// false answer here is the dangerous direction: it would send the caller back to
// apply-then-write, which is BUG-2840 half A.
//
// The test drives a restore that blocks inside commit (restoreActive set, conns
// frozen), probes both implementations, then releases the commit into a rollback.
// The negative control MUST disagree — a table both formulations pass would be
// evidence about neither.
func TestHasElectableApplierDuringRestoreAnswersTrue(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)
	room := roomFor(t, mgr, "item-a")

	releaseCommit := make(chan struct{})
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ForceRefreshRoom("item-a",
			func() (int64, int64, error) {
				<-releaseCommit
				return 0, 0, errors.New("commit: rolled back")
			}, nil)
	}()

	waitRestoreActive(t, room, 2*time.Second)
	waitAllFrozen(t, room, 2*time.Second)

	// The probe runs in a goroutine so a hypothetical blocking implementation fails
	// as a legible assertion rather than as a package-level test timeout: this hint
	// sits on a request path and MUST NOT wait out a restore transaction.
	got := make(chan bool, 1)
	go func() { got <- mgr.HasElectableApplier("item-a") }()
	select {
	case v := <-got:
		if !v {
			t.Fatal("a room held by an in-progress restore must answer TRUE: a rollback unfreezes the conns and elects an applier, so false is the direction that reintroduces BUG-2840")
		}
	case <-time.After(2 * time.Second):
		close(releaseCommit)
		t.Fatal("HasElectableApplier blocked on an in-progress restore; it must be a point-in-time read, never a gate")
	}

	if pickApplierOnly(mgr, "item-a") {
		t.Fatal("negative control is not discriminating: the pickApplier-only formulation was supposed to answer false here, so this table would prove nothing about the restore clause")
	}

	close(releaseCommit)
	select {
	case err := <-restoreDone:
		if err == nil {
			t.Fatal("this restore was supposed to roll back")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForceRefreshRoom did not return")
	}
}

// TestHasElectableApplierTakesNoAdmission pins that the hint is not a gate entry: it
// must leave admittedUnregistered untouched, or a concurrent beginRestore would
// drain against a round-trip that will never register (BUG-2276 residual 2, P1).
func TestHasElectableApplierTakesNoAdmission(t *testing.T) {
	bus := NewMemoryOpBus()
	defer bus.Close()
	mgr := NewRoomManager(&fakeOpLog{}, bus)
	defer mgr.Close()

	srv := newCollabTestServer(t, mgr)
	defer srv.Close()

	conn := dialWS(t, srv, "item-a")
	defer conn.Close()
	waitElectable(t, mgr, "item-a", 1)
	room := roomFor(t, mgr, "item-a")

	for i := 0; i < 5; i++ {
		if !mgr.HasElectableApplier("item-a") {
			t.Fatalf("probe %d: want true", i)
		}
	}

	room.restoreMu.Lock()
	admitted := room.admittedUnregistered
	room.restoreMu.Unlock()
	if admitted != 0 {
		t.Fatalf("the hint must take no admission; admittedUnregistered = %d after 5 probes", admitted)
	}
}
