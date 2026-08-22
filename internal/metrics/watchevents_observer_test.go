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
