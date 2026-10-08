package watchevents

import (
	"testing"
	"time"
)

// BUG-2741: Close cancels the receive loop's context AND closes its pubsub,
// so both select cases can be ready at once and Go picks one at random. When
// it picks the message, the receive path used to run to completion on a
// closed bus: rebuilding the replay buffer, rewriting the coverage fields, and
// REPORTING sequence resets and gaps for a bus with no subscribers left.
//
// The race itself is random, so each leg drives the receive-path function
// directly on a bus Close has finished with: the state a frame that won the
// select meets. Each leg first shows the SAME call reports on an open bus, so
// "nothing reported" means the closed check refused it and not that the input
// was inert. One leg per locked site, because each site needs its own check:
// fanOutFromRedis and fanOutLocally lock separately, and dropCoverage is also
// reached from outside the receive loop.

func closedReceiveBus(t *testing.T) (*RedisBus, *recordingObserver) {
	t.Helper()
	b := newLocalOnlyBus(16)
	obs := newRecordingObserver()
	b.SetObserver(obs)
	return b, obs
}

func TestBUG2741_FanOutFromRedisAfterCloseRecordsNothing(t *testing.T) {
	// PREMISE: on an open bus, a new epoch is a reported reset.
	open, openObs := closedReceiveBus(t)
	open.fanOutFromRedis("1", Notification{ID: 1, Kind: "test"}, open.currentGen())
	open.fanOutFromRedis("2", Notification{ID: 1, Kind: "test"}, open.currentGen())
	if got := openObs.snapshot(); got.resets[ResetReasonEpochChange] != 1 {
		t.Fatalf("premise: an epoch change on an open bus reported %+v, want one epoch_change reset", got)
	}
	open.Close()

	b, obs := closedReceiveBus(t)
	b.fanOutFromRedis("1", Notification{ID: 1, Kind: "test"}, b.currentGen())
	b.Close()
	before := obs.snapshot()

	b.fanOutFromRedis("2", Notification{ID: 1, Kind: "test"}, b.currentGen())

	if got := obs.snapshot(); got.totalEvents != before.totalEvents {
		t.Fatalf("a frame after Close was reported: before %+v, after %+v", before, got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epoch != "1" || b.lastAppendedID != 1 {
		t.Fatalf("a frame after Close rewrote the bus: epoch %q lastAppendedID %d, want 1 / 1", b.epoch, b.lastAppendedID)
	}
}

func TestBUG2741_FanOutLocallyAfterCloseRecordsNothing(t *testing.T) {
	// PREMISE: on an open bus, id 5 after id 1 is a reported gap.
	open, openObs := closedReceiveBus(t)
	open.fanOutLocally(Notification{ID: 1, Kind: "test"}, open.currentGen())
	open.fanOutLocally(Notification{ID: 5, Kind: "test"}, open.currentGen())
	if got := openObs.snapshot(); got.gaps != 1 {
		t.Fatalf("premise: a hole on an open bus reported %+v, want one gap", got)
	}
	open.Close()

	b, obs := closedReceiveBus(t)
	b.fanOutLocally(Notification{ID: 1, Kind: "test"}, b.currentGen())
	b.Close()
	before := obs.snapshot()

	b.fanOutLocally(Notification{ID: 5, Kind: "test"}, b.currentGen())

	if got := obs.snapshot(); got.totalEvents != before.totalEvents {
		t.Fatalf("a frame after Close was reported: before %+v, after %+v", before, got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastAppendedID != 1 {
		t.Fatalf("a frame after Close was appended: lastAppendedID %d, want 1", b.lastAppendedID)
	}
}

func TestBUG2741_DropCoverageAfterCloseRecordsNothing(t *testing.T) {
	// PREMISE: on an open bus, dropping coverage is a reported reset.
	open, openObs := closedReceiveBus(t)
	open.fanOutLocally(Notification{ID: 1, Kind: "test"}, open.currentGen())
	open.dropCoverage(ResetReasonSubscriptionResumed)
	if got := openObs.snapshot(); got.resets[ResetReasonSubscriptionResumed] != 1 {
		t.Fatalf("premise: a coverage drop on an open bus reported %+v, want one subscription_resumed reset", got)
	}
	open.Close()

	b, obs := closedReceiveBus(t)
	b.fanOutLocally(Notification{ID: 1, Kind: "test"}, b.currentGen())
	b.Close()
	before := obs.snapshot()

	b.dropCoverage(ResetReasonSubscriptionResumed)

	if got := obs.snapshot(); got.totalEvents != before.totalEvents {
		t.Fatalf("a coverage drop after Close was reported: before %+v, after %+v", before, got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastAppendedID != 1 {
		t.Fatalf("a coverage drop after Close rewrote the bus: lastAppendedID %d, want 1", b.lastAppendedID)
	}
}

func TestBUG2741_StampLastSeenAfterCloseRecordsNothing(t *testing.T) {
	// PREMISE: on an open bus, a received frame stamps the liveness clock.
	stamp := time.Unix(1_000, 0)
	open, _ := closedReceiveBus(t)
	open.nowFunc = func() time.Time { return stamp }
	open.stampLastSeen(open.currentGen())
	if !open.lastSeen.Equal(stamp) {
		t.Fatalf("premise: an open bus did not stamp lastSeen (got %v)", open.lastSeen)
	}
	open.Close()

	b, _ := closedReceiveBus(t)
	b.nowFunc = func() time.Time { return stamp }
	b.Close()

	b.stampLastSeen(b.currentGen())

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.lastSeen.IsZero() {
		t.Fatalf("a frame after Close stamped lastSeen = %v; a closed bus has no subscription to vouch for", b.lastSeen)
	}
}
