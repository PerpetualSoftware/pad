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
	"context"
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
	resumeCh, replay, _ := b.SubscribeAndReplaySince(context.Background(), second.ID-1)
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
	healthyCh, healthy, _ := b.SubscribeAndReplaySince(context.Background(), first.ID)
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
	refusedCh, refused, _ := b.SubscribeAndReplaySince(context.Background(), first.ID)
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
	freshCh, fresh, _ := b.SubscribeAndReplaySince(context.Background(), 0)
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
	healthyCh, healthy, _ := b.SubscribeAndReplaySince(context.Background(), first.ID)
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
	refusedCh, refused, _ := b.SubscribeAndReplaySince(context.Background(), first.ID)
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
// ONE RESIDUAL, STATED BECAUSE IT IS REAL AND MEASURED RATHER THAN DISMISSED
// (codex rounds 3 and 17 both raised it). The confirmation is recorded only if
// it arrives after newCutterBus attaches the observer, and nothing ORDERS
// those two — so in principle a mutated constructor could let it slip through
// unobserved and this test would pass on broken code.
//
// It does not happen, and the asymmetry is why: the confirmation needs a round
// trip through a TCP proxy to miniredis and back into a goroutine that has not
// started yet, while SetObserver is two field writes on the goroutine that
// just returned from the constructor. Measured with the mutation applied
// rather than argued — 50/50 caught plain, and 30/30 caught under -race, which
// perturbs scheduling hard enough to surface a genuine window.
//
// Closing it properly would need a construction-time observer seam, i.e.
// widening production API for a test. The measurement is the better trade, and
// it is recorded here so a future flake is read as this window opening rather
// than as noise.
func TestNoCoverageIsDroppedAtStartup(t *testing.T) {
	b, _, _, obs := newCutterBus(t, 64)

	ch, gaps := b.Subscribe()
	defer b.Unsubscribe(ch)

	// ORDERED, NOT TIMED (codex round 16). The first version slept 500ms and
	// hoped. This publishes and waits for DELIVERY instead, which gives a real
	// happens-before: the pub/sub channel is FIFO, so a startup confirmation —
	// if the constructor were no longer consuming it — is queued AHEAD of this
	// notification and has necessarily been processed by the time the
	// notification comes out the other end. Receiving it therefore proves the
	// loop has already had its chance to mishandle the confirmation.
	//
	// It also makes the assertions' premise explicit: a subscription that
	// delivers is a LIVE one, not a dead loop reporting nothing because it
	// receives nothing.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	drainOne(t, ch)

	if got := obs.snapshot(); len(got.resets) != 0 {
		t.Fatalf("a bus that has merely started must not drop coverage, got %v — "+
			"has NewRedisBusWithKeys stopped consuming the initial subscribe confirmation?", got.resets)
	}
	if raised(gaps) {
		t.Fatal("a subscriber on a freshly started bus must not be told it missed anything")
	}
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

// A COUNTER RESET THAT HAPPENS DURING THE RECONNECT WINDOW (codex round 13).
//
// The two conditions were covered separately and their COMPOSITION was not,
// which is where the regression lived: dropCoverage zeroes lastAppendedID, so
// a reset that arrives after it looks like an ordinary cold start and
// counter_backward is never reported. Not an exotic pairing — a Redis
// restarted from a stale snapshot drops every connection AND restores
// watchevents_seq to an older value in one event.
//
// Driven through fanOutLocally rather than through a real Redis, because
// miniredis will not evict a key mid-test on demand; the arm under test is
// the local bookkeeping, and the reconnect half is covered by the tests above
// that DO sever a connection.
func TestACounterResetDuringTheReconnectWindowIsStillReported(t *testing.T) {
	b := newLocalOnlyBus(64)
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	for _, id := range []int64{1, 2, 3} {
		b.fanOutLocally(Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"})
	}

	// CONTROL: an ordinary contiguous stream reports nothing.
	if got := obs.snapshot(); len(got.resets) != 0 || got.gaps != 0 {
		t.Fatalf("a contiguous stream must report nothing, got resets=%v gaps=%d", got.resets, got.gaps)
	}

	// The outage. dropCoverage zeroes lastAppendedID; the counter is reset in
	// Redis while we are away.
	b.dropCoverage(ResetReasonSubscriptionResumed)

	// The first notification of the restarted sequence.
	b.fanOutLocally(Notification{ID: 1, Kind: KindComment, ItemRef: "TASK-2"})

	got := obs.snapshot()
	if got.resets[ResetReasonCounterBackward] != 1 {
		t.Fatalf("a counter reset inside the reconnect window must still be reported as %q, got %v — "+
			"lastAppendedID does not survive dropCoverage, so backward detection needs the separate high water mark",
			ResetReasonCounterBackward, got.resets)
	}

	// AND THE MARK REBASED, so the restarted sequence is not read as a run of
	// further resets. Without the rebase this is where one incident becomes a
	// stream of false counter_backward reports.
	b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-3"})
	b.dropCoverage(ResetReasonSubscriptionResumed)
	b.fanOutLocally(Notification{ID: 3, Kind: KindComment, ItemRef: "TASK-4"})

	if after := obs.snapshot().resets[ResetReasonCounterBackward]; after != 1 {
		t.Fatalf("ids climbing within the NEW space must not report further resets: %q went 1 -> %d",
			ResetReasonCounterBackward, after)
	}
}

// The same rebase obligation on the ORIGINAL backward arm — the one that fires
// when lastAppendedID is still set — which is a separate site with the
// identical failure mode and was covered by nothing.
//
// A reset detected there leaves the instance tracking a small id while the
// high water mark still holds the old space's peak. Every later id of the
// restarted sequence is below that peak, so the first coverage drop after it
// turns one incident into a second, false reset — and then another, and
// another. Verified by mutation: without this test, dropping the rebase from
// that arm survives the suite.
func TestTheOriginalBackwardArmRebasesTheHighWaterMark(t *testing.T) {
	b := newLocalOnlyBus(64)
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	for _, id := range []int64{98, 99, 100} {
		b.fanOutLocally(Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"})
	}

	// The counter resets while we are CONNECTED, so the original arm sees it.
	b.fanOutLocally(Notification{ID: 1, Kind: KindComment, ItemRef: "TASK-2"})
	if got := obs.snapshot(); got.resets[ResetReasonCounterBackward] != 1 {
		t.Fatalf("the connected counter reset must be reported once, got %v", got.resets)
	}

	// The new space climbs normally, then a flap drops coverage.
	b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-3"})
	b.dropCoverage(ResetReasonSubscriptionResumed)
	b.fanOutLocally(Notification{ID: 3, Kind: KindComment, ItemRef: "TASK-4"})

	if after := obs.snapshot().resets[ResetReasonCounterBackward]; after != 1 {
		t.Fatalf("id 3 of the restarted sequence is not a second reset: %q went 1 -> %d — "+
			"the original backward arm must rebase the high water mark onto the new space",
			ResetReasonCounterBackward, after)
	}
}

// An epoch change discards the high water mark, because ids from a new space
// are not comparable with the old space's peak.
//
// Without this the first notification of a new epoch — which legitimately
// restarts low — would be reported as a counter reset, turning every
// id-space migration into a false alarm on the metric operators use to WATCH
// that migration.
func TestAnEpochChangeDiscardsTheHighWaterMark(t *testing.T) {
	b := newLocalOnlyBus(64)
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	b.fanOutFromRedis("epoch-a", Notification{ID: 90, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())
	b.fanOutFromRedis("epoch-a", Notification{ID: 91, Kind: KindComment, ItemRef: "TASK-2"}, b.currentGen())

	// The migration: new epoch, ids restart from 1.
	b.fanOutFromRedis("epoch-b", Notification{ID: 1, Kind: KindComment, ItemRef: "TASK-3"}, b.currentGen())

	got := obs.snapshot()
	if got.resets[ResetReasonEpochChange] != 1 {
		t.Fatalf("the epoch change must be reported, got %v", got.resets)
	}
	if got.resets[ResetReasonCounterBackward] != 0 {
		t.Fatalf("a new epoch's low ids are not a counter reset, got %v — "+
			"the high water mark must be discarded with the epoch it belonged to", got.resets)
	}

	// THE LEG THAT ACTUALLY DISCRIMINATES, and the assertion above does not:
	// the epoch arm sets epochJustChanged, which suppresses backward
	// detection for the very next notification whether or not the mark was
	// cleared. A STALE mark only bites later — after a coverage drop, when an
	// ordinary id of the new space falls below the OLD space's peak and is
	// read as a reset. Verified by mutation: without this leg, deleting
	// `highWaterID = 0` from the epoch arm survives the whole suite.
	b.fanOutFromRedis("epoch-b", Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-4"}, b.currentGen())
	b.dropCoverage(ResetReasonSubscriptionResumed)
	b.fanOutFromRedis("epoch-b", Notification{ID: 3, Kind: KindComment, ItemRef: "TASK-5"}, b.currentGen())

	if after := obs.snapshot().resets[ResetReasonCounterBackward]; after != 0 {
		t.Fatalf("an ordinary id of the new epoch must not be read as a counter reset after a coverage drop, got %d — "+
			"the old epoch's high water mark was not discarded", after)
	}
}

// A CURSOR FROM THE OLD ID SPACE MUST NOT BE SERVED THE NEW ONE (BUG-2739,
// codex round 21).
//
// Both backward arms set knownFrom to the first id they see of the restarted
// sequence, which ADMITS a resume from one below it — and one below it is
// exactly the ambiguous cursor, because the two spaces overlap and the client
// may be holding the OLD sequence's copy of that id. Serving it replays the
// new space's notifications as though they followed the client's old cursor,
// which is the corruption these arms exist to prevent.
//
// Pre-existing on the connected path (verified by running this case against
// unmodified main, where it fails), and inherited by the arm this branch
// added — so both are covered here, and the two must not drift apart.
//
// The assertion is through EventsSince because it answers from LOCAL state
// only: the shared-counter check cannot see this at all, since after the reset
// the remote counter and our high-water mark AGREE on the new space's value.
//
// SCOPE, so this test is not read as more than it is (codex round 22): it
// pins the boundary at n.ID-1, and n.ID ITSELF is still admitted. If the old
// space also reached n.ID that cursor remains ambiguous, as does every
// old-space id up to the old high water mark. No constant here closes that —
// it needs a boundary that remembers the old space's extent, which is
// BUG-2743. The +1 is a strict improvement over n.ID and not a complete fix,
// and the epoch is what actually distinguishes two sequences.
func TestAnOldSpaceCursorIsRefusedAfterACounterReset(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		drop bool
	}{
		{"reset seen while connected", false},
		{"reset seen across a coverage drop", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newLocalOnlyBus(64)
			defer b.Close()

			for _, id := range []int64{198, 199, 200} {
				b.fanOutLocally(Notification{ID: id, Kind: KindComment, ItemRef: "OLD"})
			}

			// CONTROL: inside the OLD space, a cursor below our newest is
			// served. Without this leg the test passes against a bus that
			// refuses every resume.
			if got := b.EventsSince(198); len(got) != 2 {
				t.Fatalf("a cursor inside the old space must be served, got %d notifications", len(got))
			}

			if tc.drop {
				b.dropCoverage(ResetReasonSubscriptionResumed)
			}

			// The counter restarted while we were away and climbed to 100
			// without us; 100 is the first id of the new space we receive.
			b.fanOutLocally(Notification{ID: 100, Kind: KindComment, ItemRef: "NEW"})

			if got := b.EventsSince(99); got != nil {
				t.Fatalf("cursor 99 is ambiguous — it may be the OLD space's 99 — and must be refused, "+
					"got %d notifications; knownFrom must be n.ID+1 after a counter reset, not n.ID", len(got))
			}

			// AND THE NEW SPACE STILL WORKS: a client genuinely at 100 is
			// served whatever follows. Without this leg, refusing everything
			// forever would pass the assertion above.
			b.fanOutLocally(Notification{ID: 101, Kind: KindComment, ItemRef: "NEWER"})
			got := b.EventsSince(100)
			if len(got) != 1 || got[0].ItemRef != "NEWER" {
				t.Fatalf("a cursor inside the NEW space must be served, got %+v", got)
			}
		})
	}
}
