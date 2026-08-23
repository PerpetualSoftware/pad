package watchevents

// BUG-2739 — the two holes this bus could not SEE.
//
// BUG-2730 made the holes this instance DETECTS reach the client holding the
// stream open. These are the ones it never detected: a pub/sub resubscription
// (go-redis reconnects and re-subscribes silently, so the outage's
// notifications simply never arrive) and a message that cannot be decoded.
// Both leave the replay buffer claiming a span it no longer has, and both are
// invisible to id arithmetic when the lost notification is the NEWEST one and
// the stream then goes quiet — which is the case a connected CLI sits in
// forever.
//
// SCOPED TO THE OPEN STREAM, deliberately. A RECONNECTING client was already
// covered before this work, because resumeOutrunsLocalView asks the shared
// counter instead of trusting local state — which is why several tests here
// assert through EventsSince as well: it is the only reader that answers from
// local state alone, so it can tell this fix apart from that one.

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// tcpCutter is a pass-through proxy in front of Redis whose connections can be
// severed on demand. Ported from internal/events' redis_reconnect_test.go for
// the reason its header gives, which applies unchanged here: a RESUBSCRIPTION
// is the thing under test, and nothing short of a real dropped connection
// produces one.
//
// Testing the decision logic alone — call dropCoverage and check the counters
// — would leave the load-bearing half unproven, namely that the receive loop
// uses a channel form that SURFACES subscription confirmations rather than one
// that swallows them. CONVE-19: wiring is a claim.
type tcpCutter struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newTCPCutter(t *testing.T, backend string) *tcpCutter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	c := &tcpCutter{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", backend)
			if err != nil {
				_ = client.Close()
				return
			}
			c.mu.Lock()
			c.conns = append(c.conns, client, server)
			c.mu.Unlock()
			go func() { _, _ = io.Copy(server, client) }()
			go func() { _, _ = io.Copy(client, server) }()
		}
	}()
	return c
}

func (c *tcpCutter) addr() string { return c.ln.Addr().String() }

// cut severs every connection opened so far, forcing go-redis to redial and
// re-subscribe.
func (c *tcpCutter) cut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = nil
}

// newCutterBus builds a bus whose Redis connection can be severed, plus the
// miniredis behind it, so a test can publish directly to the channel while the
// bus is not looking.
func newCutterBus(t *testing.T, size int) (*RedisBus, *miniredis.Miniredis, *tcpCutter, *recordingObserver) {
	t.Helper()
	mr := miniredis.RunT(t)
	cutter := newTCPCutter(t, mr.Addr())

	client := redis.NewClient(&redis.Options{Addr: cutter.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithReplaySize(client, size)
	t.Cleanup(b.Close)

	obs := newRecordingObserver()
	b.SetObserver(obs)
	return b, mr, cutter, obs
}

// waitForReset blocks until the observer has recorded a reset FOR THE GIVEN
// REASON, returning the counts. It fails the test rather than returning empty,
// so a caller cannot mistake "not yet" for "never".
//
// Reason-specific rather than "any reset", which is what it was first written
// as (codex round 3). Waiting on any reset lets an unrelated one satisfy the
// wait and lets the test proceed to its real assertions before the condition
// under test has happened — the wait would then be measuring nothing, and the
// assertions after it would be racing. It also makes the failure message name
// what was expected instead of reporting a bare absence.
func waitForReset(t *testing.T, obs *recordingObserver, reason, what string) observerCounts {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := obs.snapshot()
		if got.resets[reason] > 0 {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; no %q reset was ever reported (saw %v)",
		what, reason, obs.snapshot().resets)
	return observerCounts{}
}

// THE RECOVERY CASE, WRITTEN FIRST AND DELIBERATELY (BUG-2739).
//
// dropCoverage resets three fields together, and the failure this test exists
// for is the one that looks correct: clear the buffer and knownFrom, leave
// lastAppendedID at its pre-outage value, and the NEXT notification reads as
// the contiguous successor of an id we no longer hold. No arm of
// fanOutLocally's switch fires, knownFrom is never re-established, and
// replaySince refuses EVERY resume on this instance from then on — a fix that
// reads right and bricks resumes permanently.
//
// Asserting only the refusal cannot see that: a bricked bus refuses too.
func TestCoverageIsReestablishedByTheNextNotification(t *testing.T) {
	b, _, cutter, obs := newCutterBus(t, 64)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)

	cutter.cut()
	waitForReset(t, obs, ResetReasonSubscriptionResumed, "a resubscription after the connection was severed")

	// The bus is now covering nothing. Publish again: this notification is
	// received normally, and its arrival is what re-establishes coverage.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"}); err != nil {
		t.Fatalf("publish after the flap: %v", err)
	}
	second := drainOne(t, ch)

	// THE ASSERTION THAT DISCRIMINATES. A client resuming from the id JUST
	// BELOW the one that re-established coverage is contiguous with our view
	// and must be served — knownFrom was set to exactly that notification's
	// id by the cold-start arm. If lastAppendedID had survived the drop, this
	// resume is refused, and it is refused for every id, forever.
	resumeCh, replay, _ := b.SubscribeAndReplaySince(second.ID - 1)
	defer b.Unsubscribe(resumeCh)
	if replay == nil {
		t.Fatal("coverage was never re-established: a resume from the id below the first post-outage notification " +
			"was refused. dropCoverage must reset lastAppendedID alongside knownFrom and the buffer, " +
			"or the next notification looks contiguous, no arm fires, and every resume on this instance is refused forever")
	}
	if len(replay) != 1 || replay[0].ItemRef != "TASK-2" {
		t.Fatalf("the re-established coverage must replay exactly the post-outage notification, got %+v", replay)
	}
}

