package events

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// testClock is a hand-driven clock for idle detection.
//
// A REAL CLOCK CANNOT TEST THIS. The threshold is 90 seconds by construction —
// it is three publish intervals, not a tuned number — so every test here would
// either sleep through one or be rewritten against a threshold production does
// not use. Driving the clock tests the SHIPPED constants.
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

// newHeartbeatBus builds a phase-1 bus (recognises heartbeats, publishes none)
// on a hand-driven clock, with an observer attached.
func newHeartbeatBus(t *testing.T, publishHeartbeat bool) (*RedisBus, *miniredis.Miniredis, *testClock, *recordingObserver) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, redisns.Default, false, publishHeartbeat)
	clock := &testClock{t: time.Now()}
	// Set before anything subscribes, so the INSTALL stamp is on this clock
	// too — the seam is read only from now(), and the maintenance loop's first
	// read is a full interval away.
	b.nowFunc = clock.now
	obs := &recordingObserver{}
	b.SetObserver(obs)
	t.Cleanup(b.Close)
	return b, mr, clock, obs
}

// resetHookObserver runs a callback on SequenceReset and ignores everything
// else. It is how a test reaches the one window inside cycleOne that has no
// seam of its own — see TestAnIdleCycleAbandonsAWorkspaceEmptiedUnderIt.
type resetHookObserver struct {
	onReset func(reason string)
}

func (o resetHookObserver) ResumeGap(string)            {}
func (o resetHookObserver) EventDropped(string)         {}
func (o resetHookObserver) ReceiveLoopExited()          {}
func (o resetHookObserver) SubscriptionUnconfirmed()    {}
func (o resetHookObserver) SubscriptionCycled()         {}
func (o resetHookObserver) SequenceReset(reason string) { o.onReset(reason) }

func (b *RedisBus) channelFor(workspaceID string) string {
	return b.keys.Name(redisChannelSuffix) + workspaceID
}

// rawSubscriber returns a channel of raw payload strings on one workspace's
// Redis channel, bypassing the bus entirely — the only way to assert what this
// instance PUBLISHES rather than what it does with what it receives.
func rawSubscriber(t *testing.T, addr, channel string) <-chan string {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	ps := client.Subscribe(context.Background(), channel)
	t.Cleanup(func() { _ = ps.Close() })
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatalf("raw subscribe to %s: %v", channel, err)
	}
	out := make(chan string, 32)
	go func() {
		for msg := range ps.Channel() {
			out <- msg.Payload
		}
	}()
	return out
}

// waitFor polls until cond holds, so a test asserts an outcome rather than a
// sleep. Fails the test rather than hanging.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// THE JOINT (BUG-2738). The idle detector is a THIRD actor in a region whose
// invariants were all designed around request goroutines plus Close. This is
// the test the plan named before the code was written, mirroring BUG-2749's
// joiner test against a different third actor.
// ---------------------------------------------------------------------------

// TestAJoinerIsServedAcrossAnIdleFiredCycle is the joint test.
//
// A subscriber arrives for a workspace at the exact moment the idle detector is
// replacing that workspace's connection. It must end up served by a LIVE
// subscription — never stranded on an establishment promise nobody keeps, and
// never handed a channel behind a subscription the cycle has already torn down.
//
// The two failure shapes it discriminates between, both of which look like a
// working test run from the outside:
//
//   - If the cycle tore down wsSubs WITHOUT first minting the establishment
//     record, the joiner would find no live subscription and no pending one,
//     appoint itself establisher, and open a SECOND Redis subscription for the
//     workspace — two connections, two receive loops, every event twice.
//   - If the cycle minted the record and then abandoned it without retiring it,
//     the joiner would wait on a promise nobody keeps, return with a channel
//     wired to nothing, and — because its own registration keeps wsCounts
//     non-zero — no later caller would establish either.
func TestAJoinerIsServedAcrossAnIdleFiredCycle(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	first, _, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("first subscriber: outcome %v", outcome)
	}
	defer b.Unsubscribe(first)

	genBefore, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("fixture: no subscription was installed for ws-1")
	}

	// Arm the seam only for the CYCLE's establishment: the first subscriber's
	// is already finished.
	var joinerCh chan Event
	var joinerOutcome SubscribeOutcome
	joinerDone := make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	var once sync.Once
	b.beforeInstallSubscription = func(workspaceID string) {
		if !armed.Load() {
			return
		}
		once.Do(func() {
			go func() {
				defer close(joinerDone)
				joinerCh, _, joinerOutcome = b.SubscribeIfAllowed(context.Background(), workspaceID, 0)
			}()
			// The joiner must be REGISTERED and waiting before the cycle's
			// establishment proceeds; otherwise this test would be exercising
			// an ordinary sequential subscribe and would pass against every
			// defect above.
			waitFor(t, "the joiner to register against the in-flight cycle", func() bool {
				b.mu.Lock()
				defer b.mu.Unlock()
				_, pending := b.pendingSubs[workspaceID]
				return b.wsCounts[workspaceID] >= 2 && pending
			})
		})
	}

	clock.advance(2 * DefaultIdleTimeout)
	b.cycleIdleSubscriptions()
	armed.Store(false)

	// BOUNDED, and the bound is part of the instrument. An implementation that
	// drops coverage without re-establishing never reaches the seam at all, so
	// the joiner is never spawned and this channel never closes — an unbounded
	// receive would turn that mutation into a hung test run rather than a
	// named failure, which is not a detection anyone can act on.
	select {
	case <-joinerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the cycle never established a replacement, so no joiner ever ran: this is drop-only, not drop-and-cycle")
	}
	if joinerOutcome != SubscribeOK {
		t.Fatalf("the joiner was refused across the cycle: outcome %v", joinerOutcome)
	}
	if joinerCh == nil {
		t.Fatal("the joiner returned no channel")
	}
	defer b.Unsubscribe(joinerCh)

	genAfter, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("no subscription is installed for ws-1 after the cycle: the joiner is holding a channel wired to nothing")
	}
	if genAfter == genBefore {
		t.Fatal("the subscription was never replaced; this test did not exercise a cycle")
	}

	// The only assertion that proves the joiner's channel is behind a LIVE
	// subscription rather than a torn-down one.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	for name, ch := range map[string]chan Event{"joiner": joinerCh, "first subscriber": first} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s received nothing after the cycle: its channel is behind a subscription that no longer exists", name)
		}
	}
}

