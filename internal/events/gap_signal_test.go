package events

import (
	"sync"
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

	// Exactly fills the slow subscriber's channel; the fast one is drained
	// each time, so only one of the two is behind. Derived rather than
	// restated: this test's whole claim is about the boundary AT the depth,
	// so a literal that drifts from the real one would leave it asserting
	// something about an arbitrary number instead.
	for range subscriberChanDepth {
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

	for range subscriberChanDepth {
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
	for i := 1; i <= subscriberChanDepth; i++ {
		b.fanOutLocally(Event{ID: int64(i), Type: ItemUpdated, WorkspaceID: "ws-1"})
		<-fast
	}
	if raised(slowGaps) {
		t.Fatal("a full-but-not-yet-overflowing channel raised the flag")
	}

	b.fanOutLocally(Event{ID: subscriberChanDepth + 1, Type: ItemUpdated, WorkspaceID: "ws-1"})
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

// TestConcurrentPublishesDeliverInIDOrder is the other way a client ends up
// processing an event twice, and it is not the subscribe/replay window.
//
// If ID assignment and fan-out are separate critical sections, two concurrent
// publishes can take IDs N and N+1 and deliver in the other order. A
// subscriber that sees N+1 and then N has a cursor that REGRESSED, so its next
// reconnect replays N+1 a second time — a duplicate this unit's headline
// guarantee says nothing about.
//
// A stress test rather than a seam-driven one: the interleaving is a
// scheduling race with no single point to pause at once the fix is in, because
// the fix is precisely that no such point exists.
//
// WHY IT IS SHAPED LIKE THIS — the obvious shape is the one it replaces, and
// that one reddened CI (BUG-2742). Publishing a large burst while a goroutine
// drains, then guarding vacuity with "at least half must arrive", makes the
// sample size a bet on runner speed: this bus DELIBERATELY drops for a
// subscriber that cannot keep up, so the arrival count is a function of how
// much CPU the reader gets.
//
// So the sample is made untruncatable rather than large. No reader runs during
// the publishes and no more events than the channel is deep are published, so
// a drop cannot occur — which turns the vacuity guard into `count == total`,
// an equality rather than a threshold.
//
// Detection power then comes from REPEATING the round, which is the thing a
// bounded channel could not supply. Do not trade rounds back for a bigger
// burst: the round count was chosen by measuring catch rate against the
// mutation below, and the burst is what could not be measured reliably. See
// BUG-2742 for the figures.
func TestConcurrentPublishesDeliverInIDOrder(t *testing.T) {
	// Enough publishers to interleave, few enough events that the subscriber
	// channel cannot overflow.
	const publishers = 32
	const each = 2
	const total = publishers * each

	// COMPILE-TIME premise check. Deriving `each` from subscriberChanDepth
	// looks tidier and is a trap: integer division silently yields 0 the day
	// the depth drops below `publishers`, and then total is 0, nothing is
	// published, and `count == total` passes as 0 == 0 — the test going
	// permanently vacuous with no signal at all (codex round 2). Stating the
	// numbers and failing the BUILD when they stop being compatible is the
	// version that cannot rot quietly.
	const _ uint = subscriberChanDepth - total

	for round := range 40 {
		func() {
			b := New()
			defer b.Close()

			ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
			if !ok {
				t.Fatal("subscribe refused")
			}
			defer b.Unsubscribe(ch)

			var wg sync.WaitGroup
			for range publishers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range each {
						b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
					}
				}()
			}
			wg.Wait()

			// Every publish has returned, so everything that will ever be in
			// the channel is in it now: drain without blocking and without a
			// timer. Nothing in the ASSERTION path waits on a goroutine being
			// scheduled, which is what stops load from truncating the sample.
			// It does not make detection deterministic: whether a broken bus
			// actually interleaves on a given round is still up to the
			// scheduler, which is why the round count above exists and why
			// the header quotes a measured catch rate rather than a proof.
			//
			// CONTIGUOUS, not merely increasing (codex round 2): one bus, one
			// workspace and nothing else publishing means the ids assigned are
			// exactly base+1..base+total, so the delivered sequence must have
			// no holes. "Increasing" would also be satisfied by a bus that
			// dropped some of these and delivered something else with a bigger
			// id, which is a pass for the wrong reason.
			var prev int64
			var count int
		drain:
			for {
				select {
				case e := <-ch:
					count++
					if prev != 0 && e.ID != prev+1 {
						t.Fatalf("round %d: ids arrived as %d after %d — consecutive publishes on one "+
							"bus must deliver consecutively; a subscriber whose cursor goes backwards "+
							"replays on its next reconnect, and one that skips has a hole nothing "+
							"will fill", round, e.ID, prev)
					}
					prev = e.ID
				default:
					break drain
				}
			}

			// PREMISE, not a tolerance: with no reader draining and no more
			// events published than the channel is deep, a drop is impossible
			// — the channel starts empty and every publish fits.
			// If this ever fires, the ordering assertion above ran on a
			// truncated sample and proved less than it claims — which is the
			// defect this test was rewritten to remove.
			if count != total {
				t.Fatalf("round %d: %d of %d events arrived; with a %d-deep channel and no "+
					"concurrent reader nothing may be dropped, so the ordering check above "+
					"ran on a sample it cannot vouch for", round, count, total, subscriberChanDepth)
			}
		}()
	}
}

// TestActivityGapSignalCoalesces is the load bound on THIS bus. The watch
// bus's twin existed from the start; this one did not, and the bound is the
// whole answer to "does telling a slow subscriber make it slower".
func TestActivityGapSignalCoalesces(t *testing.T) {
	b := New()
	defer b.Close()

	ch, gaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)

	for range 200 {
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	if !raised(gaps) {
		t.Fatalf("no gap signal after 200 events into a %d-deep channel", subscriberChanDepth)
	}
	if raised(gaps) {
		t.Error("more than one signal was queued; the channel must coalesce, not accumulate")
	}
}

// TestRedisBusSubscribeAndReplayIsAtomic is the multi-instance twin of
// TestSubscribeAndReplayIsAtomic. It gets its own test because the two
// implementations reach the guarantee by different means — this one keeps
// subscribers and buffers under a single mutex, so a future refactor that
// split them would break here and nowhere else.
func TestRedisBusSubscribeAndReplayIsAtomic(t *testing.T) {
	b := newTestRedisBus(t)

	// A holder first: this bus builds a workspace's replay buffer as part of
	// COVERING it, and a resume against a workspace it has never covered is
	// answered "cannot vouch" — correctly, and not what this test is about.
	holder, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(holder)

	b.fanOutLocally(Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before-1"})
	b.fanOutLocally(Event{ID: 11, Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "before-2"})

	// THE INTERLEAVING IS THE TEST. Publishing only before the call and after
	// it returns proves nothing: a version that released the lock between
	// registering and reading the buffer would pass that. The seam attempts a
	// fan-out from inside the critical section, which is the one moment the
	// guarantee is observable.
	published := make(chan struct{})
	b.afterSubscribeRegister = func() {
		go func() {
			defer close(published)
			// Blocks on b.mu until SubscribeAndReplaySince releases it.
			b.fanOutLocally(Event{ID: 12, Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "during"})
		}()
		// Long enough for that goroutine to reach the lock and park. If it
		// has not, the publish simply lands after, which cannot produce a
		// duplicate — the window only ever makes this stricter.
		time.Sleep(20 * time.Millisecond)
	}

	ch, missed, gaps, ok := b.SubscribeAndReplaySince("ws-1", 10, 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	<-published

	if gaps == nil {
		t.Error("no gap channel returned")
	}
	if !containsItem(missed, "before-2") {
		t.Fatalf("the event above the cursor was not replayed: %+v", missed)
	}
	if containsItem(missed, "before-1") {
		t.Error("the event AT the cursor was replayed; the client already has it")
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

// TestResumeIsSubjectToTheWorkspaceLimit closes a hole the new API could have
// opened: the resume path is a second door onto the same subscriber set, and a
// client that supplies Last-Event-ID must not thereby skip the bound that a
// fresh one is held to.
func TestResumeIsSubjectToTheWorkspaceLimit(t *testing.T) {
	b := New()
	defer b.Close()

	first, _, ok := b.SubscribeIfAllowed("ws-1", 1)
	if !ok {
		t.Fatal("the first subscribe was refused")
	}
	defer b.Unsubscribe(first)

	if _, _, _, ok := b.SubscribeAndReplaySince("ws-1", 1, 1); ok {
		t.Error("a resuming client was admitted past the per-workspace limit")
	}
	// Control: the same call succeeds when there is room, so the refusal above
	// is the limit and not the method being broken.
	if _, _, _, ok := b.SubscribeAndReplaySince("ws-1", 1, 2); !ok {
		t.Error("a resuming client was refused with room to spare")
	}
}

// TestAtomicResumeReportsAnUnservableSpan pins the observer report on the NEW
// path. EventsSince has always reported; this method reads the buffer directly
// and had to carry its own, and the report is what makes the resync population
// visible in production.
func TestAtomicResumeReportsAnUnservableSpan(t *testing.T) {
	b := New()
	defer b.Close()
	obs := &recordingObserver{}
	b.SetObserver(obs)

	// A cursor for a workspace this process has never published to.
	ch, missed, _, ok := b.SubscribeAndReplaySince("ws-unknown", b.base+9999, 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch)
	if missed != nil {
		t.Fatalf("a cursor for an unknown workspace was answered as servable: %+v", missed)
	}
	if got := obs.gaps(); len(got) != 1 || got[0] != "ws-unknown" {
		t.Errorf("resume gaps reported = %v, want exactly [ws-unknown]", got)
	}

	// Control: a FRESH subscription on the same path reports nothing. Without
	// this leg the report could fire on every subscribe and still pass.
	obs2 := &recordingObserver{}
	b.SetObserver(obs2)
	ch2, _, _, ok := b.SubscribeAndReplaySince("ws-unknown", 0, 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(ch2)
	if got := obs2.gaps(); len(got) != 0 {
		t.Errorf("a fresh subscription reported a resume gap: %v", got)
	}
}

// TestRedisBusDropIsReportedPerSubscriber pins two things the single-subscriber
// tests cannot: that the Redis fan-out reports drops at all, and that it
// reports once per DROPPED SUBSCRIBER rather than once per publish. A report
// hoisted out of the loop would halve the counter on a two-slow-subscriber
// workspace and look right on every other test.
func TestRedisBusDropIsReportedPerSubscriber(t *testing.T) {
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	slowA, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	slowB, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	defer b.Unsubscribe(slowA)
	defer b.Unsubscribe(slowB)

	// Fill both channels, then overflow both with ONE event.
	for i := 1; i <= subscriberChanDepth; i++ {
		b.fanOutLocally(Event{ID: int64(i), Type: ItemUpdated, WorkspaceID: "ws-1"})
	}
	if got := obs.dropped(); len(got) != 0 {
		t.Fatalf("drops reported before either channel overflowed: %v", got)
	}

	b.fanOutLocally(Event{ID: subscriberChanDepth + 1, Type: ItemUpdated, WorkspaceID: "ws-1"})

	got := obs.dropped()
	if len(got) != 2 {
		t.Fatalf("one event dropped for TWO subscribers must report twice, got %d: %v", len(got), got)
	}
	for _, r := range got {
		if r != DropReasonSlowSubscriber {
			t.Errorf("reason = %q, want %q", r, DropReasonSlowSubscriber)
		}
	}
}

// TestGapChannelOutlivesUnsubscribe pins the lifetime rule stated on the field.
// Closing the gap channel would make it permanently ready, and a consumer
// selecting on both would spin at full speed between Unsubscribe and noticing
// the event channel had closed.
func TestGapChannelOutlivesUnsubscribe(t *testing.T) {
	b := New()
	defer b.Close()

	ch, gaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("subscribe refused")
	}
	b.Unsubscribe(ch)

	if _, open := <-ch; open {
		t.Fatal("Unsubscribe must close the event channel; the rest of this test assumes it")
	}
	select {
	case <-gaps:
		t.Error("the gap channel was closed or signalled by Unsubscribe; a consumer's select would spin")
	default:
	}
}

// TestEverySubscribeAPIReturnsAGapChannel is the small structural claim behind
// the seam: no way of registering a subscriber may hand back a nil signal,
// because a nil channel swallows every send through the default arm and the
// subscriber would be silently unreachable.
func TestEverySubscribeAPIReturnsAGapChannel(t *testing.T) {
	for name, sub := range map[string]func(b *MemoryBus) (chan Event, <-chan struct{}){
		"Subscribe": func(b *MemoryBus) (chan Event, <-chan struct{}) {
			return b.Subscribe("ws-1")
		},
		"SubscribeIfAllowed": func(b *MemoryBus) (chan Event, <-chan struct{}) {
			ch, gaps, _ := b.SubscribeIfAllowed("ws-1", 0)
			return ch, gaps
		},
		"SubscribeAndReplaySince": func(b *MemoryBus) (chan Event, <-chan struct{}) {
			ch, _, gaps, _ := b.SubscribeAndReplaySince("ws-1", 0, 0)
			return ch, gaps
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := New()
			defer b.Close()
			ch, gaps := sub(b)
			defer b.Unsubscribe(ch)
			if gaps == nil {
				t.Errorf("%s returned a nil gap channel; every drop for this subscriber would be silent", name)
			}
		})
	}
}
