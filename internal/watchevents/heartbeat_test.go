package watchevents

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// testClock is a hand-driven clock. A real one cannot test this: the threshold
// is 90 seconds BY CONSTRUCTION — three publish intervals, not a tuned number —
// so a real-time test would either sleep through one or be written against a
// threshold production does not use.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHeartbeatBus(t *testing.T, publishHeartbeat bool) (*RedisBus, *miniredis.Miniredis, *testClock, *recordingObserver) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	clock := &testClock{t: time.Now()}
	b := NewRedisBusWithKeys(client, 64, redisns.Default, publishHeartbeat)
	b.nowFunc = clock.now
	// The constructor stamped with the real clock before nowFunc was set; put
	// both stamps on the test clock so the arithmetic below is the shipped
	// arithmetic rather than a mix of two clocks.
	b.mu.Lock()
	b.lastSeen, b.lastProbeOK = clock.now(), clock.now()
	b.mu.Unlock()

	obs := newRecordingObserver()
	b.SetObserver(obs)
	t.Cleanup(b.Close)
	return b, mr, clock, obs
}

// wedge simulates the failure this unit exists to detect: the outbound probe
// keeps SUCCEEDING and nothing comes back.
//
// Simulated rather than produced, because miniredis is a working Redis — a
// heartbeat published against it is delivered straight back to our own
// subscription and the instance never looks idle. Advancing the clock past the
// threshold (nothing arrived) and stamping lastProbeOK as a successful publish
// pass would IS a half-open route: writes accepted, reads dead.
func wedge(t *testing.T, b *RedisBus, clock *testClock, d time.Duration) {
	t.Helper()
	clock.advance(d)
	b.mu.Lock()
	b.lastProbeOK = clock.now()
	b.mu.Unlock()
}

func (b *RedisBus) currentPubSub() *redis.PubSub {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pubsub
}

// TestAHeartbeatDoesNotEndCoverage is the phase-1 contract, and the half that
// makes the two-phase roll zero-loss.
//
// Fails without the classification: "hb|1" reaches decodePayload, fails, and —
// since BUG-2739 — is treated as a hole in coverage. Every un-upgraded instance
// would drop its buffer and resync all its clients, every interval, for the
// length of a mixed deployment.
func TestAHeartbeatDoesNotEndCoverage(t *testing.T) {
	b, mr, _, obs := newHeartbeatBus(t, false)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the first notification", func() bool { return len(b.EventsSince(0)) == 1 })

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	if err := client.Publish(context.Background(), b.keys.Name(redisWatchChannelSuffix), watchHeartbeatPayload).Err(); err != nil {
		t.Fatalf("publish heartbeat: %v", err)
	}

	// ORDERING BARRIER, not a sleep: both go to the same channel, so Redis
	// delivers them to this subscription in order. Seeing the second
	// notification proves the heartbeat has already been through the loop.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the notification published after the heartbeat", func() bool { return len(b.EventsSince(0)) == 2 })

	if got := obs.snapshot().resets; len(got) != 0 {
		t.Fatalf("a heartbeat ended this instance's coverage: resets %v", got)
	}
}

// TestAPayloadWearingThePrefixStillEndsCoverage is the other half of the frame
// contract. The prefix buys forward compatibility, but it must not become a
// hole in the loud path: before this feature every payload this instance could
// not read ended coverage and moved undecodable_message, the counter whose
// documented job is "suspect a namespace collision".
func TestAPayloadWearingThePrefixStillEndsCoverage(t *testing.T) {
	for _, payload := range []string{
		"hb|",
		"hb|x",
		"hb|1|<script>",
		`hb|{"kind":"comment"}`,
	} {
		if isWatchHeartbeat(payload) {
			t.Errorf("isWatchHeartbeat(%q) = true: a payload this instance did not publish is being silently ignored "+
				"instead of ending coverage", payload)
		}
	}
	for _, payload := range []string{watchHeartbeatPayload, "hb|2", "hb|2|instance-a|1699999999"} {
		if !isWatchHeartbeat(payload) {
			t.Errorf("isWatchHeartbeat(%q) = false: a disciplined future frame must not need a third roll", payload)
		}
	}
}

