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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2/server"
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

	ch, _ := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	// Id 2 exists as far as Redis is concerned, and never reached us.
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
	if missed != nil {
		t.Fatalf("a resume from 1 must report a gap when id 2 exists and we never saw it; got %+v", missed)
	}
}

// TestResumeDoesNotReportAGapWhenTheInstanceIsCurrent is the control leg, and
// the one that stops the check being "always resync". Without it, a build that
// reported a gap on every reconnect would pass the test above.
func TestResumeDoesNotReportAGapWhenTheInstanceIsCurrent(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)

	ch, _ := b.Subscribe()
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

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	ch, _ := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	// Counter says 2 exists; the message is "in flight" and lands mid-settle.
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-2"})
	}()

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	ch, _ := b.Subscribe()
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("precondition: the first notification never arrived")
	}
	b.Unsubscribe(ch)

	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "2"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		// The id the first read was waiting for lands...
		b.fanOutLocally(Notification{ID: 2, Kind: KindComment, ItemRef: "TASK-2"})
		// ...while a THIRD is published elsewhere and never reaches us.
		_ = mr.Set(b.keys.Name(redisWatchSeqSuffix), "3")
	}()

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	ch, _ := b.Subscribe()
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
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "1"); err != nil {
		t.Fatalf("set counter: %v", err)
	}
	go func() {
		time.Sleep(settleWindow / 4)
		_ = mr.Set(b.keys.Name(redisWatchSeqSuffix), "2")
	}()

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	ch, _ := b.Subscribe()
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
	mr.Del(b.keys.Name(redisWatchSeqSuffix))
	if _, err := mr.Lpush(b.keys.Name(redisWatchSeqSuffix), "not-an-integer"); err != nil {
		t.Fatalf("seed a wrong-typed key: %v", err)
	}

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	ch, _ := b.Subscribe()
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

	mr.Del(b.keys.Name(redisWatchSeqSuffix))

	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
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

	if b.resumeOutrunsLocalView(context.Background(), 5) {
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
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	start := time.Now()
	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 0)
	if elapsed := time.Since(start); elapsed >= settleWindow {
		t.Errorf("a fresh subscriber waited %v — it should not pay the settle window at all", elapsed)
	}
	if missed == nil {
		t.Fatal("sinceID=0 is not a resume and must not be answered with a gap")
	}
}

// TestACancelledResumeStopsPayingForTheSettleWindow is BUG-2751's regression
// test.
//
// The settle wait used to be bounded by the BUS's lifetime context, never the
// request's. GET /api/v1/events/stream takes a global AND a per-user admission
// slot before subscribing (BUG-2726) and releases them by defer when the
// handler returns — so a resuming client that disconnected during the window
// held both for the remainder of it plus two Redis round trips. The connection
// was gone; the capacity was not.
//
// THE NEGATIVE CONTROL IS THE POINT, and the bug's own acceptance criteria
// asked for it: a settle window that is simply short would pass either way.
// This asserts against `settleWindow` itself, so it fails against unfixed code
// no matter what that constant is set to — measured before the fix at the full
// 250ms, and after it at well under a tenth of that.
func TestACancelledResumeStopsPayingForTheSettleWindow(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	// Put the counter ahead of this instance so the settle path is actually
	// entered. Without this the function returns on the agreement fast path
	// and the test would pass against any implementation at all.
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	waitFor(t, "the notification to be buffered", func() bool { return len(b.EventsSince(0)) == 1 })
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// CANCELLED FROM THE SEAM, not from a sleep. It has to land strictly
	// INSIDE the window — a caller that was already gone takes the
	// declined-on-entry path instead, which is a different assertion — and a
	// sleep-timed cancellation that lands late does not fail safe here, it
	// silently measures the wrong path (codex round 1).
	var entered atomic.Bool
	b.beforeSettleWait = func() {
		entered.Store(true)
		cancel()
	}

	start := time.Now()
	ch, missed, _ := b.SubscribeAndReplaySince(ctx, 1)
	elapsed := time.Since(start)

	if !entered.Load() {
		t.Fatal("the settle wait was never reached; this test did not exercise the window it is named for")
	}
	if elapsed >= settleWindow {
		t.Fatalf("a cancelled resume waited %v, the full settle window of %v: the caller is gone and its admission slots are still held",
			elapsed, settleWindow)
	}

	// ENDING THE WAIT EARLY IS HALF A FIX. resumeOutrunsLocalView answers
	// false on cancellation, which reads as an ordinary converged resume, so
	// without an explicit decline the rest of the call registers a subscriber
	// and builds a replay for a connection that is unwinding.
	if missed != nil {
		t.Fatalf("a cancelled resume was handed %d replayed notifications", len(missed))
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("a cancelled resume was handed a live subscription channel")
		}
	default:
		t.Fatal("a cancelled resume was registered as a subscriber: the channel is open and empty")
	}
	b.mu.Lock()
	n := len(b.subscribers)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("subscriber count = %d after a cancelled resume, want 0", n)
	}
}