// TestAResumingJoinerIsToldSyncRequiredAcrossACycle answers codex round 2's P2
// with the case that discriminates.
//
// The concern raised was that a subscriber arriving DURING a cycle gets no gap
// signal — dropWorkspaceCoverage signals only the subscribers present when it
// runs. That is true, and for a RESUMING caller it is not the mechanism that
// protects it: the registration MARK is. It registers while the workspace has
// no buffer at all, so its mark cannot match the buffer that exists by the
// time it reads, and eventsSinceMarkLocked answers nil — the strongest form of
// "this instance cannot vouch". Its caller then reports a resume gap and the
// SSE layer answers sync_required.
//
// MUTATION-CONFIRMED, and the first two attempts at this test were not.
// Replacing eventsSinceMarkLocked with the unmarked eventsSinceLocked fails
// here with the joiner handed the post-cycle event as though it followed its
// cursor — which is the defect this asserts against. Note that deleting the
// `mark.buffer == nil` term ALONE survives: inside that function the keep
// arithmetic already reduces to zero for a nil mark, so that term is redundant
// with its neighbour rather than load-bearing. The mark being CONSULTED AT ALL
// is what matters.
//
// A FRESH caller (sinceID == 0) is deliberately NOT signalled, and that is not
// an oversight: it holds no prior position, so there is no span it could be
// missing. It is also admitted only after the replacement subscription is
// acknowledged — it waits on the cycle's establishment record, which
// finishPending closes after the confirmation — and on the unconfirmed path it
// is told to reconcile when the acknowledgement eventually lands. Signalling
// it anyway would be a resync demanded of a client with nothing to reconcile,
// which is the load inversion this unit has already had to fix once.
func TestAResumingJoinerIsToldSyncRequiredAcrossACycle(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	first, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(first)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	var seen Event
	select {
	case seen = <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: no event, so there is no cursor to resume from")
	}

	var joinerMissed []Event
	var joinerOutcome SubscribeOutcome
	var joinerCh chan Event
	joinerDone := make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	armed.Store(true)
	b.beforeInstallSubscription = func(workspaceID string) {
		if !armed.Load() {
			return
		}
		once.Do(func() {
			go func() {
				defer close(joinerDone)
				joinerCh, joinerMissed, _, joinerOutcome =
					b.SubscribeAndReplaySince(context.Background(), workspaceID, seen.ID, 0)
			}()
			waitFor(t, "the resuming joiner to register mid-cycle", func() bool {
				b.mu.Lock()
				defer b.mu.Unlock()
				return b.wsCounts[workspaceID] >= 2
			})
		})
	}

	// A BUFFER MUST EXIST AGAIN BEFORE THE JOINER READS ITS REPLAY, or this
	// test proves nothing. Mutation-checked: without this, the cycle leaves no
	// buffer at all, eventsSinceMarkLocked returns nil from its FIRST term
	// (`!ok`), and removing the mark check entirely still passes — the test
	// would be asserting the empty case rather than the one it is named for.
	// Publishing here puts a FRESH buffer in place, so the only thing that can
	// still answer nil is the mark not matching it.
	b.afterSubscriptionConfirmed = func() {
		if !armed.Load() {
			return
		}
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		waitFor(t, "the post-cycle event to rebuild the workspace's buffer", func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.replayBuffers["ws-1"] != nil
		})
	}

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()
	armed.Store(false)

	select {
	case <-joinerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the resuming joiner never returned")
	}

	b.mu.Lock()
	rebuilt := b.replayBuffers["ws-1"] != nil
	b.mu.Unlock()
	if !rebuilt {
		t.Fatal("fixture: no buffer was rebuilt before the joiner read its replay, so this test could not have discriminated")
	}
	if joinerOutcome != SubscribeOK {
		t.Fatalf("the resuming joiner was refused: %v", joinerOutcome)
	}
	defer b.Unsubscribe(joinerCh)

	if joinerMissed != nil {
		t.Fatalf("a caller resuming from %d across a cycle was answered with %d replayed events instead of sync_required: it was told it was caught up across a span this instance cannot vouch for",
			seen.ID, len(joinerMissed))
	}
}