// The refusal half: a cursor from BEFORE the outage is no longer served,
// because this instance cannot account for what it missed while it was
// disconnected.
func TestAResubscriptionEndsThisInstancesCoverage(t *testing.T) {
	b, _, cutter, obs := newCutterBus(t, 64)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// TWO notifications, and the cursor is the FIRST one's id — not id-1.
	// A sinceID of 0 is a fresh subscriber asking for everything buffered
	// rather than a resume from a position, and replaySince short-circuits
	// the coverage check for it: it would answer an empty-but-non-nil slice
	// whatever the coverage state, so a test built on it asserts nothing.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := drainOne(t, ch)
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)

	// CONTROL: while the connection is healthy that cursor IS served and
	// nothing is reported. Without this leg the test passes against a bus
	// that refuses every resume and reports a reset on every message — a
	// differently-broken instrument, indistinguishable from the assertions
	// below on their own.
	healthyCh, healthy, _ := b.SubscribeAndReplaySince(first.ID)
	b.Unsubscribe(healthyCh)
	if healthy == nil {
		t.Fatal("a cursor inside our coverage must be served while the subscription is healthy")
	}
	if got := obs.snapshot(); len(got.resets) != 0 {
		t.Fatalf("a healthy subscription must report no reset, got %v", got.resets)
	}

	cutter.cut()

	got := waitForReset(t, obs, ResetReasonSubscriptionResumed, "a resubscription after the connection was severed")
	if got.resets[ResetReasonSubscriptionResumed] == 0 {
		t.Fatalf("a pub/sub resubscription must be reported as %q, got %v",
			ResetReasonSubscriptionResumed, got.resets)
	}

	// And the coverage really is gone for the pre-outage cursor.
	refusedCh, refused, _ := b.SubscribeAndReplaySince(first.ID)
	defer b.Unsubscribe(refusedCh)
	if refused != nil {
		t.Fatalf("after a resubscription the buffer must not vouch for the outage, got %d notifications", len(refused))
	}

	// THE LOCAL VIEW, ASSERTED SEPARATELY. The refusal above goes through
	// SubscribeAndReplaySince, which consults the shared counter BEFORE local
	// state (resumeOutrunsLocalView) — so it can pass on the authority
	// check alone, a mechanism with nothing to do with this fix. EventsSince
	// deliberately skips that check and answers from local state only, so
	// this leg says the instance itself does not vouch for the span.
	//
	// HONEST SCORING (CONVE-12 cuts both ways): this leg does NOT currently
	// kill a mutation the other legs miss. Removing `knownFrom = 0` from
	// dropCoverage survives it, because the emptied buffer makes
	// replayBuffer.since answer nil for any sinceID > 0 regardless — see the
	// note on that line in dropCoverage. Kept anyway, because the invariant
	// it states is the one that matters in production when the authority
	// check is unavailable: a failed Redis read makes resumeOutrunsLocalView
	// answer false, and a flap is exactly when Redis reads fail.
	if local := b.EventsSince(first.ID); local != nil {
		t.Fatalf("this instance must not vouch for that span from local state alone, got %d notifications — "+
			"dropCoverage must set knownFrom = 0, not merely empty the buffer", len(local))
	}

	// AND A SUBSCRIBER ARRIVING WITH NO CURSOR IS NOT HANDED THE PRE-OUTAGE
	// BUFFER. sinceID == 0 means "everything you have buffered", and it does
	// NOT go through the coverage guard — replaySince short-circuits it — so
	// a dropCoverage that clears the bookkeeping without emptying the buffer
	// replays the pre-flap notifications to a client that then believes it is
	// caught up at the newest of them. It is not: the ids lost in the flap sit
	// just above that, nothing later will be non-contiguous (lastAppendedID is
	// back to 0, so the next notification takes the cold-start arm), and this
	// subscriber is not in the signalled set because it arrived afterwards.
	// Silent loss, handed out by us. Verified by mutation: keeping the buffer
	// survives every other assertion here.
	freshCh, fresh, _ := b.SubscribeAndReplaySince(0)
	defer b.Unsubscribe(freshCh)
	if len(fresh) != 0 {
		t.Fatalf("a subscriber arriving after the outage must not be handed the pre-outage buffer, got %d notifications", len(fresh))
	}
}

