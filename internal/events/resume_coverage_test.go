package events

import "testing"

// The BUG-2731 family: a resume answered from a view that cannot actually
// vouch for the span the client is asking about.
//
// Every test here asserts BOTH halves — that sync_required (a nil return) IS
// produced where continuity cannot be proven, and that it is NOT produced
// where it can. The second half is not padding: the failure mode this fix can
// introduce is the inversion, where a bus that cannot distinguish "empty
// because nothing happened" from "empty because we just started" resyncs every
// reconnecting client. A test suite that only pins the first half would pass on
// a bus that answers nil unconditionally.

func TestResumeAgainstAWorkspaceThisProcessHasNeverPublished(t *testing.T) {
	// Case 1: cold buffer. The bus assigns its own IDs from 1 on every start,
	// so a non-zero cursor here belongs to a previous incarnation.
	bus := New()

	if got := bus.EventsSince("ws-1", 4200); got != nil {
		t.Fatalf("resuming from 4200 against a workspace with no buffer must be a gap, got %d events", len(got))
	}

	// Negative control: a FRESH client (no Last-Event-ID) is not resuming from
	// a position and must not be told to resync.
	got := bus.EventsSince("ws-1", 0)
	if got == nil {
		t.Fatal("a fresh client (sinceID=0) must not be answered with a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 events for a fresh client on an empty workspace, got %d", len(got))
	}
}

func TestResumeBelowThisInstancesFirstSeenID(t *testing.T) {
	// The instance under-counted in the original report: a buffer that is
	// NOT full and NOT empty, whose coverage simply starts above the client's
	// cursor. Reachable on any multi-instance deployment — the client saw
	// ws-1 events on another replica before this one began receiving them.
	//
	// Built through the real bus rather than a hand-made buffer, so the
	// global-counter / per-workspace-buffer shape is what is actually being
	// exercised: 49 publishes to another workspace push the counter up, and
	// ws-1's first event lands at ID 50.
	bus := New()
	for i := 0; i < 49; i++ {
		bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-other"})
	}
	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})

	first := bus.EventsSince("ws-1", 0)
	if len(first) != 1 || first[0].ID != 50 {
		t.Fatalf("fixture drifted: expected ws-1's only event at ID 50, got %+v", first)
	}

	// A cursor with room for a missed ws-1 event between it and our first.
	if got := bus.EventsSince("ws-1", 30); got != nil {
		t.Fatalf("resuming from 30 when coverage starts at 50 must be a gap, got %d events", len(got))
	}

	// Negative control, adjacent cursor: no ID lies strictly between 49 and
	// 50, so nothing can have been missed and this MUST be served.
	got := bus.EventsSince("ws-1", 49)
	if got == nil {
		t.Fatal("resuming from 49 when coverage starts at 50 leaves no room for a missed event; must be served, not a gap")
	}
	if len(got) != 1 || got[0].ID != 50 {
		t.Fatalf("expected event 50 replayed, got %+v", got)
	}

	// Negative control, cursor inside the received range: fully caught up.
	got = bus.EventsSince("ws-1", 50)
	if got == nil {
		t.Fatal("a cursor at our newest event is caught up, not a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 events for a caught-up cursor, got %d", len(got))
	}
}

func TestSteadyStateResumesAreUndisturbed(t *testing.T) {
	// The load-posture guard for the whole unit. A client that has been
	// connected to this instance all along must still be served from the
	// buffer, and told it is caught up when it is — no new sync_required.
	bus := New()
	for i := 0; i < 10; i++ {
		bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	got := bus.EventsSince("ws-1", 7)
	if got == nil {
		t.Fatal("a same-incarnation cursor inside the buffer must be served")
	}
	if len(got) != 3 {
		t.Fatalf("expected events 8,9,10, got %d", len(got))
	}

	got = bus.EventsSince("ws-1", 10)
	if got == nil {
		t.Fatal("a cursor at the newest event must be served as caught up, not as a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}
}
