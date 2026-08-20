package watchevents

// BUG-2651, the trailing gap — lead ruling: a silently lost nudge is unbounded
// staleness, a spurious resync costs one redundant fetch, so the gap must not
// survive. These run against miniredis because the check's whole mechanism is
// asking the shared counter, which is not a thing a hermetic fixture has.
//
// The settle window is the interesting part. The counter legitimately runs
// ahead of any instance for a moment after every publish (INCR happens inside
// the script; the message still has to propagate), so a strict comparison would
// resync on ordinary traffic. Waiting out propagation and re-reading turns an
// unprincipled "how many ids behind is too many" threshold into a time bound:
// in-flight ids arrive, missed ones never do.

import (
	"testing"
	"time"
)

// TestResumeReportsAGapWhenThisInstanceMissedTheTail is the case the local
// bookkeeping structurally cannot see: a hole at the END of the sequence, with
// no later id to reveal it.
//
// Simulated by advancing the shared counter WITHOUT publishing — which is
// exactly what a message this instance missed looks like from here: the
// authority has moved on and nothing arrived.
func TestResumeReportsAGapWhenThisInstanceMissedTheTail(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	// Id 2 exists as far as Redis is concerned, and never reached us.
	if err := mr.Set(redisWatchSeqKey, "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	_, missed := b.SubscribeAndReplaySince(1)
	if missed != nil {
		t.Fatalf("a resume from 1 must report a gap when id 2 exists and we never saw it; got %+v", missed)
	}
}

// TestResumeDoesNotReportAGapWhenTheInstanceIsCurrent is the control leg, and
// the one that stops the check being "always resync". Without it, a build that
// reported a gap on every reconnect would pass the test above.
func TestResumeDoesNotReportAGapWhenTheInstanceIsCurrent(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	for i := 0; i < 3; i++ {
		b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	}
	for i := 0; i < 3; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("precondition: only %d of 3 notifications arrived", i)
		}
	}
	b.Unsubscribe(ch)

	_, missed := b.SubscribeAndReplaySince(1)
	if missed == nil {
		t.Fatal("this instance has seen everything the counter knows about; a resume must replay, not resync")
	}
	if len(missed) != 2 {
		t.Errorf("resume from 1: got %d entries, want ids 2 and 3: %+v", len(missed), missed)
	}
}

// TestResumeToleratesAnInFlightCounter is the settle window doing its job.
//
// The counter is ahead when the resume starts and the "missing" notification
// arrives DURING the settle beat — which is what ordinary propagation looks
// like. A strict comparison would call that a gap and resync a client that
// missed nothing.
func TestResumeToleratesAnInFlightCounter(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	// Counter says 2 exists; the message is "in flight" and lands mid-settle.
	if err := mr.Set(redisWatchSeqKey, "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-2"})
	}()

	_, missed := b.SubscribeAndReplaySince(1)
	if missed == nil {
		t.Fatal("the id arrived during the settle window, so nothing was missed; " +
			"reporting a gap here would resync on ordinary in-flight traffic")
	}
	if len(missed) != 1 || missed[0].ID != 2 {
		t.Errorf("resume from 1: got %+v, want just id 2", missed)
	}
}

// TestResumeReportsAGapWhenTheCounterAdvancesAgainDuringTheSettle — codex
// round 11, P1. The first version of the check re-read only the LOCAL side, so
// it compared against a stale counter snapshot: id 2 arrives during the beat,
// the captured remote was 2, and it declared convergence — while id 3 had been
// published and missed in the meantime.
func TestResumeReportsAGapWhenTheCounterAdvancesAgainDuringTheSettle(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	if err := mr.Set(redisWatchSeqKey, "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		// The id the first read was waiting for lands...
		b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-2"})
		// ...while a THIRD is published elsewhere and never reaches us.
		_ = mr.Set(redisWatchSeqKey, "3")
	}()

	_, missed := b.SubscribeAndReplaySince(1)
	if missed != nil {
		t.Fatalf("id 3 exists and never reached this instance; the resume must report a gap "+
			"rather than converge against a stale counter snapshot; got %+v", missed)
	}
}