// ---------------------------------------------------------------------------
// Phase 1: recognise and ignore.
// ---------------------------------------------------------------------------

// TestAHeartbeatDoesNotEndAWorkspacesCoverage is the phase-1 contract, and it
// is the half that makes the two-phase roll zero-loss.
//
// Fails without the classification in decodePayload: "hb|1" has one separator,
// so it falls through to the bare-JSON branch, fails to unmarshal, and — since
// BUG-2739 — is treated as a hole in coverage. The buffer would be dropped and
// every live subscriber told to resync, every interval, for the length of a
// mixed deployment.
func TestAHeartbeatDoesNotEndAWorkspacesCoverage(t *testing.T) {
	b, mr, _, obs := newHeartbeatBus(t, false)

	ch, _, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("subscribe: %v", outcome)
	}
	defer b.Unsubscribe(ch)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	var first Event
	select {
	case first = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: the first event never arrived, so there is no buffer to protect")
	}

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	if err := client.Publish(context.Background(), b.channelFor("ws-1"), heartbeatPayload).Err(); err != nil {
		t.Fatalf("publish heartbeat: %v", err)
	}

	// ORDERING BARRIER, not a sleep: both are published on the same channel, so
	// Redis delivers them to this subscription in order. Receiving the second
	// event proves the heartbeat has already been through receiveMessages.
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the event published after the heartbeat never arrived")
	}

	if reasons := obs.resetReasons(); len(reasons) != 0 {
		t.Fatalf("a heartbeat ended this workspace's coverage: resets %v", reasons)
	}
	if missed := b.EventsSince("ws-1", first.ID); len(missed) != 1 {
		t.Fatalf("the replay buffer no longer covers the span across the heartbeat: EventsSince returned %d events, want 1", len(missed))
	}
}

// TestAHeartbeatReachesNoSubscriber pins the other half of "bus-internal": a
// heartbeat must never be delivered as an event, which would put a zero-valued
// Event with no type and no id in front of a client.
func TestAHeartbeatReachesNoSubscriber(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, false)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	if err := client.Publish(context.Background(), b.channelFor("ws-1"), heartbeatPayload).Err(); err != nil {
		t.Fatalf("publish heartbeat: %v", err)
	}
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})

	select {
	case got := <-ch:
		if got.Type != ItemCreated {
			t.Fatalf("a heartbeat was delivered to a subscriber as an event: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the real event never arrived")
	}
}

// TestAHeartbeatIsRecognisedByPrefixNotByExactPayload pins the forward
// compatibility the prefix buys: a LATER version of the frame, carrying fields
// this binary knows nothing about, must still be ignored rather than ending
// coverage. Without it, extending the frame would need a third two-phase roll.
func TestAHeartbeatIsRecognisedByPrefixNotByExactPayload(t *testing.T) {
	for _, payload := range []string{
		heartbeatPayload,
		heartbeatPrefix + "2",
		heartbeatPrefix + "2|instance-a|1699999999",
		heartbeatPrefix,
	} {
		kind, _, _, err := decodePayload(payload)
		if err != nil {
			t.Fatalf("decodePayload(%q) errored: %v — an error here ends a workspace's coverage", payload, err)
		}
		if kind != payloadHeartbeat {
			t.Fatalf("decodePayload(%q) kind = %v, want payloadHeartbeat", payload, kind)
		}
	}
}

// TestAnEventIsNotMistakenForAHeartbeat is the counterfactual to the test
// above: the classification must not swallow real traffic. A bare JSON body
// and a prefixed one both still decode as events.
func TestAnEventIsNotMistakenForAHeartbeat(t *testing.T) {
	for _, payload := range []string{
		`{"type":"item.created","workspace_id":"ws-1","id":7}`,
		`3|9|{"type":"item.created","workspace_id":"ws-1"}`,
	} {
		kind, _, ev, err := decodePayload(payload)
		if err != nil {
			t.Fatalf("decodePayload(%q): %v", payload, err)
		}
		if kind != payloadEvent {
			t.Fatalf("decodePayload(%q) kind = %v, want payloadEvent", payload, kind)
		}
		if ev.WorkspaceID != "ws-1" {
			t.Fatalf("decodePayload(%q) lost the event: %+v", payload, ev)
		}
	}
}

// ---------------------------------------------------------------------------
// Idle detection and the drop-and-cycle remedy.
// ---------------------------------------------------------------------------

