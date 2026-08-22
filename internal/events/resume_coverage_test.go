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
	// Case 1: cold buffer — a workspace this process has never published to,
	// which is a cold start, a restart, or a scale-up.
	//
	// THE CURSOR IS BASE-RELATIVE, and that is what keeps this test about the
	// no-buffer branch. Since BUG-2736 a small literal like 4200 sits below
	// this incarnation's base and is refused one level up by the incarnation
	// guard, so the branch this test is named for would never be reached and
	// deleting it would leave the test green.
	bus := New()
	cursor := bus.base + 4200

	if got := bus.EventsSince("ws-1", cursor); got != nil {
		t.Fatalf("resuming from %d against a workspace with no buffer must be a gap, got %d events", cursor, len(got))
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
	other := publishN(bus, "ws-other", 49)
	ours := publishTyped(bus, "ws-1", ItemCreated)
	coverageStart := ours[0]

	first := bus.EventsSince("ws-1", 0)
	if len(first) != 1 || first[0].ID != coverageStart {
		t.Fatalf("fixture drifted: expected ws-1's only event at ID %d, got %+v", coverageStart, first)
	}
	// The fixture's own premise: ws-1's first ID must be the immediate
	// successor of the last ws-other ID, or the adjacent-cursor control below
	// is not testing adjacency.
	if adjacent := other[48]; adjacent+1 != coverageStart {
		t.Fatalf("fixture drifted: ws-other's last id %d is not adjacent to ws-1's first %d", adjacent, coverageStart)
	}

	// A cursor with room for a missed ws-1 event between it and our first.
	if got := bus.EventsSince("ws-1", other[29]); got != nil {
		t.Fatalf("resuming from %d when coverage starts at %d must be a gap, got %d events", other[29], coverageStart, len(got))
	}

	// Negative control, adjacent cursor: no ID lies strictly between it and
	// our first event, so nothing can have been missed and this MUST be served.
	got := bus.EventsSince("ws-1", other[48])
	if got == nil {
		t.Fatalf("resuming from %d when coverage starts at %d leaves no room for a missed event; must be served, not a gap", other[48], coverageStart)
	}
	if len(got) != 1 || got[0].ID != coverageStart {
		t.Fatalf("expected event %d replayed, got %+v", coverageStart, got)
	}

	// Negative control, cursor inside the received range: fully caught up.
	got = bus.EventsSince("ws-1", coverageStart)
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
	ids := publishN(bus, "ws-1", 10)

	got := bus.EventsSince("ws-1", ids[6])
	if got == nil {
		t.Fatal("a same-incarnation cursor inside the buffer must be served")
	}
	if len(got) != 3 {
		t.Fatalf("expected events %v, got %d", ids[7:], len(got))
	}

	got = bus.EventsSince("ws-1", ids[9])
	if got == nil {
		t.Fatal("a cursor at the newest event must be served as caught up, not as a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}
}
