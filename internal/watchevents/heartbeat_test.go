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

// TestAPayloadWearingThePrefixEndsCoverageThroughTheRealPath is the wiring half
// of the test above (CONVE-19), and it exists because that one only calls
// isWatchHeartbeat. The predicate can be perfect while the receive loop routes
// every "hb|…" payload to the ignore arm without asking it — a mutant that
// checks the PREFIX rather than the shape survives every assertion up there,
// and the name of the test above promises coverage ends, which is a claim about
// the loop.
func TestAPayloadWearingThePrefixEndsCoverageThroughTheRealPath(t *testing.T) {
	b, mr, _, obs := newHeartbeatBus(t, false)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	raw := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = raw.Close() }()
	channel := b.keys.Name(redisWatchChannelSuffix)

	// Wears the prefix, fails the shape.
	if err := raw.Publish(context.Background(), channel, "hb|not-a-version").Err(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the malformed frame to end coverage", func() bool {
		return obs.snapshot().resets[ResetReasonUndecodableMessage] == 1
	})

	// CONTROL: the well-formed frame on the same path, same connection, must
	// NOT move it. Without this leg the assertion above also passes on a loop
	// that treats every payload as undecodable, which is the opposite defect
	// and equally wrong.
	if err := raw.Publish(context.Background(), channel, watchHeartbeatPayload).Err(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Ordering barrier: same channel, so seeing the notification proves the
	// heartbeat ahead of it has already been through the loop.
	waitFor(t, "the notification published after the heartbeat", func() bool { return len(b.EventsSince(0)) == 1 })
	if got := obs.snapshot().resets[ResetReasonUndecodableMessage]; got != 1 {
		t.Fatalf("a well-formed heartbeat ended coverage too (%d): the loop is matching the prefix, not the shape", got)
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

	// WAITED FOR, NOT SAMPLED (codex round 9). The count is incremented inside
	// the goroutine, so reading it straight after the constructor returns can
	// catch zero — and then "the count returned to where it started" is
	// satisfied by a loop that never ran, which is the assertion below going
	// vacuous. The sibling concurrency test had the same defect and was fixed
	// a commit earlier; this one was missed because only the named test was
	// looked at. LATENT, not observed: sampling instead of waiting survives 10
	// runs here, because the goroutine does get going in time in practice.
	// This removes the possibility rather than a failure anyone has seen.
	waitFor(t, "the constructor's receive loop to be running", func() bool { return liveReceiveLoops.Load() == 1 })
	before := liveReceiveLoops.Load()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	// IT LEFT, and this leg is why the test is not just an absence (codex round
	// 4). "No exit was reported" is also what a replaced goroutine that never
	// exits at all produces — a leak, and a worse outcome than the noisy exit
	// this test exists to rule out. The count returns to where it started: one
	// loop torn down, one replacement installed.
	waitFor(t, "the replaced receive loop to leave", func() bool { return liveReceiveLoops.Load() == before })

	// ...AND IT LEFT BY THE QUIET DOOR. Checked after the join above, so this
	// is a statement about a goroutine that has finished rather than one that
	// has not got there yet.
	if got := obs.snapshot().loopExits; got != 0 {
		t.Fatalf("a deliberately replaced receive loop reported itself as a failure %d time(s): an operator would "+
			"read that counter as this instance having gone deaf", got)
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

// TestTheMaintenanceLoopStartsThePublisher is the wiring (CONVE-19) for ONE of
// the loop's two halves: every test above calls publishHeartbeats directly, and
// none would notice if the constructor stopped starting the loop.
//
// ONE HALF, AND THE NAME SAYS SO. An earlier version claimed both and observed
// only a heartbeat, which a loop that started just the publisher satisfies —
// and a shared kick channel between the two halves, the defect this file's
// first draft actually had, is precisely that shape (codex round 4).
//
// The idle half is not provable here. Its scanner needs an instance that has
// received nothing, and against a live miniredis this bus's own heartbeats come
// straight back and refresh liveness every cadence — an attempt to wedge it
// with the loop running is a race against the publisher, which is what the
// first fix for this finding turned out to be. TestAWedgedWatchRouteIsDetected-
// EndToEnd is the honest proof: it darkens the route with the blackhole proxy,
// never calls cycleIfIdle, and waits for the scanner to reach it. Verified
// against both mutations — starting only the publisher, and pointing the
// scanner at the publisher's kick channel — each of which that test detects and
// this one cannot.
func TestTheMaintenanceLoopStartsThePublisher(t *testing.T) {
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

	// Buffered by one so the loop never blocks on a test that has stopped
	// reading, and drained exactly once per published frame below.
	handled := make(chan struct{}, 1)
	b.afterFrameHandled = func() {
		select {
		case handled <- struct{}{}:
		default:
		}
	}
	// BOUNDED, because the failure this barrier exists to catch is a loop that
	// never handles the frame — and a bare receive turns that into a package
	// timeout with no message attached to the test that found it.
	awaitFrame := func(what string) {
		t.Helper()
		select {
		case <-handled:
		case <-time.After(3 * time.Second):
			t.Fatalf("the receive loop never handled %s: it is stalled or was never started, so nothing this test "+
				"asserts about the generation means anything", what)
		}
	}

	b.mu.Lock()
	b.subGen++ // the live loop is now a generation behind
	before := b.lastSeen
	b.mu.Unlock()

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-refused"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A REAL BARRIER, NOT A SLEEP (codex round 4). The original waited 300ms
	// and asserted nothing had changed — which a loop that stalled, or never
	// started, satisfies perfectly. There is no natural signal to wait on
	// instead, because a frame the fence refuses is by design invisible: that
	// is the whole property. Hence the afterFrameHandled seam, which fires
	// after the loop has handled a frame whichever arm it took.
	awaitFrame("the refused notification")
	if got := len(b.EventsSince(0)); got != 0 {
		t.Fatalf("a frame from a superseded generation entered the replacement's buffer (%d entries)", got)
	}

	b.mu.Lock()
	after := b.lastSeen
	b.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("a frame from a superseded generation stamped liveness (%v → %v): the loop is not consulting the "+
			"generation, so a dead connection's buffered tail can suppress the detector for its successor",
			before, after)
	}

	// CONTROL. Everything above is an absence, and an absence is what a dead
	// loop produces too — the seam proves a frame was handled, this proves the
	// same loop still ACCEPTS one when the generation matches, so the refusal
	// was the fence and not paralysis.
	b.mu.Lock()
	b.subGen--
	b.mu.Unlock()
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-accepted"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	awaitFrame("the accepted notification")
	got := b.EventsSince(0)
	if len(got) != 1 || got[0].ItemRef != "TASK-accepted" {
		t.Fatalf("buffer holds %v, want exactly the accepted notification", got)
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

// TestClosingTheBusDuringAnIdleCycleIsSafe drives the one interleave the cycle
// path created and nothing else exercises: Close running while a cycle is
// between tearing the old subscription down and installing its replacement.
//
// Both hazards there are mine, and both came from making b.pubsub REASSIGNABLE.
// Before BUG-2769 it was written once in the constructor, so Close could read
// it without the lock and resubscribe did not exist. Now:
//
//   - Close's read of b.pubsub races the cycle's write of it, and the value
//     that races is the one whose Close unblocks the loop Close then waits for.
//   - resubscribe's wg.Add can land after Close has set closed and reached
//     Wait. A WaitGroup forbids an Add that takes the counter off zero from
//     racing a Wait, and the cycle has just torn the only other loop down, so
//     zero is exactly where the counter is.
//
// WHAT THIS TEST DOES AND DOES NOT PROVE, because the mutation matrix was
// clear about it. It proves Close survives a cycle in flight and returns with
// no receive loop running — both real assertions, and the second fails against
// a Close that stops waiting properly. It does NOT prove either ordering fix
// above is load-bearing: reverting each in turn leaves this green, and that is
// not a gap in the test but a fact about the code. resubscribe checks b.closed
// and takes its count under ONE lock acquisition, and Close cannot do anything
// destructive without that same lock, so the window each fix guards is already
// shut by the b.closed check. Both changes are kept as defence — the invariant
// they rely on is three functions away and an easy one to lose — and this
// comment exists so nobody later reads them as fixes for an observed race.
// Run with -race and -count>1; the interleave is worth exercising even so.
func TestClosingTheBusDuringAnIdleCycleIsSafe(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// HELD OPEN AT THE POINT THE WINDOW ACTUALLY IS. A first version started
	// Close from beforeDropHook and slept, which never overlapped anything:
	// the cycle had not reached resubscribe yet, so Close simply won the race,
	// resubscribe found b.closed and returned its error, and the two mutations
	// below both survived. The seam fires inside resubscribe with the new
	// subscription installed and counted and the lock released — the state
	// Close's read of b.pubsub and its Wait have to be correct against.
	closed := make(chan struct{})
	b.afterResubscribeInstall = func() {
		go func() {
			defer close(closed)
			b.Close()
		}()
		// Close blocks on the mutex only until this function's caller released
		// it, which it already has, so this is long enough for Close to be
		// inside its own body — reading b.pubsub, or in Wait.
		time.Sleep(20 * time.Millisecond)
	}

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned: a cycle in flight deadlocked it, or a receive loop it is waiting for never left")
	}

	// AND IT WAITED FOR WHAT IT PROMISED. This is the assertion with teeth:
	// Close's contract is that no receive goroutine is running when it
	// returns, and an Add that lands after Close has reached Wait breaks it
	// silently — Wait returns on a counter that was zero at the wrong moment
	// and the loop outlives the bus. Returning at all is not evidence of that;
	// the count is.
	// CHECKED THE INSTANT Close RETURNS, not eventually. Eventually is what a
	// leaked goroutine also satisfies once its context is cancelled, so a
	// waitFor here passes against the very defect this asserts. Sound because
	// receiveMessages drops this count before it calls wg.Done, so Wait
	// returning means the count has already reached zero.
	if got := liveReceiveLoops.Load(); got != 0 {
		t.Fatalf("Close returned with %d receive loop(s) still running: it waited on a counter that was zero at the "+
			"wrong moment, so the loop outlives the bus", got)
	}
}

// TestAFailedResubscribeDoesNotReDropCoverageEveryPass is codex round 5's P2,
// and it is a full-outage behaviour rather than a logic slip.
//
// The probe-failure suspension does not cover this. Suspension asks "did our
// last probe get through", and the answer can be YES with the route already
// gone: the last successful publish stamps lastProbeOK, Redis dies before that
// frame comes back, and lastSeen stays behind it. From there the timestamps are
// frozen — the probe fails, so nothing stamps lastProbeOK, and nothing arrives,
// so nothing stamps lastSeen — and the cycle's own precondition stays true for
// the whole outage. Every pass then drops coverage, announces to every
// subscriber, and re-dials.
//
// Only the RE-DIAL should repeat. Dropping coverage a second time drops a
// buffer that is already empty and re-announces a hole every subscriber has
// already been told about, and it moves the reset counter once per cadence for
// as long as Redis is away — turning one outage into an unbounded incident
// count on the very series an operator is asked to alert on.
func TestAFailedResubscribeDoesNotReDropCoverageEveryPass(t *testing.T) {
	b, mr, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// Redis goes away entirely: the resubscribe inside the cycle cannot
	// succeed, which is the state this test is about.
	mr.Close()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()
	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("fixture: the first pass did not cycle (%d resets), so a second pass proves nothing", got)
	}

	// Two more passes, exactly as the maintenance loop would run them.
	clock.advance(DefaultWatchIdleTimeout + time.Second)
	b.cycleIfIdle()
	clock.advance(DefaultWatchIdleTimeout + time.Second)
	b.cycleIfIdle()

	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("coverage was dropped %d times across three passes with Redis away: each pass after the first "+
			"re-drops an already-empty buffer and re-announces a hole every subscriber has been told about, and "+
			"an operator sees the outage as that many separate incidents", got)
	}
}

// TestRecoveryAfterAFailedResubscribeStillReplacesTheConnection is the other
// half, and the reason the fix above cannot simply suspend the cycle. Retrying
// the re-dial IS the recovery path — the comment in cycleIfIdle says so — so a
// fix that stops the pass from running at all would trade a noisy outage for
// one the instance never comes back from.
func TestRecoveryAfterAFailedResubscribeStillReplacesTheConnection(t *testing.T) {
	b, mr, clock, obs := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	addr := mr.Addr()
	mr.Close()

	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()
	if b.currentPubSub() != nil {
		t.Fatal("a failed resubscribe left a subscription behind; Close would close it a second time")
	}

	// Redis comes back at the same address.
	revived, err := miniredis.Run()
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	t.Cleanup(revived.Close)
	if err := revived.StartAddr(addr); err != nil {
		// Already listening from Run(); StartAddr on a running instance is the
		// error path, so fall back to pointing the client at the new address.
		b.mu.Lock()
		b.client = redis.NewClient(&redis.Options{Addr: revived.Addr()})
		b.mu.Unlock()
	}

	clock.advance(DefaultWatchIdleTimeout + time.Second)
	b.cycleIfIdle()

	if b.currentPubSub() == nil {
		t.Fatal("the instance never re-established its subscription after Redis came back: the retry that makes " +
			"this self-healing is the same pass the noise fix touches")
	}
	if got := obs.snapshot().resets[ResetReasonIdleTimeout]; got != 1 {
		t.Fatalf("recovery moved the reset counter to %d; the coverage drop belongs to the outage, not to coming back", got)
	}
}

// TestTwoConcurrentRetriesInstallOneSubscription is codex round 6's P2.
//
// Both the cycle and its retry arm dial with the lock RELEASED — deliberately,
// since a Redis round trip under the bus's hot mutex would stall every fan-out
// on the instance — so two passes can each find no subscription and each dial
// one. Installing both is wrong twice over: two receive loops would run on the
// SAME generation, so both accept every frame and each notification is
// processed twice, and the loser's PubSub would be untracked — nothing closes
// it, Close included.
//
// Only ONE goroutine calls this in production, so this is a guard on an
// invariant rather than a fix for an observed fault. It is written down because
// the invariant lives in a different file from the code that depends on it, and
// because the failure it prevents is silent duplication rather than a crash.
func TestTwoConcurrentRetriesInstallOneSubscription(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// Get the bus into the no-subscription state the retry arm exists for, by
	// the only route that produces it: a cycle whose resubscribe failed.
	//
	// WAITED FOR, NOT SAMPLED. The count is incremented inside the goroutine,
	// so reading it straight after the constructor returns catches zero — as
	// the first version of this test did, and then measured every later count
	// against that wrong baseline.
	waitFor(t, "the constructor's receive loop to be running", func() bool { return liveReceiveLoops.Load() == 1 })

	b.mu.Lock()
	good := b.client
	b.client = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	b.mu.Unlock()
	wedge(t, b, clock, DefaultWatchIdleTimeout+time.Second)
	b.cycleIfIdle()
	if b.currentPubSub() != nil {
		t.Fatal("fixture: the teardown left a subscription behind, so there is nothing to race for")
	}
	waitFor(t, "the torn-down loop to leave", func() bool { return liveReceiveLoops.Load() == 0 })

	b.mu.Lock()
	b.client = good
	b.mu.Unlock()

	// WHAT THIS DETECTS, MEASURED. Removing the guard entirely: 10 runs, 10
	// failures. A guard that checks b.pubsub in its OWN acquisition and then
	// takes the lock again to install — the regression codex round 8 asked
	// about — is NOT detected: 10 runs, 0 failures. The barrier below opens the
	// window, but landing the second caller inside the mutant's own
	// check-to-install gap needs a seam that exists only in the mutant, and
	// after the barrier releases, the winner crosses that gap with nothing to
	// yield on. Said here rather than left implied: this test covers "a guard
	// exists", not "the guard is in the right critical section". The latter is
	// held by the comment at the guard and by review.
	//
	// COUNTED, NOT SYNCHRONISED ON. The seam fires only for a caller that
	// actually installs, so a WaitGroup expecting both arrivals can never
	// complete in the passing case — the waiter on it is a goroutine leaked by
	// design, which an earlier version of this test had. An atomic the
	// abandoning caller simply never touches says the same thing and blocks
	// nobody.
	var installs atomic.Int32
	b.afterResubscribeInstall = func() { installs.Add(1) }

	// BOTH HELD AT THE DIAL/INSTALL BOUNDARY, which is the only place the
	// window is actually open. Starting two goroutines together makes overlap
	// likely and guarantees nothing — one can finish before the other begins,
	// and a guard that checked b.pubsub OUTSIDE the install lock would pass
	// (codex round 8). Each caller announces its arrival and waits for the
	// other, so both are past the dial and neither has taken the lock.
	atBoundary := make(chan struct{}, 2)
	bothDialled := make(chan struct{})
	b.beforeInstallLock = func() {
		atBoundary <- struct{}{}
		select {
		case <-bothDialled:
		case <-time.After(3 * time.Second):
			// Bounded so a caller that never gets a partner fails the test
			// below rather than wedging the package.
		}
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.resubscribe() }()
	}
	for range 2 {
		select {
		case <-atBoundary:
		case <-time.After(5 * time.Second):
			close(bothDialled)
			wg.Wait()
			t.Fatal("both callers never reached the dial/install boundary together, so the window this test exists " +
				"for was never open and nothing below is evidence")
		}
	}
	close(bothDialled)
	wg.Wait()

	// THE DISCRIMINATING ASSERTION, and it is on the install count rather than
	// on the loop count. A loop is started after its install, so reading
	// liveReceiveLoops can catch a second caller's loop before it has begun and
	// see the passing value on a failing run. The install count cannot: both
	// callers have returned by here, so it is final.
	if got := installs.Load(); got != 1 {
		t.Fatalf("%d callers installed a subscription, want exactly 1: two receive loops on one generation deliver "+
			"every notification twice, and the loser's PubSub is tracked by nothing, Close included", got)
	}
	if b.currentPubSub() == nil {
		t.Fatal("neither caller installed anything: the guard rejected both and the instance is now deaf")
	}
	waitFor(t, "the installed receive loop to be running", func() bool { return liveReceiveLoops.Load() == 1 })
}