// THE PROPERTY THE UNIT EXISTS FOR: the client holding the stream OPEN across
// the flap is told, not merely the one that reconnects. A CLI sitting on
// /api/v1/events/stream through a Redis failover was previously told nothing
// and stayed stale indefinitely, because nothing later ever exposed the hole.
func TestAResubscriptionSignalsEverySubscriberHoldingTheStreamOpen(t *testing.T) {
	b, _, cutter, obs := newCutterBus(t, 64)

	chA, gapsA := b.Subscribe()
	defer b.Unsubscribe(chA)
	chB, gapsB := b.Subscribe()
	defer b.Unsubscribe(chB)

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, chA)
	drainOne(t, chB)

	// CONTROL: a delivered notification on its own does not raise the flag.
	if raised(gapsA) || raised(gapsB) {
		t.Fatal("an ordinary delivery must not signal a gap")
	}

	cutter.cut()
	waitForReset(t, obs, ResetReasonSubscriptionResumed, "a resubscription after the connection was severed")

	if !raised(gapsA) || !raised(gapsB) {
		t.Fatal("every subscriber connected across the outage must be signalled, not just a reconnecting one")
	}
}

// An unreadable message makes coverage unprovable, so coverage ends rather
// than the message being discarded.
//
// It is NOT enough that this bus's ids are consecutive by construction and the
// gap arm would catch it on the next notification: detection through the gap
// arm needs a LATER notification to arrive, and the case that matters is an
// undecodable NEWEST message on a stream that then goes quiet. So the test
// publishes NOTHING afterwards — which is exactly what makes it discriminate.
//
// Note what the instance does and does not know here, because the assertion
// is about coverage rather than about loss: it knows a message it could not
// read arrived on its channel, NOT that a notification was missed. The
// payload could equally be something foreign. Ending coverage is the honest
// response to not being able to tell, which is why the test asserts a refusal
// and a signal rather than any claim about a missing id.
func TestAnUndecodableMessageEndsCoverage(t *testing.T) {
	b, mr, _, obs := newCutterBus(t, 64)

	ch, gaps := b.Subscribe()
	defer b.Unsubscribe(ch)

	// Two, for the same reason as the resubscription test: the cursor has to
	// be a real resume position, and sinceID == 0 is not one.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	first := drainOne(t, ch)
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)

	if raised(gaps) {
		t.Fatal("an ordinary delivery must not signal a gap")
	}

	// CONTROL: that cursor is served right now.
	healthyCh, healthy, _ := b.SubscribeAndReplaySince(first.ID)
	b.Unsubscribe(healthyCh)
	if healthy == nil {
		t.Fatal("a cursor inside our coverage must be served before the undecodable message arrives")
	}

	// Something this installation did not publish lands on the watch channel.
	// Straight to Redis, bypassing the publish script, which is the only way
	// to produce a payload the codec rejects.
	mr.Publish(b.keys.Name(redisWatchChannelSuffix), "this is not <epoch>|<id>|<json>")

	got := waitForReset(t, obs, ResetReasonUndecodableMessage, "an undecodable message on the watch channel")
	if got.resets[ResetReasonUndecodableMessage] == 0 {
		t.Fatalf("an undecodable message must be reported as %q, got %v",
			ResetReasonUndecodableMessage, got.resets)
	}

	// NOTHING IS PUBLISHED AFTER THIS POINT. The cursor that was inside our
	// coverage a moment ago is refused now, on a stream that has gone quiet —
	// the case id arithmetic alone can never reach.
	refusedCh, refused, _ := b.SubscribeAndReplaySince(first.ID)
	defer b.Unsubscribe(refusedCh)
	if refused != nil {
		t.Fatalf("after an undecodable message the buffer must not vouch for the span, got %d notifications", len(refused))
	}

	// THE LOCAL VIEW, ASSERTED SEPARATELY. The refusal above goes through
	// SubscribeAndReplaySince, which consults the shared counter BEFORE local
	// state (resumeOutrunsLocalView) — so it can pass on the authority
	// check alone, a mechanism with nothing to do with this fix. EventsSince
	// deliberately skips that check and answers from local state only, so
	// this leg says the instance itself does not vouch for the span.
	//
	// HONEST SCORING (CONVE-12 cuts both ways): this leg does NOT currently
	// kill a mutation the other legs miss. Removing `knownFrom = 0` from
	// dropCoverage survives it, because the emptied buffer makes
	// replayBuffer.since answer nil for any sinceID > 0 regardless — see the
	// note on that line in dropCoverage. Kept anyway, because the invariant
	// it states is the one that matters in production when the authority
	// check is unavailable: a failed Redis read makes resumeOutrunsLocalView
	// answer false, and a flap is exactly when Redis reads fail.
	if local := b.EventsSince(first.ID); local != nil {
		t.Fatalf("this instance must not vouch for that span from local state alone, got %d notifications — "+
			"dropCoverage must set knownFrom = 0, not merely empty the buffer", len(local))
	}

	if !raised(gaps) {
		t.Fatal("the subscriber holding the stream open must be told about a message we could not read")
	}
}