// TestResumeDoesNotResyncWhenTheCounterReadRacedAPublish — codex round 11, P2,
// the other direction. A GET can land just before a publish completes and
// return a value BELOW what this instance already holds. Comparing against
// that stale snapshot forever meant an unnecessary full resync for a client
// that had missed nothing.
func TestResumeDoesNotResyncWhenTheCounterReadRacedAPublish(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("precondition: only %d of 2 notifications arrived", i)
		}
	}
	b.Unsubscribe(ch)

	// The counter reads BEHIND what we hold — the shape a read racing a
	// publish produces. It catches up during the settle window.
	if err := mr.Set(redisWatchSeqKey, "1"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		_ = mr.Set(redisWatchSeqKey, "2")
	}()

	_, missed := b.SubscribeAndReplaySince(1)
	if missed == nil {
		t.Fatal("this instance holds everything the counter ends up reporting; resyncing here " +
			"would punish a client that missed nothing")
	}
	if len(missed) != 1 || missed[0].ID != 2 {
		t.Errorf("resume from 1: got %+v, want just id 2", missed)
	}
}

// TestResumeFallsBackToLocalKnowledgeWhenTheCounterIsUnreadable — a Redis
// hiccup must not turn every reconnect into a resync. Failing closed here would
// be a worse failure than the one the check guards against, because it fires on
// every client at once.
func TestResumeFallsBackToLocalKnowledgeWhenTheCounterIsUnreadable(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("precondition: only %d of 2 notifications arrived", i)
		}
	}
	b.Unsubscribe(ch)

	// The counter becomes unreadable — a WRONGTYPE error rather than a
	// connection failure, so the rest of the bus keeps working and only the
	// validating read fails.
	mr.Del(redisWatchSeqKey)
	if _, err := mr.Lpush(redisWatchSeqKey, "not-an-integer"); err != nil {
		t.Fatalf("seed a wrong-typed key: %v", err)
	}

	_, missed := b.SubscribeAndReplaySince(1)
	if missed == nil {
		t.Fatal("an unreadable counter must fall back to local knowledge, not resync every reconnect")
	}
	if len(missed) != 1 || missed[0].ID != 2 {
		t.Errorf("resume from 1: got %+v, want just id 2", missed)
	}
}

// TestResumeReportsAGapWhenTheCounterKeyDisappears — codex round 12, P2.
//
// The sequence key can vanish after this bus has seen ids (FLUSHDB, eviction).
// Reading redis.Nil as "unreadable" meant falling back to local knowledge and
// replaying an id space the authority no longer has, while the next publish
// starts again at 1. Absent is a VALUE — zero — and an instance holding ids
// disagrees with it.
func TestResumeReportsAGapWhenTheCounterKeyDisappears(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("precondition: only %d of 2 notifications arrived", i)
		}
	}
	b.Unsubscribe(ch)

	mr.Del(redisWatchSeqKey)

	_, missed := b.SubscribeAndReplaySince(1)
	if missed != nil {
		t.Fatalf("the counter is gone while this instance holds ids 1-2; replaying a dead id space "+
			"is exactly what the next publish (starting again at 1) will collide with; got %+v", missed)
	}
}

// TestResumeOnAVirginDeploymentDoesNotReportAGap is the control for the case
// above: an absent counter on a bus that has seen nothing is genuine agreement
// at zero, not a gap. Without this leg, "absent means gap" would pass the test
// above while resyncing every first connection on a fresh deployment.
func TestResumeOnAVirginDeploymentDoesNotReportAGap(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)

	if b.resumeOutrunsLocalView(5) {
		t.Error("nothing has ever been published and this instance has seen nothing; " +
			"there is no gap to report")
	}
}

// TestResumeIsUnaffectedForAFreshSubscriber — sinceID 0 is not a resume, so it
// must not pay the settle window or be handed a gap.
func TestResumeIsUnaffectedForAFreshSubscriber(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	waitFor(t, "the notification to be buffered", func() bool { return len(b.EventsSince(0)) == 1 })

	// Counter deliberately ahead, which would trip the check for a resume.
	if err := mr.Set(redisWatchSeqKey, "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	start := time.Now()
	_, missed := b.SubscribeAndReplaySince(0)
	if elapsed := time.Since(start); elapsed >= settleWindow {
		t.Errorf("a fresh subscriber waited %v — it should not pay the settle window at all", elapsed)
	}
	if missed == nil {
		t.Fatal("sinceID=0 is not a resume and must not be answered with a gap")
	}
}