// TestAnIdleSubscriptionEndsItsCoverageAndReplacesItsConnection is the remedy.
//
// DROP ALONE IS NOT ENOUGH, and this test is what says so. A half-open route
// stays half-open: dropping coverage makes the next resume honest, but the
// resync it demands is served from the same dead subscription and the detector
// fires again on the next tick — a loop metering the failure at 3T intervals
// rather than a recovery. The generation assertion below is what distinguishes
// "coverage dropped" from "connection replaced"; without it a drop-only
// implementation passes.
func TestAnIdleSubscriptionEndsItsCoverageAndReplacesItsConnection(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	// TWO events, and the cursor is the FIRST one's id. A cursor of 0 is a
	// FRESH client, for which "nothing to replay" is the honest answer and
	// eventsSinceLocked returns an empty slice rather than nil — so a
	// one-event fixture cannot tell an ended coverage window from a fresh
	// connection, and would pass against a bus that never dropped anything.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	var first Event
	select {
	case first = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: no event arrived, so there is no coverage to end")
	}
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: the second event never arrived")
	}
	if got := b.EventsSince("ws-1", first.ID); len(got) != 1 {
		t.Fatalf("fixture: the workspace has no replay coverage to drop (EventsSince returned %d)", len(got))
	}

	genBefore, _ := b.liveGen("ws-1")

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()

	if got := obs.cycledCount(); got != 1 {
		t.Fatalf("SubscriptionCycled fired %d times, want 1", got)
	}
	if reasons := obs.resetReasons(); len(reasons) != 1 || reasons[0] != ResetReasonIdleTimeout {
		t.Fatalf("reset reasons = %v, want exactly [%s]", reasons, ResetReasonIdleTimeout)
	}
	if got := b.EventsSince("ws-1", first.ID); got != nil {
		t.Fatalf("coverage was not ended: a resume across the silence is still answered with %d events instead of sync_required", len(got))
	}

	genAfter, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("the subscription was dropped and never replaced: every later event for this workspace is lost")
	}
	if genAfter == genBefore {
		t.Fatalf("the connection was not replaced (generation still %d): coverage was dropped from behind the same dead socket, which is a resync loop, not a recovery", genAfter)
	}

	// THE OLD CONNECTION MUST ALSO BE GONE, and a generation check alone does
	// not say so: establishSubscription overwrites wsSubs, so an implementation
	// that installed a replacement WITHOUT tearing the old one down would pass
	// every assertion above while leaking a PubSub, its connection and its
	// receive goroutine on every cycle — forever, since nothing else will ever
	// tear them down. The wedged route is exactly the case where they never
	// die on their own.
	waitFor(t, "the cycled subscription's receive loop to exit", func() bool {
		return obs.loopExitCount() >= 1
	})

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the replacement subscription delivers nothing")
	}
}

// TestAFreshlySubscribedWorkspaceIsNotCycled is JOINT RULE 2.
//
// Fails without the install-time stamp: a zero lastSeen reads as 1970, so a
// subscription that has simply not been given the chance to receive anything is
// older than any threshold and is cycled on the detector's first pass —
// forever, since each replacement is equally fresh.
func TestAFreshlySubscribedWorkspaceIsNotCycled(t *testing.T) {
	b, _, _, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	genBefore, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("fixture: nothing was installed")
	}

	// No clock advance at all: this subscription is zero seconds old.
	b.cycleIdleSubscriptions()

	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("a subscription installed this instant was cycled as idle (%d times)", got)
	}
	if genAfter, _ := b.liveGen("ws-1"); genAfter != genBefore {
		t.Fatalf("the subscription was replaced (generation %d → %d) despite having just been installed", genBefore, genAfter)
	}
}

// TestAnUnconfirmedAdmissionIsNotCycledAsIdle is JOINT RULE 2 in the case that
// motivated it — and the one a fresh-subscription test alone does NOT cover.
//
// The plain fresh case is stamped twice over: at install, and again by the
// subscription acknowledgement arriving. Under BUG-2747's unconfirmed
// admission NO acknowledgement arrives, so the install stamp is the only thing
// standing between that subscriber and a detector that would cycle hardest on
// exactly the workspaces already having a bad time.
func TestAnUnconfirmedAdmissionIsNotCycledAsIdle(t *testing.T) {
	mr := miniredis.RunT(t)
	// Park the SUBSCRIBE in flight for longer than the bus is willing to wait,
	// so the caller is admitted with no acknowledgement behind it.
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 2*time.Second)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	// PHASE 2, because the detector only runs there — flipping the gate in
	// codex round 1 silently un-covered this test, which the mutation matrix
	// caught: removing the install-time stamp went from DETECTED back to
	// SURVIVED, because a phase-1 bus never scans and this was the only test
	// that could see the zero value.
	b := NewRedisBusWithKeys(client, redisns.Default, false, true)
	clock := &testClock{t: time.Now()}
	b.nowFunc = clock.now
	b.confirmTimeout = 50 * time.Millisecond
	obs := &recordingObserver{}
	b.SetObserver(obs)
	t.Cleanup(b.Close)

	ch, _, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("subscribe: %v", outcome)
	}
	defer b.Unsubscribe(ch)

	if proxy.held.Load() == 0 {
		t.Fatal("the SUBSCRIBE was never parked; this test could not have discriminated")
	}
	if obs.unconfirmedCount() == 0 {
		t.Fatal("the admission was confirmed after all; this test is not exercising the unconfirmed case")
	}

	b.cycleIdleSubscriptions()
	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("an unconfirmed admission was cycled as idle (%d times): the detector attacks the workspaces already in trouble", got)
	}
}

