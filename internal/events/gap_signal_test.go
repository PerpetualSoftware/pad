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

	// TWO events before the resume: the cursor names the first, so the second
	// is what the replay must legitimately carry. One would not do — a cursor
	// at the only event's id leaves nothing above it, and the replay would be
	// correctly empty either way.
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before-1"})
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before-2"})
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

	// The seeded event MUST be in the replay. Without this leg the test would
	// pass for an implementation that always answers "cannot vouch" (nil
	// missed) and delivers everything live — which satisfies "never both" by
	// never replaying anything (codex round 3).
	if !containsItem(missed, "before-2") {
		t.Fatalf("the event above the cursor was not replayed; missed = %+v", missed)
	}

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

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before-1"})
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

// TestRedisBusDropSignalsOnlyTheSubscriberItHappenedTo is the per-subscriber
// scope on the multi-instance implementation, which keeps subscribers indexed
// by workspace and so has its own way to get the scope wrong.
func TestRedisBusDropSignalsOnlyTheSubscriberItHappenedTo(t *testing.T) {
	b := newTestRedisBus(t)

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

	// Contiguous ids so no coverage arm can fire and take credit.
	for i := 1; i <= 64; i++ {
		b.fanOutLocally(Event{ID: int64(i), Type: ItemUpdated, WorkspaceID: "ws-1"})
		<-fast
	}
	if raised(slowGaps) {
		t.Fatal("a full-but-not-yet-overflowing channel raised the flag")
	}

	b.fanOutLocally(Event{ID: 65, Type: ItemUpdated, WorkspaceID: "ws-1"})
	<-fast

	if !raised(slowGaps) {
		t.Error("the subscriber whose event was dropped was not told")
	}
	if raised(fastGaps) {
		t.Error("a subscriber that received every event was told it had missed one")
	}
}

// TestCoverageDropSignalsOnlyThatWorkspace pins the per-INSTANCE scope on the
// activity bus. Ending a workspace's coverage makes its next RESUME honest and
// says nothing to a client still holding the stream open, which is the case
// this signal exists for — but a dropped subscription says nothing about any
// OTHER workspace's channel, so the control leg is a second workspace's
// subscriber staying quiet.
func TestCoverageDropSignalsOnlyThatWorkspace(t *testing.T) {
	b := newTestRedisBus(t)

	affected, affectedGaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	bystander, bystanderGaps, ok := b.SubscribeIfAllowed("ws-2", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(affected)
	defer b.Unsubscribe(bystander)

	// A buffer must exist, or dropWorkspaceCoverage returns early with
	// nothing to end — and the test would pass for a bus that never signals.
	b.fanOutLocally(Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})
	<-affected
	if raised(affectedGaps) {
		t.Fatal("an ordinary event raised the coverage flag")
	}

	b.dropWorkspaceCoverage("ws-1", ResetReasonSubscriptionResumed, b.currentSubGen("ws-1"))

	if !raised(affectedGaps) {
		t.Error("a subscriber whose workspace coverage ended was not told")
	}
	if raised(bystanderGaps) {
		t.Error("a subscriber on an unaffected workspace was told it had missed something")
	}
}

// TestIDSpaceResetSignalsEveryWorkspace is the other per-instance scope: an
// ID-space change invalidates every buffer at once, so every subscriber has a
// hole. This is the case where the bystander in the test above SHOULD be told,
// which is what makes the two tests a pair rather than a repetition.
func TestIDSpaceResetSignalsEveryWorkspace(t *testing.T) {
	b := newTestRedisBus(t)

	one, oneGaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	two, twoGaps, ok := b.SubscribeIfAllowed("ws-2", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(one)
	defer b.Unsubscribe(two)

	b.fanOutLocally(Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})
	<-one
	b.fanOutLocally(Event{ID: 11, Type: ItemUpdated, WorkspaceID: "ws-2"})
	<-two
	if raised(oneGaps) || raised(twoGaps) {
		t.Fatal("ordinary events raised the flag")
	}

	b.dropAllBuffers(floorRaise)

	if !raised(oneGaps) || !raised(twoGaps) {
		t.Error("an ID-space reset invalidates every buffer, so every subscriber must be told")
	}
}

// TestCoverageDropWithNoBufferStillSignals is codex round 4's P1. A subscriber
// that connected and then sat through a pub/sub outage before ANY event was
// received for its workspace has the largest possible hole and the least
// evidence of it — and the early return that (correctly) suppresses the reset
// metric was returning before the subscribers were told.
//
// The control is the metric: the reset must stay unreported, because there was
// no coverage to end. Without that leg the obvious "fix" of deleting the early
// return would pass.
func TestCoverageDropWithNoBufferStillSignals(t *testing.T) {
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, gaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)

	// Deliberately no fanOutLocally: this workspace has no replay buffer.
	b.dropWorkspaceCoverage("ws-1", ResetReasonSubscriptionResumed, b.currentSubGen("ws-1"))

	if !raised(gaps) {
		t.Error("a subscriber that sat through an outage before any event arrived was not told")
	}
	if got := obs.resetReasons(); len(got) != 0 {
		t.Errorf("a reset was reported for a workspace with no coverage to end: %v", got)
	}
}