// TestAnIdleSubscriptionEndsCoverageAndIsReplaced is the remedy. The generation
// check is what separates "coverage dropped" from "connection replaced";
// without it a drop-only implementation passes.
func TestAnIdleSubscriptionEndsCoverageAndIsReplaced(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("idle_timeout resets = %d, want 1", got)
	}
	if after := b.currentPubSub(); after == before {
		t.Fatal("coverage was dropped from behind the same dead socket: that is a resync loop, not a recovery")
	}

	// And the replacement delivers.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-9"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the replacement subscription to deliver", func() bool { return len(b.EventsSince(0)) >= 1 })
}

// TestAReplacedLoopExitsQuietly pins the distinction the receive loop's own
// close-arm exists to make. That arm logs an ERROR and moves
// pad_watchevents_receive_loop_exits_total, documented as "non-zero outside
// shutdown means this instance receives no notifications at all". A cycle
// closes a subscription deliberately, and must not trip it.
func TestAReplacedLoopExitsQuietly(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	// Give the replaced loop time to notice its closed channel and take
	// whichever exit it is going to take.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if obs.snapshot().loopExits > 0 {
			t.Fatal("a deliberately replaced receive loop reported itself as a failure: an operator would read that " +
				"counter as this instance having gone deaf")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAFreshSubscriptionIsNotCycled is the install-stamp rule. A zero lastSeen
// reads as 1970 and would cycle the replacement on the next pass — forever,
// since each replacement is equally fresh.
func TestAFreshSubscriptionIsNotCycled(t *testing.T) {
	b, _, _, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 0 {
		t.Fatalf("a subscription installed this instant was cycled (%d resets)", got)
	}
	if b.currentPubSub() != before {
		t.Fatal("the subscription was replaced despite having just been installed")
	}
}

// TestAQuietInstanceIsNotCycledOnPhase1 is the regression test for the defect
// BUG-2738's first draft shipped: with no heartbeat and no traffic, a perfectly
// healthy subscription crosses the threshold and is cycled — a resync storm on
// the DEFAULT configuration.
func TestAQuietInstanceIsNotCycledOnPhase1(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, false)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	for range 10 {
		wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
		b.cycleIfIdle()
	}

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 0 {
		t.Fatalf("a healthy, quiet instance was cycled %d times on phase 1", got)
	}
	if b.currentPubSub() != before {
		t.Fatal("the connection was replaced although nothing was wrong with it")
	}
}

// TestPhase2CyclesTheSameInstance is the counterfactual. Without it, "no
// cycles" is satisfied by a detector that never works.
func TestPhase2CyclesTheSameInstance(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("idle_timeout resets = %d on phase 2, want 1: the phase gate has disabled the detector outright", got)
	}
}

// TestAFailedProbeSuspendsDetection is the mirror image of the failure this
// feature finds. PUBLISH travels on the client's ordinary pool while the
// subscription holds one from the separate pub/sub pool, so a publish-side
// failure says nothing about whether this subscription can receive — and
// reading it as evidence tears down healthy connections on a schedule.
func TestAFailedProbeSuspendsDetection(t *testing.T) {
	b, mr, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	mr.Close()
	clock.advance(DefaultWatchIdleTimeout + time.Second)
	b.publishHeartbeats()
	if obs.snapshot().probeFailures == 0 {
		t.Fatal("the probe did not fail; this test could not have discriminated")
	}
	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 0 {
		t.Fatalf("a subscription was cycled (%d resets) on the strength of silence we caused ourselves", got)
	}
	if b.currentPubSub() != before {
		t.Fatal("the connection was replaced although we never managed to probe it")
	}
}

// TestTheMaintenanceLoopRunsBothHalves is the wiring (CONVE-19): every test
// above calls the halves directly, and none would notice if the constructor
// stopped starting the loop.
func TestTheMaintenanceLoopRunsBothHalves(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, true)

	raw := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = raw.Close() })
	ps := raw.Subscribe(context.Background(), b.keys.Name(redisWatchChannelSuffix))
	t.Cleanup(func() { _ = ps.Close() })
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatalf("raw subscribe: %v", err)
	}

	var sawFrame atomic.Bool
	go func() {
		for msg := range ps.Channel() {
			if isWatchHeartbeat(msg.Payload) {
				sawFrame.Store(true)
				return
			}
		}
	}()

	b.setMaintenanceCadence(5*time.Millisecond, DefaultWatchIdleTimeout)

	deadline := time.Now().Add(5 * time.Second)
	for !sawFrame.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the maintenance loop published nothing: publishHeartbeats is correct but nothing calls it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheCadenceDoesNotDriftWithPassDuration pins the scheduling arithmetic.