// TestAnyInboundFrameKeepsASubscriptionAlive pins that liveness is stamped for
// EVERY frame, ahead of the type switch — not only for frames that turn into
// events.
//
// An undecodable payload ends the workspace's COVERAGE, which is correct and
// unchanged. It says nothing about the CONNECTION, which demonstrably works:
// something arrived on it. Stamping inside the switch would miss this path (it
// continues), and the detector would then cycle a healthy socket on top of a
// coverage drop it had already reported.
func TestAnyInboundFrameKeepsASubscriptionAlive(t *testing.T) {
	b, mr, clock, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)
	genBefore, _ := b.liveGen("ws-1")

	// A real event first, BEFORE the clock moves, for two reasons: it stamps
	// liveness at time zero (so the advance below is measured from there), and
	// it creates the replay buffer without which dropWorkspaceCoverage reports
	// no reset at all — which is the barrier this test waits on.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: no event arrived, so the undecodable frame below reports nothing to wait on")
	}

	// Age the subscription to just short of the threshold, then deliver a frame
	// that CANNOT be decoded. If that frame stamps liveness the workspace is
	// young again; if it does not, the next advance takes it over the line.
	clock.advance(DefaultIdleTimeout - time.Second)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	if err := client.Publish(context.Background(), b.channelFor("ws-1"), "not an event and not a heartbeat").Err(); err != nil {
		t.Fatalf("publish garbage: %v", err)
	}
	waitFor(t, "the undecodable frame to be processed", func() bool {
		for _, r := range obs.resetReasons() {
			if r == ResetReasonUndecodableMessage {
				return true
			}
		}
		return false
	})

	clock.advance(2 * time.Second)
	b.cycleIdleSubscriptions()

	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("a subscription that had just received a frame was cycled as idle (%d times): only frames that decode are stamping liveness", got)
	}
	if genAfter, _ := b.liveGen("ws-1"); genAfter != genBefore {
		t.Fatalf("the connection was replaced (generation %d → %d) although traffic was arriving on it", genBefore, genAfter)
	}
}

// TestAHeartbeatKeepsAQuietWorkspaceOutOfTheDetector is the whole point of
// publishing them: on a workspace with no events at all, our own frame is what
// makes the silence diagnostic rather than ambiguous.
func TestAHeartbeatKeepsAQuietWorkspaceOutOfTheDetector(t *testing.T) {
	b, mr, clock, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)
	genBefore, _ := b.liveGen("ws-1")

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	// Three intervals of a completely silent workspace, kept alive by nothing
	// but heartbeats. Without the stamp on the heartbeat path this crosses the
	// threshold on the second pass.
	for range 6 {
		clock.advance(DefaultHeartbeatInterval)
		if err := client.Publish(context.Background(), b.channelFor("ws-1"), heartbeatPayload).Err(); err != nil {
			t.Fatalf("publish heartbeat: %v", err)
		}
		waitFor(t, "the heartbeat to be processed", func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			sub, ok := b.wsSubs["ws-1"]
			return ok && !sub.lastSeen.Before(clock.now())
		})
		b.cycleIdleSubscriptions()
	}

	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("a workspace kept alive by heartbeats alone was cycled %d times", got)
	}
	if genAfter, _ := b.liveGen("ws-1"); genAfter != genBefore {
		t.Fatalf("the connection was replaced (generation %d → %d) while heartbeats were arriving", genBefore, genAfter)
	}
}

// TestAStragglerCannotRefreshTheLivenessOfItsSuccessor pins the generation
// check on the liveness stamp.
//
// stopRedisSubscription only SIGNALS a receive loop; it never joins it. So a
// goroutine belonging to a subscription that has already been replaced can
// reach the stamp afterwards — and if it did, its dying frames would keep the
// REPLACEMENT looking alive. On a wedged route that is the worst possible
// direction for the error: the buffered tail of the dead connection would
// suppress the detector for the new one.
func TestAStragglerCannotRefreshTheLivenessOfItsSuccessor(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)
	staleGen, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("fixture: nothing installed")
	}

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()

	freshGen, ok := b.liveGen("ws-1")
	if !ok {
		t.Fatal("fixture: no replacement subscription")
	}
	if freshGen == staleGen {
		t.Fatal("fixture: nothing was replaced, so there is no straggler to simulate")
	}

	b.mu.Lock()
	before := b.wsSubs["ws-1"].lastSeen
	b.mu.Unlock()

	// The straggler arrives: a frame stamped against the generation that no
	// longer exists.
	clock.advance(time.Hour)
	b.stampLastSeen("ws-1", staleGen)

	b.mu.Lock()
	after := b.wsSubs["ws-1"].lastSeen
	b.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("a frame from generation %d refreshed generation %d's liveness (%v → %v): a dead connection's tail can suppress the detector for its replacement",
			staleGen, freshGen, before, after)
	}
}

// ---------------------------------------------------------------------------
// Joint rules 1 and 3.
// ---------------------------------------------------------------------------

