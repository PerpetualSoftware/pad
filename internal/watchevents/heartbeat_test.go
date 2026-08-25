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

// TestAnInstanceThatRecoversBeforeItsCycleIsNotCycled is codex round 1's P2,
// and the same defect BUG-2738 had to fix at its round 11.
//
// cycleIfIdle decides under one lock and tears down under another. Between
// them a heartbeat or a notification can arrive, at which point the
// subscription is demonstrably alive — and dropping it resyncs every client on
// the instance for nothing. False positives are the property this design cares
// about most.
func TestAnInstanceThatRecoversBeforeItsCycleIsNotCycled(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)
	before := b.currentPubSub()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)

	// The recovery lands between the decision and the teardown. beforeDropHook
	// runs in exactly that window.
	var fired atomic.Bool
	b.beforeDropHook = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		b.mu.Lock()
		b.lastSeen = clock.now()
		b.mu.Unlock()
	}

	b.cycleIfIdle()

	if !fired.Load() {
		t.Fatal("the window was never reached; this test could not have discriminated")
	}
	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 0 {
		t.Fatalf("an instance that started receiving again before its cycle was cycled anyway (%d resets)", got)
	}
	if b.currentPubSub() != before {
		t.Fatal("the connection was replaced although it had started working again")
	}
}

// TestAStragglerFromAReplacedSubscriptionIsIgnored is codex round 1's other P2.
//
// Cancelling a receive loop and closing its PubSub does NOT join the goroutine,
// and go-redis's channel is buffered — so a frame from a subscription that has
// already been replaced can still reach the switch. Without a generation it
// would stamp the REPLACEMENT's liveness, append to the replacement's buffer,
// or drop the replacement's coverage, all on the strength of a frame that
// arrived on a dead socket. On a wedged route that is the worst direction: the
// dead connection's buffered tail would suppress the detector for its
// successor.
func TestAStragglerFromAReplacedSubscriptionIsIgnored(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	b.mu.Lock()
	staleGen := b.subGen
	b.mu.Unlock()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	b.mu.Lock()
	freshGen := b.subGen
	stampBefore := b.lastSeen
	b.mu.Unlock()
	if freshGen == staleGen {
		t.Fatal("fixture: nothing was replaced, so there is no straggler to simulate")
	}

	if b.isCurrentGen(staleGen) {
		t.Fatal("a frame from the replaced subscription is being treated as current")
	}

	// A straggler arriving now must change nothing.
	clock.advance(time.Hour)
	if b.isCurrentGen(staleGen) {
		t.Fatal("the stale generation is still accepted")
	}
	b.mu.Lock()
	stampAfter := b.lastSeen
	b.mu.Unlock()
	if !stampAfter.Equal(stampBefore) {
		t.Fatalf("the replacement's liveness moved from %v to %v without a frame of its own", stampBefore, stampAfter)
	}
}

// TestTheReceiveLoopActuallyConsultsTheGeneration is the WIRING for the
// straggler fence (CONVE-19). Its sibling above asserts isCurrentGen answers
// correctly; neither that nor the direct stamp checks say whether the loop ever
// calls it, and removing the call from the loop survived both.
//
// Driven by moving the generation on under a LIVE subscription: the running
// loop then holds a stale one, so a real notification arriving on a working
// socket must be ignored exactly as a straggler would be.
func TestTheReceiveLoopActuallyConsultsTheGeneration(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// THE CLOCK MUST MOVE FIRST or this test cannot discriminate: on a frozen
	// clock a stamp writes the value that is already there, so removing the
	// fence looks identical to keeping it. The mutation matrix said so before
	// this line existed.
	clock.advance(time.Minute)

	b.mu.Lock()
	b.subGen++ // the live loop is now a generation behind
	before := b.lastSeen
	b.mu.Unlock()

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Give the frame time to be delivered and dropped.
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	after := b.lastSeen
	b.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("a frame from a superseded generation stamped liveness (%v → %v): the loop is not consulting the "+
			"generation, so a dead connection's buffered tail can suppress the detector for its successor",
			before, after)
	}
}

// TestAProbeDoesNotCreditTheSubscriptionThatReplacedIt is the wiring for the
// other half of the fence. publishHeartbeats captures the generation, publishes
// off the lock — for as long as go-redis's timeouts allow — and stamps
// afterwards; if the subscription was replaced in that window, crediting the
// replacement claims a probe reached a socket that never saw one.
func TestAProbeDoesNotCreditTheSubscriptionThatReplacedIt(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	var moved atomic.Bool
	b.afterProbePublish = func() {
		if !moved.CompareAndSwap(false, true) {
			return
		}
		b.mu.Lock()
		b.subGen++ // a cycle replaced the subscription mid-publish
		b.mu.Unlock()
		clock.advance(time.Minute)
	}

	b.mu.Lock()
	before := b.lastProbeOK
	b.mu.Unlock()

	b.publishHeartbeats()

	if !moved.Load() {
		t.Fatal("the window was never reached; this test could not have discriminated")
	}
	b.mu.Lock()
	after := b.lastProbeOK
	b.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("a probe sent to the previous subscription credited its replacement (%v → %v)", before, after)
	}
}