// Breaking it is self-defeating rather than untidy: an instance whose passes are
// slow emits heartbeats further apart, sees them further apart, and crosses its
// own 3T threshold — the slowness manufacturing the incident.
func TestTheCadenceDoesNotDriftWithPassDuration(t *testing.T) {
	const interval = 30 * time.Second
	base := time.Unix(1_700_000_000, 0)

	if got := nextWatchTick(base, interval, base.Add(25*time.Second)); !got.Equal(base.Add(interval)) {
		t.Errorf("a slow pass pushed the next tick to %v, want %v", got, base.Add(interval))
	}

	prev := base
	for i := 1; i <= 10; i++ {
		prev = nextWatchTick(prev, interval, prev.Add(20*time.Second))
		if want := base.Add(time.Duration(i) * interval); !prev.Equal(want) {
			t.Fatalf("tick %d landed at %v, want %v: drift is accumulating", i, prev, want)
		}
	}

	now := base.Add(5 * interval)
	if got := nextWatchTick(base, interval, now); !got.Equal(now.Add(interval)) {
		t.Errorf("an overrun replayed missed ticks instead of resetting: got %v, want %v", got, now.Add(interval))
	}
}

// TestOrdinaryTrafficKeepsTheInstanceAlive pins that inbound frames stamp
// liveness. Without it the detector would cycle a connection that is plainly
// working, on a schedule, for as long as the deployment kept using it.
//
// Found by mutation: removing the per-frame stamp survived every other test
// here, because they all drive idleness through the clock and never let real
// traffic arrive.
func TestOrdinaryTrafficKeepsTheInstanceAlive(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	// Three thresholds of steady traffic, with the clock advancing past the
	// threshold between each — the workspace is only ever idle if the arriving
	// notifications do not count as liveness.
	for i := range 3 {
		clock.advance(DefaultWatchIdleTimeout - time.Second)
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		want := i + 1
		waitFor(t, "the notification to arrive back", func() bool { return len(b.EventsSince(0)) >= want })
		// The probe succeeded more recently than nothing arrived, which is the
		// state a healthy phase-2 instance is in.
		b.mu.Lock()
		b.lastProbeOK = clock.now()
		b.mu.Unlock()
		clock.advance(2 * time.Second)
		b.cycleIfIdle()
	}

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 0 {
		t.Fatalf("an instance receiving notifications throughout was cycled %d times: inbound frames are not stamping liveness", got)
	}
	if b.currentPubSub() != before {
		t.Fatal("the connection was replaced while traffic was arriving on it")
	}
}

// TestAReplacementCanItselfBeCycled is the install-stamp rule stated as the
// consequence that actually matters. A replacement that inherits the OLD
// stamps has lastProbeOK equal to lastSeen, so the ordering premise never holds
// again and the detector goes permanently quiet after its first cycle — a
// detector that works exactly once, which is worse than one that never runs
// because it looks like it is working.
//
// Found by mutation: dropping the install stamps survived, because no test
// asked for a SECOND detection.
func TestAReplacementCanItselfBeCycled(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()
	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("first cycle: idle_timeout resets = %d, want 1", got)
	}
	first := b.currentPubSub()

	// THE REPLACEMENT CARRIES CURRENT STAMPS, asserted directly because the
	// behavioural route below cannot see it: wedge() writes lastProbeOK itself,
	// so a replacement that inherited stale stamps would still be cycled and
	// the mutation survived. What the install stamp actually guarantees is this
	// state, so this is where it is checked.
	b.mu.Lock()
	seen, probed := b.lastSeen, b.lastProbeOK
	b.mu.Unlock()
	if seen.Before(clock.now()) || probed.Before(clock.now()) {
		t.Fatalf("the replacement inherited stale stamps (lastSeen=%v lastProbeOK=%v, now=%v): a subscription that has "+
			"not been given a chance to receive anything is already older than the threshold",
			seen, probed, clock.now())
	}

	// The replacement wedges too, which is the ordinary case when the route
	// itself is bad rather than the socket.
	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 2 {
		t.Fatalf("second cycle: idle_timeout resets = %d, want 2 — the detector stopped after one cycle, which it does "+
			"if the replacement inherits stamps instead of being stamped at install", got)
	}
	if b.currentPubSub() == first {
		t.Fatal("the replacement was never itself replaced")
	}
}
