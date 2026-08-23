package events

import (
	"sync"
	"testing"
)

// BUG-2736: a restarted process used to reuse its ID space, so a cursor issued
// by a PREVIOUS incarnation could be answered from the CURRENT one's buffer as
// though the two were the same sequence.
//
// The fix carries the ID space's identity in the ID's VALUE (see
// internal/idspace), so these tests assert BOTH halves of that: the
// mechanism (the spaces are disjoint, and disjoint in the right direction) and
// the behaviour it produces (the stale cursor is refused). Asserting only the
// behaviour would pass on a bus that answers nil unconditionally, which is the
// false-positive inversion this family has to keep ruling out; asserting only
// the mechanism would pass on a bus that computes a fine base and then ignores
// it when answering a resume.

// publishN publishes n events to a workspace and returns the IDs assigned, in
// order. The IDs are the observable half of the ID space, which is what these
// tests are about — reading them back through a subscriber rather than
// recomputing them keeps the test honest about what the bus actually issued.
func publishN(b *MemoryBus, workspaceID string, n int) []int64 {
	types := make([]string, n)
	for i := range types {
		types[i] = ItemUpdated
	}
	return publishTyped(b, workspaceID, types...)
}

// publishTyped is publishN for tests that care which event types land, and is
// the reason neither helper recomputes an ID: since BUG-2736 the IDs a bus
// issues depend on when the process started, so a test that spells one out is
// asserting the clock. Every test in this package indexes off what the bus
// actually assigned.
func publishTyped(b *MemoryBus, workspaceID string, types ...string) []int64 {
	ids := make([]int64, 0, len(types))
	ch, _ := b.Subscribe(workspaceID)
	defer b.Unsubscribe(ch)
	for _, typ := range types {
		b.Publish(Event{Type: typ, WorkspaceID: workspaceID})
		ids = append(ids, (<-ch).ID)
	}
	return ids
}

func TestRestartCursorIsRefusedBecauseTheIDSpacesAreDisjoint(t *testing.T) {
	const ws = "ws-1"

	// Two BUS INCARNATIONS in one process, which is what a test can build:
	// a genuine process restart is not reproducible in-process, and the thing
	// under test is the bus's identity rather than the operating system's. The
	// two are equivalent here because a bus takes its base at construction,
	// exactly as it does at process start.
	//
	// Incarnation 1: the client streams and leaves holding a cursor.
	first := New()
	firstIDs := publishN(first, ws, 3)
	first.Close()
	cursor := firstIDs[1] // the client's Last-Event-ID, from the DEAD space

	// Incarnation 2: the process restarted. Same workspace, fresh bus.
	second := New()
	secondIDs := publishN(second, ws, 5)

	// PREMISE, asserted rather than assumed. The whole fix is that these two
	// spaces cannot overlap; if they did, everything below would be testing a
	// coincidence. Note the DIRECTION matters too — the dead cursor must sit
	// BELOW the new space, because that is what makes the coverage check
	// (sinceID+1 < knownFrom) the thing that refuses it.
	for _, id := range secondIDs {
		if id == cursor {
			t.Fatalf("id spaces overlap: dead cursor %d was reissued by the new incarnation (ids %v)", cursor, secondIDs)
		}
	}
	if cursor >= secondIDs[0] {
		t.Fatalf("dead cursor %d must sit below the new space's first id %d", cursor, secondIDs[0])
	}

	// The cursor names a position in a sequence this bus never had. It cannot
	// say what the client missed, so the only honest answer is sync_required.
	if got := second.EventsSince(ws, cursor); got != nil {
		t.Fatalf("resuming from a previous incarnation's cursor %d must be a gap, got %d events (ids %v)", cursor, len(got), idsOf(got))
	}

	// NEGATIVE CONTROL. A cursor issued by THIS incarnation must still be
	// served, with the right events — otherwise the fix is "answer nil
	// always", which would resync every ordinary reconnect.
	live := secondIDs[1]
	got := second.EventsSince(ws, live)
	if got == nil {
		t.Fatalf("a cursor issued by this incarnation (%d) must still be servable", live)
	}
	if want := secondIDs[2:]; len(got) != len(want) || idsOf(got)[0] != want[0] {
		t.Fatalf("resuming from %d: want ids %v, got %v", live, want, idsOf(got))
	}
}

