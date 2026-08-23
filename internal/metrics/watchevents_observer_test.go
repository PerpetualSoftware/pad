package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TestWatchEventsObserverMapsEachEventToItsOwnCounter is the ADAPTER
// test codex round 5 found missing.
//
// Both sides of this seam are tested: internal/watchevents proves the bus
// calls the observer, and the metric names are pinned here. Neither
// proves the MAPPING — an adapter that incremented the resume counter on
// a sequence gap, or dropped ResumeGap entirely, would pass both suites
// and quietly send an incident the wrong way.
//
// Each event is fired a DIFFERENT number of times, so a mapping that
// crosses two wires cannot produce the expected totals by coincidence.
func TestWatchEventsObserverMapsEachEventToItsOwnCounter(t *testing.T) {
	t.Parallel()

	m := New()
	obs := NewWatchEventsObserver(m)

	obs.NotificationDropped(watchevents.DropReasonSlowSubscriber)

	obs.SequenceGap(3)
	obs.SequenceGap(4)

	obs.ResumeGap()
	obs.ResumeGap()
	obs.ResumeGap()

	obs.SequenceReset(watchevents.ResetReasonEpochChange)
	obs.SequenceReset(watchevents.ResetReasonCounterBackward)
	obs.SequenceReset(watchevents.ResetReasonCounterBackward)

	obs.ReceiveLoopExited()

	assertCounter(t, m, "pad_watchevents_notifications_dropped_total",
		map[string]string{"reason": watchevents.DropReasonSlowSubscriber}, 1)
	assertCounter(t, m, "pad_watchevents_sequence_gaps_total", nil, 2)
	// 3+4 — the SPAN of the gaps, not their count. The two counters
	// exist precisely so one gap of seven and seven gaps of one are
	// distinguishable, and equal values here would hide a mapping that
	// incremented both from the same place.
	assertCounter(t, m, "pad_watchevents_notifications_missed_total", nil, 7)
	assertCounter(t, m, "pad_watchevents_resume_gaps_total", nil, 3)
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": watchevents.ResetReasonEpochChange}, 1)
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": watchevents.ResetReasonCounterBackward}, 2)
	assertCounter(t, m, "pad_watchevents_receive_loop_exits_total", nil, 1)
}

// TestWatchEventsObserverIgnoresNonPositiveGapSpans pins the documented
// asymmetry: the gap EVENT always counts, the span only when positive.
// The bus only calls SequenceGap on a forward jump, so a non-positive
// span means the caller's arithmetic changed — swallowing the event too
// would hide that regression along with the gap.
func TestWatchEventsObserverIgnoresNonPositiveGapSpans(t *testing.T) {
	t.Parallel()

	m := New()
	obs := NewWatchEventsObserver(m)

	obs.SequenceGap(0)
	obs.SequenceGap(-5)

	assertCounter(t, m, "pad_watchevents_sequence_gaps_total", nil, 2)
	assertCounter(t, m, "pad_watchevents_notifications_missed_total", nil, 0)
}

func assertCounter(t *testing.T, m *Metrics, name string, labels map[string]string, want float64) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			if !labelsMatch(metric.GetLabel(), labels) {
				continue
			}
			if got := metric.GetCounter().GetValue(); got != want {
				t.Errorf("%s%v = %v, want %v", name, labels, got, want)
			}
			return
		}
		if want == 0 {
			return // absent series is a zero value
		}
		t.Errorf("%s has no series matching %v", name, labels)
		return
	}
	if want == 0 {
		return
	}
	t.Errorf("%s is not exported", name)
}

func labelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(want) != len(pairs) {
		return false
	}
	for _, p := range pairs {
		if want[p.GetName()] != p.GetValue() {
			return false
		}
	}
	return true
}

// TestWatchEventsResetReasonsReachTheMetricWithTheirWireSPELLING covers the
// two reasons BUG-2739 added, and does it with LITERAL strings rather than
// the exported constants.
//
// That is the whole point of the test and the reason it does not simply
// extend the table above (codex round 7). Every other assertion in this file
// derives its expected label from the same constant the code emits, so the
// pair moves together: rename the constant and the suite stays green while
// every operator dashboard breaks. A metric label is a WIRE FORMAT with
// consumers outside this repo, and a wire format is pinned by writing it out.
//
// counter_backward is included for the same reason — its spelling was
// CHANGED on this branch (it was counter_backwards), and nothing in the tree
// would have noticed either the change or a revert of it.
func TestWatchEventsResetReasonsReachTheMetricWithTheirWireSpelling(t *testing.T) {
	t.Parallel()

	m := New()
	obs := NewWatchEventsObserver(m)

	obs.SequenceReset(watchevents.ResetReasonSubscriptionResumed)
	obs.SequenceReset(watchevents.ResetReasonUndecodableMessage)
	obs.SequenceReset(watchevents.ResetReasonUndecodableMessage)
	obs.SequenceReset(watchevents.ResetReasonCounterBackward)
	obs.SequenceReset(watchevents.ResetReasonEpochChange)

	// Literal, deliberately. Do not replace these with the constants.
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": "subscription_resumed"}, 1)
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": "undecodable_message"}, 2)
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": "counter_backward"}, 1)
	assertCounter(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": "epoch_change"}, 1)

	// AND THE SPELLING THAT WAS RETIRED IS NOT ALSO BEING EMITTED. Without
	// this leg the assertions above pass on an adapter that emits both, which
	// is what a "compatibility shim" fix for the rename would look like — a
	// double-counting bus that satisfies old and new dashboards at once.
	if got := counterValue(t, m, "pad_watchevents_sequence_resets_total",
		map[string]string{"reason": "counter_backwards"}); got != 0 {
		t.Fatalf("the retired plural spelling must not be emitted, got %v", got)
	}
}

// counterValue reads one labelled counter, answering 0 when the series does
// not exist — which is the state an absence assertion wants, and which
// assertCounter cannot express because it fails on a missing series.
func counterValue(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if !labelsMatch(metric.GetLabel(), labels) {
				continue
			}
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}
