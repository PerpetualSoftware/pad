package metrics

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// The ADAPTER test: internal/events proves the bus calls the observer, and the
// metric names are pinned here. Neither proves the MAPPING — an adapter that
// incremented the reset counter on a resume gap would pass both suites and
// send an incident the wrong way. Different counts per wire, so a crossed
// mapping cannot produce the expected totals by coincidence.
func TestEventsObserverMapsEachEventToItsOwnCounter(t *testing.T) {
	t.Parallel()

	m := New()
	obs := NewEventsObserver(m)

	obs.ResumeGap("ws-1")
	obs.ResumeGap("ws-2")

	obs.SequenceReset(events.ResetReasonSubscriptionResumed)
	obs.SequenceReset(events.ResetReasonSubscriptionResumed)
	obs.SequenceReset(events.ResetReasonSubscriptionResumed)
	obs.SequenceReset(events.ResetReasonSubscriptionResumed)

	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()

	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 2)
	// The reason must land on a LABELLED series, not on the bare counter: an
	// adapter that dropped the label would satisfy a total-only assertion and
	// silently merge reasons BUG-2736 will add.
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonSubscriptionResumed}, 4)
	assertCounter(t, m, "pad_event_receive_loop_exits_total", nil, 5)
}

// TestResumeGapsAreNotLabelledByWorkspace pins a deliberate omission. The bus
// passes the workspace ID and the adapter drops it: workspace count is
// unbounded and operator-controlled, so a per-workspace label is a cardinality
// bomb. Without this test, "helpfully" adding the label later looks like an
// improvement and passes everything else.
func TestResumeGapsAreNotLabelledByWorkspace(t *testing.T) {
	t.Parallel()

	m := New()
	obs := NewEventsObserver(m)
	obs.ResumeGap("ws-1")
	obs.ResumeGap("ws-2")
	obs.ResumeGap("ws-3")

	// Three different workspaces, one series, value 3. A labelled
	// implementation would have three series of 1 and no unlabelled series
	// for assertCounter to find.
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 3)
}

// The bus-to-adapter-to-registry chain end to end: a real MemoryBus, a real
// EventsObserver, a real registry, so a bus that never calls its observer or an
// adapter that writes nowhere cannot pass.
//
// IT DOES NOT COVER THE PRODUCTION WIRING — it attaches the observer itself, so
// it stays green with cmd/pad's SetObserver call deleted. That binding is
// CONVE-19's real subject and lives in cmd/pad's TestBothEventBusShapesReportToMetrics,
// which is the only test that fails when the wiring goes missing.
func TestARealBusMovesTheRealCounter(t *testing.T) {
	t.Parallel()

	m := New()
	bus := events.New()
	bus.SetObserver(NewEventsObserver(m))

	// Negative control FIRST, so a counter that was already non-zero for
	// some unrelated reason cannot be mistaken for this test's work.
	bus.Publish(events.Event{Type: events.ItemCreated, WorkspaceID: "ws-1"})
	if got := bus.EventsSince("ws-1", 1); got == nil {
		t.Fatal("a caught-up cursor must be served")
	}
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 0)

	// A resume this instance genuinely cannot serve.
	if got := bus.EventsSince("ws-never-seen", 4200); got != nil {
		t.Fatalf("expected a gap, got %d events", len(got))
	}
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 1)
}