func TestFirstIDOfAnIncarnationIsAboveItsBase(t *testing.T) {
	// Guards the wiring rather than the component (team CONVE-19): a base can
	// be computed correctly and then not reach the ID that is actually
	// assigned. Two buses' first IDs must differ, and each must sit inside its
	// own bus's space.
	a, b := New(), New()
	idsA := publishN(a, "ws-1", 1)
	idsB := publishN(b, "ws-1", 1)

	if idsA[0] <= a.base {
		t.Fatalf("first id %d must be above its own incarnation base %d", idsA[0], a.base)
	}
	if idsB[0] <= b.base {
		t.Fatalf("first id %d must be above its own incarnation base %d", idsB[0], b.base)
	}
	if idsA[0] == idsB[0] {
		t.Fatalf("two incarnations assigned the same first id %d", idsA[0])
	}
}

func TestTheAdjacentCursorAcrossIncarnationsIsRefused(t *testing.T) {
	// The one case the stride invariant does NOT cover on its own, and
	// therefore the only case that distinguishes the explicit base check in
	// EventsSince from the coverage check inside replayBuffer.since.
	//
	// since() serves sinceID+1 == knownFrom, reasoning that no ID lies
	// strictly between the cursor and our first event. That is true only
	// WITHIN one ID space. A cursor of exactly `base` is adjacent to this
	// incarnation's first ID (base+1) and belongs to a space that ended — so
	// the adjacency reasoning would hand it a replay across a boundary it
	// cannot see.
	//
	// Reachable only at the stride boundary in production (a previous
	// incarnation would have to have published 2^20 events per millisecond of
	// its life to issue `base`). Pinned anyway: the guard is what makes the
	// case impossible rather than improbable, and a test that only exercised
	// the probable half would pass with the guard deleted.
	bus := New()
	ids := publishN(bus, "ws-1", 1)
	if ids[0] != bus.base+1 {
		t.Fatalf("fixture: first id %d should be base+1 (%d)", ids[0], bus.base+1)
	}

	if got := bus.EventsSince("ws-1", bus.base); got != nil {
		t.Fatalf("a cursor at the base is adjacent to our first id but belongs to a dead space; must be a gap, got %d events", len(got))
	}

	// Control, one ID higher: the first ID this incarnation issued is served.
	if got := bus.EventsSince("ws-1", bus.base+1); got == nil {
		t.Fatal("a cursor at this incarnation's first id must be served")
	}
}

func idsOf(events []Event) []int64 {
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestConcurrentPublishesLeaveTheBufferInIDOrder(t *testing.T) {
	// codex round 18. The id used to be assigned BEFORE the replay lock, so
	// two concurrent publishes could take N and N+1 and append in the other
	// order. replayBuffer.since computes oldest and newest by POSITION, so a
	// buffer holding [N+1, N] reports N as its newest and answers a resume
	// from N+1 with sync_required — a client told to resync at the moment it
	// was exactly current.
	bus := New()
	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		}()
	}
	wg.Wait()

	buffered := bus.EventsSince("ws-1", 0)
	if len(buffered) != n {
		t.Fatalf("expected %d buffered events, got %d", n, len(buffered))
	}
	for i := 1; i < len(buffered); i++ {
		if buffered[i].ID <= buffered[i-1].ID {
			t.Fatalf("buffer out of id order at index %d: %d after %d", i, buffered[i].ID, buffered[i-1].ID)
		}
	}
	// The consequence, asserted rather than inferred: every id in the buffer
	// must be servable as a cursor. Under the race the newest ones were not.
	for _, e := range buffered {
		if got := bus.EventsSince("ws-1", e.ID); got == nil {
			t.Fatalf("cursor %d is inside the buffer and must be servable", e.ID)
		}
	}
}