// TestAnIdleCycleRefusesWhileAnEstablishmentIsInFlight is JOINT RULE 1.
//
// The window is real and narrow: establishSubscription installs wsSubs and only
// THEN waits for the acknowledgement, so between those two points a workspace
// has both a live subscription and an in-flight establishment record. A
// detector that cycled there would tear down the very subscription that
// establishment is about to confirm — and would mint a second establishment
// record for a workspace that already has one, breaching the single-establisher
// wall from a direction it was never guarded against.
//
// The seam runs after the install and before the wait, which is the only point
// at which this state is observable.
func TestAnIdleCycleRefusesWhileAnEstablishmentIsInFlight(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	var reached atomic.Bool
	var pendingSeen, liveSeen atomic.Bool
	b.afterInstallSubscription = func(workspaceID string) {
		if !reached.CompareAndSwap(false, true) {
			return
		}
		b.mu.Lock()
		_, pending := b.pendingSubs[workspaceID]
		_, live := b.wsSubs[workspaceID]
		b.mu.Unlock()
		pendingSeen.Store(pending)
		liveSeen.Store(live)

		// Make it look maximally idle at the one moment it is also mid-
		// establishment.
		clock.advance(10 * DefaultIdleTimeout)
		b.cycleIdleSubscriptions()
	}

	ch, _, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("subscribe: %v", outcome)
	}
	defer b.Unsubscribe(ch)

	if !reached.Load() {
		t.Fatal("the seam never ran; this test could not have discriminated")
	}
	// The test asserts its own premise: without BOTH of these true at the seam,
	// rule 1's window was never entered and a passing run means nothing.
	if !pendingSeen.Load() || !liveSeen.Load() {
		t.Fatalf("the overlap was not reached (pending=%v live=%v): rule 1's window is not being exercised",
			pendingSeen.Load(), liveSeen.Load())
	}

	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("the detector cycled a subscription with an establishment in flight (%d times)", got)
	}

	// And the establishment it would have torn down still works.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the subscription the cycle refused to touch delivers nothing")
	}
}

// TestAnIdleCycleAbandonsAWorkspaceEmptiedUnderIt is JOINT RULE 3, at the
// SECOND read — the one that matters.
//
// wsCounts is read once when the scan selects a victim and again under the lock
// that performs the teardown, and between them the last subscriber can leave.
// Its Unsubscribe stops the subscription itself, so a detector that trusted its
// first read would install a fresh Redis connection and receive loop for a
// workspace nobody is watching, with nothing left to ever tear it down —
// Unsubscribe only stops a subscription when it takes the count from one to
// zero, and the count is already zero.
//
// The observer callback is the instrument: dropWorkspaceCoverage reports its
// reset with b.mu RELEASED (the report defer is registered before the unlock
// defer, so it runs after it), which is exactly the gap between cycleOne's drop
// and its second read.
func TestAnIdleCycleAbandonsAWorkspaceEmptiedUnderIt(t *testing.T) {
	b, _, clock, _ := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: no event arrived, so the drop below reports nothing and the seam never fires")
	}

	var fired atomic.Bool
	b.SetObserver(resetHookObserver{onReset: func(reason string) {
		if reason != ResetReasonIdleTimeout || !fired.CompareAndSwap(false, true) {
			return
		}
		// The last subscriber leaves in the window between the coverage drop
		// and the teardown decision.
		b.Unsubscribe(ch)
	}})

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()

	if !fired.Load() {
		t.Fatal("the seam never fired; this test could not have discriminated")
	}
	b.mu.Lock()
	_, live := b.wsSubs["ws-1"]
	_, pending := b.pendingSubs["ws-1"]
	count := b.wsCounts["ws-1"]
	b.mu.Unlock()

	if count != 0 {
		t.Fatalf("fixture: wsCounts is %d, want 0 — the subscriber did not actually leave", count)
	}
	if live {
		t.Fatal("a Redis subscription and receive loop were installed for a workspace with no subscribers; nothing will ever tear them down")
	}
	if pending {
		t.Fatal("the establishment record was left behind: the next subscriber for this workspace waits on a promise nobody keeps")
	}
}

// ---------------------------------------------------------------------------
// Phase 2: publishing.
// ---------------------------------------------------------------------------

// TestPhase1PublishesNoHeartbeats is the gate. An instance that emits before
// every peer can classify the frame makes each un-upgraded instance drop its
// buffer and resync every one of its clients, every interval, for the length of
// the roll.
func TestPhase1PublishesNoHeartbeats(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, false)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)
	raw := rawSubscriber(t, mr.Addr(), b.channelFor("ws-1"))

	b.publishHeartbeats()

	// An ordering barrier again: a real publish behind the heartbeat that must
	// not exist. If a frame were emitted it would be ahead of this one.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case payload := <-raw:
		if isHeartbeat(payload) {
			t.Fatalf("a phase-1 instance published a heartbeat (%q): every un-upgraded peer resyncs all its clients", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was published at all; the barrier did not arrive")
	}
}