// TestAResumeCancelledBeforeItStartsIsDeclinedOutright is the entry-path twin.
// A client can be gone before the handler ever calls in — the request context
// is cancelled the moment the connection drops, which can precede this call.
// That path never reaches the settle window, so the test above says nothing
// about it.
func TestAResumeCancelledBeforeItStartsIsDeclinedOutright(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	waitFor(t, "the notification to be buffered", func() bool { return len(b.EventsSince(0)) == 1 })
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	var reachedSettle atomic.Bool
	b.beforeSettleWait = func() { reachedSettle.Store(true) }

	// ASSERTED ON THE LOG, not on a proxy for it. Counting Redis reads was the
	// first instrument and it measured nothing: go-redis short-circuits a
	// cancelled context before it touches the wire, so no GET reaches miniredis
	// whether or not the early decline exists, and removing that decline
	// survived. The misleading WARN is emitted from sharedCounter's error path,
	// so the log is the only thing that distinguishes the two.
	var logged logCapture
	restore := captureSlog(&logged)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, missed, _ := b.SubscribeAndReplaySince(ctx, 1)

	if reachedSettle.Load() {
		t.Error("an already-cancelled caller paid for the settle window")
	}
	if msg := logged.firstAtLeast(slog.LevelWarn); msg != "" {
		t.Errorf("an already-cancelled caller logged %q at WARN or above: that line means 'Redis is unhealthy' to "+
			"whoever reads it, and ordinary disconnect churn would fire it on every client that hung up a moment "+
			"before its resume landed", msg)
	}
	if missed != nil {
		t.Fatalf("an already-cancelled caller was handed %d replayed notifications", len(missed))
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("an already-cancelled caller was handed a live subscription channel")
		}
	default:
		t.Fatal("an already-cancelled caller was registered as a subscriber")
	}
	b.mu.Lock()
	n := len(b.subscribers)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("subscriber count = %d, want 0", n)
	}
}

// TestTheMemoryBusDeclinesACancelledResumeToo pins the parity. The two
// implementations answer the same question and must not disagree about whether
// a departed client ends up registered — a divergence there is invisible on a
// single-process deployment right up until it is a leak on a clustered one.
func TestTheMemoryBusDeclinesACancelledResumeToo(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)
	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, missed, _ := b.SubscribeAndReplaySince(ctx, 1)
	if missed != nil {
		t.Fatalf("MemoryBus handed a cancelled caller %d replayed notifications", len(missed))
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("MemoryBus handed a cancelled caller a live subscription channel")
		}
	default:
		t.Fatal("MemoryBus registered a cancelled caller as a subscriber")
	}
}

// TestAnUncancelledResumeStillSettles is the counterfactual. Without it, "the
// wait ended early" is satisfied by an implementation that never waits — which
// would silently undo the convergence check BUG-2731 built and start resyncing
// clients that missed nothing.
func TestAnUncancelledResumeStillSettles(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	waitFor(t, "the notification to be buffered", func() bool { return len(b.EventsSince(0)) == 1 })
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	start := time.Now()
	_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
	elapsed := time.Since(start)

	if elapsed < settleWindow {
		t.Fatalf("a live resume returned in %v, less than the %v settle window: propagation is no longer being given its beat",
			elapsed, settleWindow)
	}
	if missed != nil {
		t.Fatal("the counter never converged, so this resume must be answered with a gap")
	}
}

// TestClosingTheBusStillEndsTheSettleWindow pins the half that threading the
// request context could have silently dropped.
//
// b.ctx is what lets Close cut a wait short. Swapping the wait onto the
// caller's context ALONE would trade one leak for another: a shutdown would
// block behind a client that is still perfectly connected. Both endings are
// reasons to stop, which is why the wait is on a merge of the two rather than
// on either.
func TestClosingTheBusStillEndsTheSettleWindow(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"})
	waitFor(t, "the notification to be buffered", func() bool { return len(b.EventsSince(0)) == 1 })
	if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "99"); err != nil {
		t.Fatalf("set counter: %v", err)
	}

	go func() {
		time.Sleep(settleWindow / 10)
		b.Close()
	}()

	start := time.Now()
	// The caller's context stays live throughout, so only the bus closing can
	// end this wait.
	_, _, _ = b.SubscribeAndReplaySince(context.Background(), 1)

	if elapsed := time.Since(start); elapsed >= settleWindow {
		t.Fatalf("a closing bus waited %v, the full settle window of %v: shutdown is blocked behind a connected client",
			elapsed, settleWindow)
	}
}