// TestAStragglerCannotEnterTheReplacementsBuffer is the fan-out half of the
// generation fence. The straggler test above covers the liveness stamp; nothing
// covered the buffer, and removing fanOutFromRedis's check survived.
//
// A notification from a replaced subscription entering the new buffer is worse
// than a wasted stamp: it makes the instance vouch for a span it never received
// on a socket it has stopped believing, which is precisely the false coverage
// claim this whole family exists to remove.
func TestAStragglerCannotEnterTheReplacementsBuffer(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	staleGen := b.currentGen()
	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	if b.currentGen() == staleGen {
		t.Fatal("fixture: nothing was replaced, so there is no straggler to simulate")
	}
	if got := len(b.EventsSince(0)); got != 0 {
		t.Fatalf("fixture: the buffer should be empty after a cycle, has %d", got)
	}

	// The old loop's buffered tail arrives late.
	b.fanOutFromRedis("epoch-a", Notification{ID: 99, Kind: KindComment, ItemRef: "TASK-stale"}, staleGen)

	if got := b.EventsSince(0); len(got) != 0 {
		t.Fatalf("a notification from the replaced subscription entered the replacement's buffer (%d entries): this "+
			"instance would then vouch for a span it never received", len(got))
	}
}

// TestEachMutationRefusesAStragglerOnItsOwn is the per-mutation half of the
// fence, and the mutation matrix is why it exists as four separate assertions
// rather than one.
//
// The fence is not "check once and pass the frame along" — that was the shape
// codex round 3 blocked, because a check in one lock acquisition and a write in
// another is a TOCTOU. Each of the four things a frame can mutate re-checks
// under the lock in which it writes, and each is therefore reachable on its own:
// driving only fanOutFromRedis leaves dropCoverageForGen's check unexercised,
// and its outer early-return masks fanOutLocally's.
func TestEachMutationRefusesAStragglerOnItsOwn(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	staleGen := b.currentGen()
	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()
	if b.currentGen() == staleGen {
		t.Fatal("fixture: nothing was replaced")
	}
	resetsAfterCycle := obs.snapshot().resets[ResetReasonIdleTimeout]

	t.Run("the append refuses it", func(t *testing.T) {
		// fanOutLocally directly, because fanOutFromRedis's own guard would
		// return before reaching it — that guard protects the EPOCH fields it
		// writes, and this one protects the buffer.
		b.fanOutLocally(Notification{ID: 77, Kind: KindComment, ItemRef: "TASK-stale"}, staleGen)
		if got := b.EventsSince(0); len(got) != 0 {
			t.Fatalf("a straggler entered the replacement's buffer (%d entries)", len(got))
		}
	})

	t.Run("the coverage drop refuses it", func(t *testing.T) {
		b.dropCoverageForGen(ResetReasonUndecodableMessage, staleGen)
		got := obs.snapshot()
		if got.resets[ResetReasonUndecodableMessage] != 0 {
			t.Fatalf("a straggler ended the replacement's coverage: undecodable_message resets = %d",
				got.resets[ResetReasonUndecodableMessage])
		}
		if got.resets[ResetReasonIdleTimeout] != resetsAfterCycle {
			t.Fatal("the straggler moved the idle_timeout count")
		}
	})

	t.Run("the epoch bookkeeping refuses it", func(t *testing.T) {
		// fanOutFromRedis writes b.epoch and, on a mismatch, drops the WHOLE
		// buffer and reports epoch_change. A straggler carrying the old id
		// space would therefore resync every client on the instance — a
		// distinct mutation from the append below it, with its own guard.
		b.mu.Lock()
		epochBefore := b.epoch
		b.mu.Unlock()

		b.fanOutFromRedis("some-other-epoch", Notification{ID: 5, Kind: KindComment, ItemRef: "TASK-x"}, staleGen)

		b.mu.Lock()
		epochAfter := b.epoch
		b.mu.Unlock()
		if epochAfter != epochBefore {
			t.Fatalf("a straggler rewrote the id space (%q → %q)", epochBefore, epochAfter)
		}
		if got := obs.snapshot().resets[ResetReasonEpochChange]; got != 0 {
			t.Fatalf("a straggler reported an epoch change (%d): every client on the instance would resync", got)
		}
	})

	t.Run("the liveness stamp refuses it", func(t *testing.T) {
		b.mu.Lock()
		before := b.lastSeen
		b.mu.Unlock()
		clock.advance(time.Minute)
		b.stampLastSeen(staleGen)
		b.mu.Lock()
		after := b.lastSeen
		b.mu.Unlock()
		if !after.Equal(before) {
			t.Fatalf("a straggler refreshed the replacement's liveness (%v → %v)", before, after)
		}
	})
}