// TestPhase2PublishesOneHeartbeatPerSubscribedWorkspace pins both halves of the
// emission: it happens, and it is scoped to the workspaces this instance is
// actually subscribed to — a frame on a channel we do not read proves nothing
// about any connection of ours and is pure load.
func TestPhase2PublishesOneHeartbeatPerSubscribedWorkspace(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, true)

	one, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(one)
	two, _, _ := b.Subscribe(context.Background(), "ws-2")
	defer b.Unsubscribe(two)

	rawOne := rawSubscriber(t, mr.Addr(), b.channelFor("ws-1"))
	rawTwo := rawSubscriber(t, mr.Addr(), b.channelFor("ws-2"))
	rawUnsubscribed := rawSubscriber(t, mr.Addr(), b.channelFor("ws-3"))

	b.publishHeartbeats()

	for name, raw := range map[string]<-chan string{"ws-1": rawOne, "ws-2": rawTwo} {
		select {
		case payload := <-raw:
			if !isHeartbeat(payload) {
				t.Fatalf("%s received %q, want a heartbeat", name, payload)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s received no heartbeat", name)
		}
	}

	select {
	case payload := <-rawUnsubscribed:
		t.Fatalf("a heartbeat was published on a channel this instance does not subscribe to (%q): it proves nothing and costs a publish per workspace per interval", payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAHeartbeatConsumesNoEventID is the constraint that keeps the liveness
// probe from manufacturing the resets it exists to avoid.
//
// Three of this bus's reset reasons — counter_backward, epoch_change and
// epoch_regressed — are derived from the shared Redis counter's values. A
// heartbeat that went through Publish would INCR it, inflating the ID space
// every receiving instance reasons about.
func TestAHeartbeatConsumesNoEventID(t *testing.T) {
	b, mr, _, _ := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: no event, so the counter was never established")
	}

	seqKey := b.keys.Name(redisSeqSuffix)
	before, err := mr.Get(seqKey)
	if err != nil {
		t.Fatalf("fixture: reading %s: %v", seqKey, err)
	}

	for range 5 {
		b.publishHeartbeats()
	}
	waitFor(t, "the heartbeats to be delivered", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return true
	})

	after, err := mr.Get(seqKey)
	if err != nil {
		t.Fatalf("reading %s: %v", seqKey, err)
	}
	if before != after {
		t.Fatalf("heartbeats consumed event IDs: %s went %s → %s", seqKey, before, after)
	}
}

// ---------------------------------------------------------------------------
// Wiring (team CONVE-19: a direct-call test vouches for the component, not its
// binding). Every test above calls publishHeartbeats/cycleIdleSubscriptions
// directly; none of them would notice if the constructor stopped starting the
// loop, or if the loop called only one half.
// ---------------------------------------------------------------------------

// TestTheMaintenanceLoopRunsBothHalves drives the loop itself, at a cadence set
// through the same tunables production reads.
func TestTheMaintenanceLoopRunsBothHalves(t *testing.T) {
	t.Run("it publishes", func(t *testing.T) {
		b, mr, _, _ := newHeartbeatBus(t, true)
		ch, _, _ := b.Subscribe(context.Background(), "ws-1")
		defer b.Unsubscribe(ch)
		raw := rawSubscriber(t, mr.Addr(), b.channelFor("ws-1"))

		b.setMaintenanceCadence(5*time.Millisecond, DefaultIdleTimeout)

		select {
		case payload := <-raw:
			if !isHeartbeat(payload) {
				t.Fatalf("first payload from the loop was %q, want a heartbeat", payload)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the maintenance loop published nothing: publishHeartbeats is correct but nothing calls it")
		}
	})

	t.Run("it cycles", func(t *testing.T) {
		b, _, clock, obs := newHeartbeatBus(t, true)
		ch, _, _ := b.Subscribe(context.Background(), "ws-1")
		defer b.Unsubscribe(ch)

		clock.advance(10 * DefaultIdleTimeout)
		b.setMaintenanceCadence(5*time.Millisecond, DefaultIdleTimeout)

		waitFor(t, "the maintenance loop to cycle an idle subscription", func() bool {
			return obs.cycledCount() > 0
		})
	})
}

// TestAQuietWorkspaceIsNotCycledOnPhase1 is the regression test for the defect
// codex round 1 found in this unit's first draft, and it is the one most worth
// keeping.
//
// Idle detection originally ran on EVERY instance from phase 1, on the
// reasoning that it could detect off whatever traffic the deployment already
// carried. That reasoning holds only for a BUSY workspace. A quiet one on
// phase 1 has no events AND no heartbeat, so a perfectly healthy subscription
// crossed the threshold every 90-120s and was cycled — replay coverage
// dropped and every live subscriber told to resync, indefinitely, on the
// DEFAULT configuration that every deployment lands in before it flips
// anything. A resync storm shipped as the default, by the feature whose whole
// purpose is to avoid inverting the load posture.
//
// Fails without the phase gate: this bus is silent for ten thresholds.
func TestAQuietWorkspaceIsNotCycledOnPhase1(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, false)

	ch, _, _ := b.Subscribe(context.Background(), "ws-quiet")
	defer b.Unsubscribe(ch)
	genBefore, ok := b.liveGen("ws-quiet")
	if !ok {
		t.Fatal("fixture: nothing installed")
	}

	for range 10 {
		clock.advance(DefaultIdleTimeout + time.Second)
		b.cycleIdleSubscriptions()
	}

	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("a healthy, quiet workspace was cycled %d times on phase 1: this is a resync storm on the default configuration", got)
	}
	if reasons := obs.resetReasons(); len(reasons) != 0 {
		t.Fatalf("a healthy, quiet workspace had its coverage dropped on phase 1: %v", reasons)
	}
	if genAfter, _ := b.liveGen("ws-quiet"); genAfter != genBefore {
		t.Fatalf("the connection was replaced (generation %d → %d) although nothing was wrong with it", genBefore, genAfter)
	}
}

