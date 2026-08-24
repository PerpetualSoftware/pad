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
	// Every reason this bus can emit, so an adapter that collapsed them onto
	// one series would fail here rather than in production. The counts differ
	// on purpose: identical counts would pass on an adapter that ignored the
	// label entirely and let one series absorb them all.
	obs.SequenceReset(events.ResetReasonEpochChange)
	obs.SequenceReset(events.ResetReasonEpochChange)
	obs.SequenceReset(events.ResetReasonEpochChange)
	obs.SequenceReset(events.ResetReasonCounterBackward)
	obs.SequenceReset(events.ResetReasonCounterBackward)
	obs.SequenceReset(events.ResetReasonEpochRegressed)
	obs.SequenceReset(events.ResetReasonUndecodableMessage)
	obs.SequenceReset(events.ResetReasonUndecodableMessage)
	obs.SequenceReset(events.ResetReasonUndecodableMessage)
	obs.SequenceReset(events.ResetReasonUndecodableMessage)
	obs.SequenceReset(events.ResetReasonUndecodableMessage)
	// The comment above claimed "every reason this bus can emit" while this one
	// was missing (codex round 8). Production emits it from
	// confirmSubscription's late-acknowledgement path, so the mapping was
	// unexercised — and the zero-assertion further down proved only that the
	// dedicated counter does not leak INTO this series, which is a different
	// claim.
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)
	obs.SequenceReset(events.ResetReasonSubscriptionUnconfirmed)

	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()
	obs.ReceiveLoopExited()

	// Counted on its OWN counter, not folded into the reset series. The two
	// answer different questions — a reset says coverage ended, this says a
	// stream was admitted whose coverage is undescribable — and an adapter that
	// merged them would satisfy a total-only assertion while destroying the
	// distinction an operator acts on (BUG-2747).
	obs.SubscriptionUnconfirmed()
	obs.SubscriptionUnconfirmed()

	// Same argument again for BUG-2738's pair, which arrived after the comment
	// above and needs the same protection: idle_timeout is a reset REASON and
	// SubscriptionCycled is its OWN counter, and they deliberately disagree —
	// the reason only fires when a buffer existed to drop, the counter fires on
	// every replacement. An adapter that folded the counter into the reset
	// series, or mapped the new reason onto an existing one, would satisfy a
	// total-only assertion while destroying exactly that distinction.
	obs.SequenceReset(events.ResetReasonIdleTimeout)
	obs.SequenceReset(events.ResetReasonIdleTimeout)
	obs.SequenceReset(events.ResetReasonIdleTimeout)
	obs.SequenceReset(events.ResetReasonIdleTimeout)
	obs.SequenceReset(events.ResetReasonIdleTimeout)
	obs.SequenceReset(events.ResetReasonIdleTimeout)

	obs.SubscriptionCycled()
	obs.SubscriptionCycled()
	obs.SubscriptionCycled()
	obs.SubscriptionCycled()
	obs.SubscriptionCycled()
	obs.SubscriptionCycled()
	obs.SubscriptionCycled()

	// A THIRD count distinct from both its neighbours. These three say
	// different things and an operator acts on the difference: cycled means a
	// connection was replaced, idle_timeout means coverage ended, and this one
	// means DETECTION IS DEGRADED because the probe never went out. An adapter
	// that merged any pair of them would satisfy a total-only assertion while
	// destroying exactly that distinction.
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()
	obs.HeartbeatPublishFailed()

	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 2)
	// The reason must land on a LABELLED series, not on the bare counter: an
	// adapter that dropped the label would satisfy a total-only assertion and
	// silently merge the reasons BUG-2736 added.
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonSubscriptionResumed}, 4)
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonEpochChange}, 3)
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonCounterBackward}, 2)
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonEpochRegressed}, 1)
	assertCounter(t, m, "pad_event_subscription_unconfirmed_total", nil, 2)
	// ...and the two stay SEPARATE. The counts differ on purpose — 2 on the
	// dedicated counter, 8 on the reset reason — so an adapter that merged them
	// cannot satisfy both, which a zero-versus-nonzero pair could not establish
	// once the reason itself started being emitted here.
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonSubscriptionUnconfirmed}, 8)
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonUndecodableMessage}, 5)
	assertCounter(t, m, "pad_event_sequence_resets_total",
		map[string]string{"reason": events.ResetReasonIdleTimeout}, 6)
	assertCounter(t, m, "pad_event_subscription_cycled_total", nil, 7)
	assertCounter(t, m, "pad_event_heartbeat_publish_failures_total", nil, 9)
	// The counter must not leak into the reset series either, the same half
	// that a merged-counter adapter would pass without.
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
	// Read the id the bus actually assigned rather than spelling one out:
	// since BUG-2736 the counter starts from this incarnation's base, so a
	// literal 1 is a cursor from a DEAD space and would be answered as a gap
	// — turning this negative control into the positive case by accident.
	published := bus.EventsSince("ws-1", 0)
	if len(published) != 1 {
		t.Fatalf("fixture: expected one buffered event, got %d", len(published))
	}
	if got := bus.EventsSince("ws-1", published[0].ID); got == nil {
		t.Fatal("a caught-up cursor must be served")
	}
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 0)

	// A resume this instance genuinely cannot serve: a cursor from THIS
	// incarnation (so the incarnation guard is not what answers it) naming a
	// workspace with no buffer.
	cursor := published[0].ID + 4200
	if got := bus.EventsSince("ws-never-seen", cursor); got != nil {
		t.Fatalf("expected a gap, got %d events", len(got))
	}
	assertCounter(t, m, "pad_event_resume_gaps_total", nil, 1)
}