// TestTheFrameSeamFiresForEveryArm pins what the seam's comment claims, after
// the comment was found claiming more than the code did (codex round 9). The
// arms that decline to act were `continue` statements in the receive loop, so
// the seam after the switch never ran for a heartbeat, an undecodable payload,
// or a subscription confirmation — and a test waiting on it for one of those
// would have hung rather than failed, which is the worst way to learn this.
func TestTheFrameSeamFiresForEveryArm(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, false)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	var handled atomic.Int32
	b.afterFrameHandled = func() { handled.Add(1) }

	raw := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = raw.Close() }()
	channel := b.keys.Name(redisWatchChannelSuffix)

	// One frame per arm the switch has, in the order they appear in it. The
	// subscription-confirmation arm is not driveable from here — go-redis emits
	// it on its own reconnect — so this covers the three a publisher can cause.
	for _, payload := range []string{
		watchHeartbeatPayload, // the recognise-and-ignore arm
		"not a notification",  // the undecodable arm
	} {
		if err := raw.Publish(context.Background(), channel, payload).Err(); err != nil {
			t.Fatalf("publish %q: %v", payload, err)
		}
	}
	// The acting arm last, so its arrival is an ordering barrier for the two
	// above: same channel, so Redis delivers in order.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the notification to arrive", func() bool { return len(b.EventsSince(0)) == 1 })
	waitFor(t, "all three frames to be reported handled", func() bool { return handled.Load() == 3 })

	if got := handled.Load(); got != 3 {
		t.Fatalf("the seam fired %d times for 3 frames: an arm that declines to act skips it, so a test waiting on "+
			"it for such a frame hangs instead of failing", got)
	}
}