// TestPhase2CyclesTheSameWorkspace is the counterfactual to the test above: the
// gate must silence phase 1, not the detector. Without this pair, "no cycles"
// is satisfied by a detector that never works at all.
func TestPhase2CyclesTheSameWorkspace(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-quiet")
	defer b.Unsubscribe(ch)

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()

	if got := obs.cycledCount(); got != 1 {
		t.Fatalf("SubscriptionCycled fired %d times on phase 2, want 1: the phase gate has disabled the detector outright", got)
	}
}

// TestClosingTheBusDuringACycleInstallsNothing covers codex round 3's P2.
//
// Close cancels b.ctx and drains wsSubs, but it does NOT join the maintenance
// goroutines — deliberately, because a heartbeat publish against a wedged
// Redis is bounded by go-redis's own timeouts and joining would let a dead
// network hold shutdown open. What must hold instead is that a cycle already
// past its own ctx check cannot leave anything behind: establishSubscription
// re-checks b.ctx under its deciding lock and abandons, closing the PubSub and
// retiring the record in that same section, and the dial itself dies with
// b.ctx (BUG-2749's mergeCancellation) except under TLS, where DialTimeout
// bounds it instead.
//
// The seam runs after the dial and before that decision, which is exactly
// where the race lives.
func TestClosingTheBusDuringACycleInstallsNothing(t *testing.T) {
	b, _, clock, obs := newHeartbeatBus(t, true)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")

	var armed, reached atomic.Bool
	armed.Store(true)
	b.beforeInstallSubscription = func(string) {
		if !armed.Load() || !reached.CompareAndSwap(false, true) {
			return
		}
		b.Close()
	}

	clock.advance(DefaultIdleTimeout + time.Second)
	b.cycleIdleSubscriptions()
	armed.Store(false)

	if !reached.Load() {
		t.Fatal("the cycle never reached an establishment; this test could not have discriminated")
	}

	b.mu.Lock()
	_, live := b.wsSubs["ws-1"]
	_, pending := b.pendingSubs["ws-1"]
	b.mu.Unlock()

	if live {
		t.Fatal("a Redis subscription was installed after Close: its receive loop and connection outlive the bus, and nothing will ever tear them down")
	}
	if pending {
		t.Fatal("an establishment record survived Close: any later subscriber for this workspace waits on a promise nobody keeps")
	}
	// The metric means "torn down AND replaced". A shutdown replaced nothing,
	// and counting it would manufacture exactly the signal an operator reads as
	// "connections to Redis are being blackholed".
	if got := obs.cycledCount(); got != 0 {
		t.Fatalf("SubscriptionCycled fired %d times for a cycle that installed nothing because the bus was closing", got)
	}
	_ = ch
}

// TestClosingTheBusStopsTheMaintenanceLoop pins the teardown half of that
// wiring.
//
// ASSERTED ON THE GOROUTINE, NOT ON REDIS TRAFFIC, and the first version of
// this test got that wrong. Close drains wsSubs, so a loop that ignored b.ctx
// entirely would find no workspaces and publish nothing — silence after Close
// is therefore evidence of nothing at all, and the test passed against a loop
// that went on waking every interval for the life of the process.
func TestClosingTheBusStopsTheMaintenanceLoop(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, redisns.Default, false, true)
	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	raw := rawSubscriber(t, mr.Addr(), b.channelFor("ws-1"))
	b.setMaintenanceCadence(5*time.Millisecond, DefaultIdleTimeout)

	select {
	case <-raw:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the loop never ran, so stopping it proves nothing")
	}

	b.Unsubscribe(ch)
	b.Close()

	select {
	case <-b.maintenanceStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the maintenance loop is still running after Close: it will wake every interval for the life of the process")
	}
}

// TestTheShippedDefaultsAreThreeToOne pins the RELATIONSHIP the ruling is made
// of, not the numbers. The threshold is not a tuned constant: it is three
// publish intervals, so that a single lost or late heartbeat is not a cycle.
// Changing one without the other silently changes what the detector means.
func TestTheShippedDefaultsAreThreeToOne(t *testing.T) {
	if DefaultIdleTimeout != 3*DefaultHeartbeatInterval {
		t.Fatalf("DefaultIdleTimeout = %v, want 3 × DefaultHeartbeatInterval (%v)", DefaultIdleTimeout, 3*DefaultHeartbeatInterval)
	}
	if DefaultHeartbeatInterval != 30*time.Second {
		t.Fatalf("DefaultHeartbeatInterval = %v, want the ruled 30s", DefaultHeartbeatInterval)
	}
	if !strings.HasSuffix(heartbeatPayload, "1") || !strings.HasPrefix(heartbeatPayload, heartbeatPrefix) {
		t.Fatalf("heartbeatPayload = %q, want %q plus a format version", heartbeatPayload, heartbeatPrefix)
	}
}