// THE ENFORCEMENT FOR A CLAIM THE RECEIVE LOOP MAKES ABOUT ANOTHER FUNCTION.
//
// receiveMessages has no "skip the first confirmation" flag — unlike
// internal/events' loop — because NewRedisBusWithKeys calls pubsub.Receive
// before the goroutine starts and that Receive consumes the initial subscribe
// confirmation. Remove the constructor's Receive and the initial confirmation
// reaches the channel instead, and every bus in the fleet announces a hole to
// its subscribers the moment it starts.
//
// A comment cannot hold that. This does: it constructs a bus, lets it settle,
// and asserts NO reset and NO raised gap.
//
// IT PASSES ON UNFIXED main, AND THAT IS CORRECT — it is a control, not a
// regression test. The unfixed Channel() discards confirmations, so nothing
// is reported either way. Its discriminating mutation is the removal of the
// constructor's Receive, which was RUN rather than assumed: with
// `pubsub.Receive(subCtx)` deleted this fails 3/3 with
// `got map[subscription_resumed:1]`.
//
// Codex round 3 argued the guard could not work, on the theory that the
// initial confirmation would arrive before SetObserver and Subscribe are
// called and so go unrecorded. It does not: go-redis does not deliver the
// confirmation until the channel goroutine started by
// ChannelWithSubscriptions reads it, which is after the constructor returns
// and after newCutterBus attaches the observer. Settled by running the
// mutation, not by arguing the timing.
func TestNoCoverageIsDroppedAtStartup(t *testing.T) {
	b, _, _, obs := newCutterBus(t, 64)

	ch, gaps := b.Subscribe()
	defer b.Unsubscribe(ch)

	// Long enough for a startup confirmation to have arrived on the channel
	// if the constructor were no longer consuming it. The probe that
	// established this behaviour saw zero within 1.5s.
	time.Sleep(500 * time.Millisecond)

	if got := obs.snapshot(); len(got.resets) != 0 {
		t.Fatalf("a bus that has merely started must not drop coverage, got %v — "+
			"has NewRedisBusWithKeys stopped consuming the initial subscribe confirmation?", got.resets)
	}
	if raised(gaps) {
		t.Fatal("a subscriber on a freshly started bus must not be told it missed anything")
	}

	// And it still works: the premise of the assertions above is a LIVE
	// subscription, not a dead one that reports nothing because it receives
	// nothing.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)
}