// TestACancelledCounterReadIsNotReportedAsRedisTrouble covers the in-flight
// twin of the entry-path decline (codex round 3).
//
// The entry-side decline catches a caller that was ALREADY gone. A caller can
// also leave while the counter GET is in flight, and the error that comes back
// is context.Canceled — indistinguishable, at the sequence-counter WARN, from
// Redis being unreachable. On a stream where clients hang up mid-resume that
// would manufacture exactly the alarm an operator would chase.
//
// DRIVEN AT sharedCounter RATHER THAN THROUGH THE HANDLER, deliberately. What
// is under test is the CLASSIFICATION of the returned error, and staging a real
// mid-flight cancellation means racing go-redis's read against a hook — which
// decides, by timing, whether the error is a context error or a server error.
// That is the thing being classified, so a test that leaves it to chance
// measures whichever case it happened to produce.
func TestACancelledCounterReadIsNotReportedAsRedisTrouble(t *testing.T) {
	b, _ := newMiniredisBus(t, 64)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var logged logCapture
	restore := captureSlog(&logged)
	defer restore()

	if _, ok := b.sharedCounter(ctx); ok {
		t.Fatal("a cancelled read must not be reported as a usable counter value")
	}
	if msg := logged.firstAtLeast(slog.LevelWarn); msg != "" {
		t.Errorf("a cancelled counter read logged %q at WARN or above: disconnect churn is being reported as Redis trouble", msg)
	}
}

// TestAGenuineCounterFailureIsStillReported is the control, and the reason the
// test above is not simply a hole. Codex round 4 BLOCKED the first version of
// this pair because the suppression keyed on ctx.Err() rather than on the
// error: a real Redis failure that merely coincided with a cancellation would
// have been downgraded to Debug and disappeared. Without this leg, "no WARN"
// is satisfied by a bus that has stopped reporting Redis trouble at all.
func TestAGenuineCounterFailureIsStillReported(t *testing.T) {
	b, mr := newMiniredisBus(t, 64)

	var hooked atomic.Bool
	mr.Server().SetPreHook(func(p *server.Peer, cmd string, _ ...string) bool {
		if !strings.EqualFold(cmd, "GET") || !hooked.CompareAndSwap(false, true) {
			return false
		}
		p.WriteError("simulated counter failure")
		return true
	})

	var logged logCapture
	restore := captureSlog(&logged)
	defer restore()

	if _, ok := b.sharedCounter(context.Background()); ok {
		t.Fatal("a failed read must not be reported as a usable counter value")
	}
	if !hooked.Load() {
		t.Fatal("the counter read was never intercepted; this test did not exercise a failure")
	}
	if msg := logged.firstAtLeast(slog.LevelWarn); msg == "" {
		t.Error("a genuine Redis failure logged nothing at WARN: the cancellation suppression is hiding real trouble")
	}
}

// TestCallerIsGoneClassifiesOnTheErrorNotTheContext is the test codex round 4's
// BLOCK actually calls for, and the two integration legs above cannot provide.
//
// The blocked form asked ctx.Err(). Both forms agree on every case a live
// client can be made to produce on demand — a cancelled context yields a
// context error, a live one yields a server error — so the integration pair
// passes against either. They disagree on exactly one case: a GENUINE Redis
// failure whose error arrives while the context happens to be dead. Staging
// that through a client is a race by construction, because go-redis decides by
// timing which error it returns.
//
// Pinning it as a predicate removes the timing. The case that matters is the
// last row.
func TestCallerIsGoneClassifiesOnTheErrorNotTheContext(t *testing.T) {
	serverErr := errors.New("READONLY You can't write against a read only replica")

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a cancelled caller", context.Canceled, true},
		{"a caller whose deadline passed", context.DeadlineExceeded, true},
		{"a wrapped cancellation", fmt.Errorf("get counter: %w", context.Canceled), true},
		{"a redis server error", serverErr, false},
		// THE ROW THE BLOCK WAS ABOUT. Under the ctx.Err() form this was
		// suppressed whenever a disconnect coincided with it, which is the one
		// combination an operator cannot afford to lose.
		{"a redis failure that merely coincides with a disconnect", fmt.Errorf("counter unreadable: %w", serverErr), false},
	} {
		if got := callerIsGone(tc.err); got != tc.want {
			t.Errorf("%s: callerIsGone = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// logCapture records slog records so a test can assert on what an OPERATOR
// would see. Used where the defect is a misleading line rather than a wrong
// value — see TestAResumeCancelledBeforeItStartsIsDeclinedOutright.
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// firstAtLeast returns the first message logged at or above level, or "".
func (c *logCapture) firstAtLeast(level slog.Level) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level >= level {
			return r.Message
		}
	}
	return ""
}

func captureSlog(c *logCapture) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	return func() { slog.SetDefault(prev) }
}
