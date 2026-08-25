package watchevents

import "testing"

// raised reports whether a gap signal is pending, without blocking. Reading
// it CONSUMES the signal, which is what a real consumer's select does.
func raised(gaps <-chan struct{}) bool {
	select {
	case <-gaps:
		return true
	default:
		return false
	}
}

// TestGapSignalIsPerInstanceForAReceivedHole pins the SCOPE of the per-instance
// announcement, which is the part of BUG-2730 that was a design decision
// rather than a mechanism.
//
// A hole in what THIS instance received from Redis is shared by every
// subscriber here — none of them got the missing ids — so every one of them is
// told. The counterfactual that makes this test mean something is the second
// half: a subscriber that registers AFTER the detection is NOT told, because
// it was never promised the ids that went missing. Without that leg the test
// would pass for an implementation that simply signals everyone forever.
func TestGapSignalIsPerInstanceForAReceivedHole(t *testing.T) {
	b := newLocalOnlyBus(16)
	defer b.Close()

	chA, gapsA := b.Subscribe()
	chB, gapsB := b.Subscribe()
	defer b.Unsubscribe(chA)
	defer b.Unsubscribe(chB)

	b.fanOutLocally(Notification{ID: 1, Kind: "test"})
	if raised(gapsA) || raised(gapsB) {
		t.Fatal("a contiguous notification raised a gap signal; the flag means a HOLE, not delivery")
	}

	// id 5 after id 1: ids 2..4 never arrived here.
	b.fanOutLocally(Notification{ID: 5, Kind: "test"})
	if !raised(gapsA) {
		t.Error("subscriber A was connected across the hole and was not told")
	}
	if !raised(gapsB) {
		t.Error("subscriber B was connected across the hole and was not told")
	}

	// A subscriber that arrives after the hole missed nothing it was
	// promised: its cursor starts here.
	chC, gapsC := b.Subscribe()
	defer b.Unsubscribe(chC)
	b.fanOutLocally(Notification{ID: 6, Kind: "test"})
	if raised(gapsC) {
		t.Error("a subscriber that registered after the hole was told it had missed something")
	}
	if raised(gapsA) {
		t.Error("the hole was re-announced to A on a contiguous notification")
	}
}

// TestGapSignalForASlowSubscriberIsNotBroadcast is the other scope, and its
// control leg is the whole point: a full channel is a fact about ONE
// connection, so signalling every subscriber would charge a resync to clients
// whose stream is intact.
func TestGapSignalForASlowSubscriberIsNotBroadcast(t *testing.T) {
	b := newLocalOnlyBus(256)
	defer b.Close()

	slow, slowGaps := b.Subscribe()
	fast, fastGaps := b.Subscribe()
	defer b.Unsubscribe(slow)
	defer b.Unsubscribe(fast)

	// Fill the slow subscriber's 64-deep channel exactly, draining the fast
	// one as we go so only one of them is behind. IDs stay contiguous so the
	// per-instance arm cannot fire and take credit for the signal.
	const chanDepth = 64
	for i := 1; i <= chanDepth; i++ {
		b.fanOutLocally(Notification{ID: int64(i), Kind: "test"})
		<-fast
	}
	if raised(slowGaps) {
		t.Fatal("a full-but-not-yet-overflowing channel raised the flag; it must mean a DROP")
	}

	// One more: the slow subscriber's channel has no room.
	b.fanOutLocally(Notification{ID: chanDepth + 1, Kind: "test"})
	<-fast

	if !raised(slowGaps) {
		t.Error("the subscriber whose notification was dropped was not told")
	}
	if raised(fastGaps) {
		t.Error("a subscriber that received everything was told it had missed something")
	}
}

// TestGapSignalCoalesces is the load bound stated on the field, asserted.
// Without capacity-1 coalescing, a subscriber that stops reading entirely
// would accumulate one signal per dropped notification, and the consumer we
// are signalling is by definition one that could not keep up.
func TestGapSignalCoalesces(t *testing.T) {
	b := newLocalOnlyBus(256)
	defer b.Close()

	ch, gaps := b.Subscribe()
	defer b.Unsubscribe(ch)

	for i := 1; i <= 200; i++ {
		b.fanOutLocally(Notification{ID: int64(i), Kind: "test"})
	}

	if !raised(gaps) {
		t.Fatal("no gap signal after 200 notifications into a 64-deep channel")
	}
	if raised(gaps) {
		t.Error("more than one signal was queued; the channel must coalesce, not accumulate")
	}
}

// TestMemoryBusSignalsTheDroppedSubscriberOnly is the same claim for the
// single-process implementation, which has no received-hole concept at all —
// it is the source of its own ids — so a slow subscriber is its only way to
// under-deliver.
func TestMemoryBusSignalsTheDroppedSubscriberOnly(t *testing.T) {
	b := New()
	defer b.Close()

	slow, slowGaps := b.Subscribe()
	fast, fastGaps := b.Subscribe()
	defer b.Unsubscribe(slow)
	defer b.Unsubscribe(fast)

	for i := 0; i < 65; i++ {
		if err := b.Publish(Notification{Kind: "test"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		<-fast
	}

	if !raised(slowGaps) {
		t.Error("the subscriber whose notification was dropped was not told")
	}
	if raised(fastGaps) {
		t.Error("a subscriber that received everything was told it had missed something")
	}
}

// TestEpochChangeSignalsEverySubscriber and its sibling below cover the other
// two per-instance arms. The sequence-gap arm was tested first because it is
// the one the bug was filed against; these two reach the same signal by
// different reasoning, and a fix applied to one arm and not the others would
// otherwise ship looking complete.
func TestEpochChangeSignalsEverySubscriber(t *testing.T) {
	b := newLocalOnlyBus(16)
	defer b.Close()

	chA, gapsA := b.Subscribe()
	chB, gapsB := b.Subscribe()
	defer b.Unsubscribe(chA)
	defer b.Unsubscribe(chB)

	b.fanOutFromRedis("epoch-1", Notification{ID: 1, Kind: "test"}, b.currentGen())
	<-chA
	<-chB
	if raised(gapsA) || raised(gapsB) {
		t.Fatal("adopting the first epoch raised the flag; there was nothing to miss")
	}

	// A different epoch: the id space this instance was tracking is gone, so
	// every buffered id is meaningless and every subscriber has a hole.
	b.fanOutFromRedis("epoch-2", Notification{ID: 1, Kind: "test"}, b.currentGen())

	if !raised(gapsA) || !raised(gapsB) {
		t.Error("an epoch change invalidates the whole buffer, so every subscriber must be told")
	}
}

// TestCounterBackwardsSignalsEverySubscriber is the arm for a shared counter
// that restarted inside one epoch — evicted under maxmemory, lost to a
// FLUSHDB, restored from an older snapshot.
func TestCounterBackwardsSignalsEverySubscriber(t *testing.T) {
	b := newLocalOnlyBus(16)
	defer b.Close()

	chA, gapsA := b.Subscribe()
	chB, gapsB := b.Subscribe()
	defer b.Unsubscribe(chA)
	defer b.Unsubscribe(chB)

	b.fanOutLocally(Notification{ID: 100, Kind: "test"})
	<-chA
	<-chB
	if raised(gapsA) || raised(gapsB) {
		t.Fatal("a first notification raised the flag")
	}

	b.fanOutLocally(Notification{ID: 3, Kind: "test"})

	if !raised(gapsA) || !raised(gapsB) {
		t.Error("a counter reset drops the whole buffer, so every subscriber must be told")
	}
}