// drainOne reads one notification or fails.
func drainOne(t *testing.T, ch chan Notification) Notification {
	t.Helper()
	select {
	case n, ok := <-ch:
		if !ok {
			t.Fatal("subscriber channel closed while waiting for a notification")
		}
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}
	return Notification{}
}

// Close must not look like a resubscription (codex round 7).
//
// WHAT THIS DOES AND DOES NOT PIN, because the first version of this comment
// got it wrong and the mutation said so. It pins the INVARIANT — a graceful
// stop must not move an operator's failover counter — and that invariant is
// worth a test whichever mechanism upholds it.
//
// It does NOT exercise the Kind filter in receiveMessages. Deleting that
// filter leaves this test green, because Close cancels b.ctx and the loop
// takes the ctx.Done() case (or sees the channel closed) rather than
// processing any unsubscribe confirmation. RedisBus never calls
// pubsub.Unsubscribe, so in practice nothing reaches that branch: the filter
// is defence, and is labelled as such where it lives.
//
// The assertion is a DIFFERENCE across Close rather than a final total, so a
// pre-existing reset from earlier in the test cannot mask a new one.
func TestCloseDoesNotLookLikeAResubscription(t *testing.T) {
	b, _, _, obs := newCutterBus(t, 64)

	ch, _ := b.Subscribe()
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)

	before := obs.snapshot().resets[ResetReasonSubscriptionResumed]

	b.Close()
	// Close returns only after wg.Wait(), so the receive loop has finished by
	// here and any confirmation it was going to mishandle already went
	// through. No sleep is needed and none would make this stronger.

	if after := obs.snapshot().resets[ResetReasonSubscriptionResumed]; after != before {
		t.Fatalf("Close must not report a resubscription: %q went %d -> %d. "+
			"Something on the shutdown path is reaching dropCoverage — check whether receiveMessages "+
			"now processes a confirmation on the way out instead of leaving through ctx.Done()",
			ResetReasonSubscriptionResumed, before, after)
	}
}

// A resubscription with NOTHING in the buffer yet (codex round 7).
//
// Every other reconnect test publishes first, so all of them enter
// dropCoverage with real bookkeeping to clear. An instance that has been up
// but idle — a fresh replica, or any instance during a quiet period — has
// lastAppendedID and knownFrom already at 0 and an empty buffer, and its
// subscribers are exactly the ones a flap can strand, because there is no
// later notification pending to expose anything.
//
// The behaviour under test is that it still REPORTS and still SIGNALS. A
// dropCoverage guarded by "only if we were covering something" would look
// like a reasonable optimisation and would silently reintroduce this bug for
// the quietest instances, which are the ones it hurts most.
func TestAResubscriptionOnAnIdleBusStillEndsCoverage(t *testing.T) {
	b, _, cutter, obs := newCutterBus(t, 64)

	ch, gaps := b.Subscribe()
	defer b.Unsubscribe(ch)

	// CONTROL: nothing has been published, so nothing has been reported.
	if got := obs.snapshot(); len(got.resets) != 0 {
		t.Fatalf("an idle bus must report nothing before the cut, got %v", got.resets)
	}
	if raised(gaps) {
		t.Fatal("an idle bus must not signal a gap before the cut")
	}

	cutter.cut()

	waitForReset(t, obs, ResetReasonSubscriptionResumed, "a resubscription on an idle bus")
	if !raised(gaps) {
		t.Fatal("a subscriber connected across a flap must be told even when the bus had buffered nothing — " +
			"an empty buffer is not evidence that nothing was missed, it is the absence of evidence either way")
	}
}
