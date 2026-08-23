package events

import (
	"testing"
	"time"
)

// raised reports whether a gap signal is pending, without blocking. Reading it
// CONSUMES the signal, which is what a real consumer's select does.
func raised(gaps <-chan struct{}) bool {
	select {
	case <-gaps:
		return true
	default:
		return false
	}
}

// TestDropSignalsOnlyTheSubscriberItHappenedTo is the per-subscriber half of
// BUG-2730. The control leg carries the claim: a full channel is a fact about
// ONE connection's read rate, so a broadcast would charge a delta sync to
// clients whose stream is intact.
func TestDropSignalsOnlyTheSubscriberItHappenedTo(t *testing.T) {
	b := New()
	defer b.Close()

	slow, slowGaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	fast, fastGaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(slow)
	defer b.Unsubscribe(fast)

	// Exactly fills the slow subscriber's 64-deep channel; the fast one is
	// drained each time, so only one of the two is behind.
	const chanDepth = 64
	for range chanDepth {
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		<-fast
	}
	if raised(slowGaps) {
		t.Fatal("a full-but-not-yet-overflowing channel raised the flag; it must mean a DROP")
	}

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	<-fast

	if !raised(slowGaps) {
		t.Error("the subscriber whose event was dropped was not told")
	}
	if raised(fastGaps) {
		t.Error("a subscriber that received every event was told it had missed one")
	}
}

// TestDropIsReportedToTheObserver pins the metric half. The condition was
// log-only on this bus until BUG-2730 — internal/watchevents has counted it
// since BUG-2699 — so a claim that the fix is measurable in production needs
// this asserted rather than assumed.
func TestDropIsReportedToTheObserver(t *testing.T) {
	b := New()
	defer b.Close()
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)

	for range 64 {
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	}
	if got := obs.dropped(); len(got) != 0 {
		t.Fatalf("reported %d drops before the channel overflowed: %v", len(got), got)
	}

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	got := obs.dropped()
	if len(got) != 1 {
		t.Fatalf("want exactly one drop reported, got %d: %v", len(got), got)
	}
	if got[0] != DropReasonSlowSubscriber {
		t.Errorf("reason = %q, want %q", got[0], DropReasonSlowSubscriber)
	}
}

// TestSubscribeAndReplayIsAtomic is the load-bearing test for the duplicate
// window (BUG-2730's third strand).
//
// The guarantee is that an event is in the replay slice OR delivered on the
// channel, never both. The ONLY interleaving that can distinguish a correct
// implementation from the two-step one the SSE handler used is a publish
// attempted BETWEEN the subscribe and the buffer read, which is what the
// afterSubscribeRegister seam arranges.
//
// The control below runs the SAME interleaving against the two-step sequence
// and asserts it DOES duplicate. Without it this test would pass for an
// implementation whose window simply never opened during the run, and would
// be evidence of nothing.
func TestSubscribeAndReplayIsAtomic(t *testing.T) {
	b := New()
	defer b.Close()

	// One event before the resume, so the cursor names a real position and
	// the replay has something legitimate to carry.
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before"})
	cursor := b.EventsSince("ws-1", 0)[0].ID

	published := make(chan struct{})
	b.afterSubscribeRegister = func() {
		go func() {
			defer close(published)
			// Blocks on b.mu until SubscribeAndReplaySince releases it.
			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "during"})
		}()
		// Long enough for that goroutine to reach the lock and park. If it
		// has not, the test degrades to "publish happened after", which
		// still cannot produce a duplicate — this window only ever makes the
		// test stricter, never falsely green.
		time.Sleep(20 * time.Millisecond)
	}

	ch, missed, _, ok := b.SubscribeAndReplaySince("ws-1", cursor, 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	<-published

	inReplay := containsItem(missed, "during")
	inLive := drainContains(t, ch, "during")

	if inReplay && inLive {
		t.Error("the event published between subscribe and replay was delivered TWICE")
	}
	if !inReplay && !inLive {
		t.Error("the event published between subscribe and replay was LOST")
	}
}

// TestTwoStepSubscribeThenReplayDuplicates is TestSubscribeAndReplayIsAtomic's
// negative control: the sequence the SSE handler used before BUG-2730,
// exercised through the same window, must produce the duplicate the atomic
// version is asserted not to. If this ever stops duplicating, the test above
// has stopped proving anything and both need re-deriving.
func TestTwoStepSubscribeThenReplayDuplicates(t *testing.T) {
	b := New()
	defer b.Close()

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before"})
	cursor := b.EventsSince("ws-1", 0)[0].ID

	ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)

	// The window, made explicit: the old handler did these as two calls with
	// arbitrary scheduling in between.
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "during"})

	missed := b.EventsSince("ws-1", cursor)

	if !containsItem(missed, "during") || !drainContains(t, ch, "during") {
		t.Fatal("the two-step sequence did not reproduce the duplicate window; " +
			"TestSubscribeAndReplayIsAtomic's guarantee is no longer being discriminated")
	}
}

func containsItem(events []Event, itemID string) bool {
	for _, e := range events {
		if e.ItemID == itemID {
			return true
		}
	}
	return false
}

// drainContains reads whatever is already queued on ch without blocking.
func drainContains(t *testing.T, ch chan Event, itemID string) bool {
	t.Helper()
	found := false
	for {
		select {
		case e := <-ch:
			if e.ItemID == itemID {
				found = true
			}
		default:
			return found
		}
	}
}
