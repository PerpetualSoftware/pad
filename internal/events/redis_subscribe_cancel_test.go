package events

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2749: a client that disconnects while its workspace's Redis
// subscription is being established used to hold both admission slots — the
// process-wide one in internal/server and the per-workspace one here — for the
// whole of the dial plus the acknowledgement bound. The connection was gone;
// the capacity was not.
//
// The fix is a RULE rather than a mechanism: cancellation is deregistration,
// and wsCounts — which already answers "is anyone still here" — decides
// everything downstream of that. These tests pin the two cancellation
// positions separately, because they take different paths through
// establishSubscription and only one of them owes the joiners anything.

// newCancelTestBus is newTestRedisBus plus the miniredis handle, which these
// tests need in order to assert about the SERVER's view of the subscription
// rather than only the bus's own bookkeeping.
func newCancelTestBus(t *testing.T) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	return b, mr
}

func (b *RedisBus) pendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pendingSubs)
}

func (b *RedisBus) hasWorkspaceSub(workspaceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.wsSubs[workspaceID]
	return ok
}

// TestCancelBeforeInstallReleasesTheSlotAndInstallsNothing is the pre-install
// half: the caller's context dies after the dial has returned but BEFORE the
// section that decides whether to install, so that decision finds an
// establisher who is no longer there.
//
// It is deliberately not a test of cancellation DURING the dial. The seam it
// uses runs once client.Subscribe has returned, and a test that genuinely
// stalled a TCP connect would be testing the operating system's accept
// backlog. The dial's own binding to the caller's context is pinned separately
// and structurally by TestTheDialUsesTheCallersContext.
//
// THE ASSERTION IS ABOUT WHAT WAS LEFT BEHIND, not about the return value
// alone (CONVE-12). A bus that returned SubscribeCancelled and still installed
// a subscription would satisfy a return-value check while leaking exactly the
// connection and receive loop this unit is about, so the subscriber count, the
// wsSubs entry, the establishment record and miniredis's own subscriber count
// are all pinned.
//
// Fails before the fix: the context is not consulted at all, so this returns a
// live subscription with the workspace count at 1.
func TestCancelBeforeInstallReleasesTheSlotAndInstallsNothing(t *testing.T) {
	b, mr := newCancelTestBus(t)

	ctx, cancel := context.WithCancel(context.Background())
	b.beforeInstallSubscription = func(string) { cancel() }

	ch, gaps, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0)

	if outcome != SubscribeCancelled {
		t.Fatalf("outcome = %v, want SubscribeCancelled", outcome)
	}
	if ch != nil || gaps != nil {
		t.Error("a cancelled subscribe handed back channels; nothing is owed to a caller that left")
	}
	if n := b.WorkspaceSubscriberCount("ws-1"); n != 0 {
		t.Errorf("workspace subscriber count = %d, want 0 — the per-workspace slot is still held", n)
	}
	if b.hasWorkspaceSub("ws-1") {
		t.Error("a Redis subscription was installed for a caller that had already gone")
	}
	if n := b.pendingCount(); n != 0 {
		t.Errorf("pendingSubs = %d, want 0 — an establishment record outlived its establisher, and the next caller will wait on it forever", n)
	}
	if n, ok := pollSubscriberCount(mr, "pad:events:ws-1", func(n int) bool { return n == 0 }); !ok {
		t.Errorf("miniredis still reports %d subscriber(s) on the channel; the connection or its receive loop was left behind", n)
	}
}

// TestCancelDuringConfirmationTearsDownWhenNobodyElseIsWaiting is the
// post-install half with no joiner. The subscription IS installed by the time
// the context dies — the install deliberately precedes the wait — so the
// cancellation path has to reach it through the ordinary count-to-zero
// teardown rather than by abandoning.
//
// Fails before the fix: the wait runs to its bound, the caller is handed a
// live subscription, and both slots stay held for the whole of it.
func TestCancelDuringConfirmationTearsDownWhenNobodyElseIsWaiting(t *testing.T) {
	mr := miniredis.RunT(t)
	// The SUBSCRIBE is parked in flight, so the acknowledgement cannot arrive
	// and the wait is genuinely running when the cancellation lands. Parking
	// the command rather than delaying the client's write is what makes this
	// the production shape — see subscribeDelayProxy.
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 2*time.Second)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled on the FAR side of the install, which is what separates this
	// test from the one above. See afterInstallSubscription.
	b.afterInstallSubscription = func(string) { cancel() }

	start := time.Now()
	ch, _, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0)
	elapsed := time.Since(start)

	if outcome != SubscribeCancelled {
		t.Fatalf("outcome = %v, want SubscribeCancelled", outcome)
	}
	if ch != nil {
		t.Error("a cancelled subscribe handed back a channel")
	}
	// The point of the unit: the caller stops paying for a wait it will never
	// use. The bound is the bus's own confirmTimeout (1s by default), so
	// anything close to it means the cancellation was not observed.
	if elapsed >= b.confirmTimeout {
		t.Errorf("subscribe took %v, which is at or past the %v confirmation bound — the slot was held for the whole wait", elapsed, b.confirmTimeout)
	}
	if n := b.WorkspaceSubscriberCount("ws-1"); n != 0 {
		t.Errorf("workspace subscriber count = %d, want 0", n)
	}
	if b.hasWorkspaceSub("ws-1") {
		t.Error("the subscription outlived its only subscriber")
	}
	// PREMISE, POLLED RATHER THAN READ ONCE. The proxy parks the command on
	// its own goroutine, and the client's write returns as soon as the bytes
	// are gone — that asynchrony is the whole design of this instrument — so a
	// single read here can run before the park is recorded and fail a test
	// whose subject behaved correctly.
	deadline := time.Now().Add(3 * time.Second)
	for proxy.held.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the proxy never parked a SUBSCRIBE, so this test never entered the window it is named for")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCancelledEstablisherStillFinishesTheWaitForItsJoiners is the joint
// between this unit's design and BUG-2747's, which is where a third bug would
// live.
//
// A joiner waits on the establisher's record. If a cancelled establisher
// simply closed that record and returned, every joiner would be released into
// a subscription Redis has not acknowledged and told NOTHING about it — which
// is precisely the silent under-delivery BUG-2747 exists to close, re-created
// for the joiner population. So the cancelled establisher hands the REMAINDER
// of the wait to a goroutine, and the joiner's honesty signal must survive its
// departure.
//
// ASSERTS ITS OWN PREMISE: the joiner is confirmed to have been WAITING (the
// workspace count reaches two while the establishment is still in flight)
// before the cancellation is fired. Without that, a joiner that arrived after
// everything settled would pass this test while proving nothing.
func TestCancelledEstablisherStillFinishesTheWaitForItsJoiners(t *testing.T) {
	mr := miniredis.RunT(t)
	// Parked until the hand-off has run into the confirmation bound and marked
	// the admission unconfirmed — the state that later becomes the joiner's
	// sync_required. Held at the wire and released only once the observer
	// has reported that mark (below), rather than for a fixed interval, so
	// the timer arm is the only one that can win and the acknowledgement is
	// late by construction (BUG-2786).
	proxy := newSubscribeGateProxy(t, mr.Addr())
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	b.confirmTimeout = 200 * time.Millisecond
	obs := &recordingObserver{}
	b.SetObserver(obs)

	unconfirmed := make(chan struct{}, 1)
	b.beforeUnconfirmedMark = func() {
		select {
		case unconfirmed <- struct{}{}:
		default:
		}
	}

	type joinResult struct {
		ch      chan Event
		gaps    <-chan struct{}
		outcome SubscribeOutcome
	}
	joined := make(chan joinResult, 1)

	ctx, cancel := context.WithCancel(context.Background())

	b.beforeInstallSubscription = func(string) {
		// Launched here so it finds the establishment record already in the
		// map and genuinely waits on it.
		go func() {
			ch, gaps, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
			joined <- joinResult{ch, gaps, outcome}
		}()
	}
	b.afterInstallSubscription = func(string) {
		// Premise check: do not cancel until the joiner is registered and
		// waiting, or this test proves nothing about joiners.
		deadline := time.Now().Add(3 * time.Second)
		for b.WorkspaceSubscriberCount("ws-1") < 2 {
			if time.Now().After(deadline) {
				t.Error("the joiner never registered; this test never exercised the joiner path")
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}

	_, _, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0)
	if outcome != SubscribeCancelled {
		t.Fatalf("establisher outcome = %v, want SubscribeCancelled", outcome)
	}

	var jr joinResult
	select {
	case jr = <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner never returned — a cancelled establisher stranded it on a promise nobody kept")
	}

	if jr.outcome != SubscribeOK {
		t.Fatalf("joiner outcome = %v, want SubscribeOK — the joiner did not leave, and must still be served", jr.outcome)
	}
	if jr.ch == nil || jr.gaps == nil {
		t.Fatal("the joiner was released with no channel behind it")
	}
	defer b.Unsubscribe(jr.ch)

	// The hand-off's whole purpose: the wait completed on the joiner's behalf,
	// so the admission was MARKED unconfirmed rather than passing silently.
	select {
	case <-unconfirmed:
	case <-time.After(5 * time.Second):
		t.Fatal("the confirmation wait was abandoned with the establisher: the joiner was admitted into an unacknowledged subscription and told nothing (BUG-2747's defect, re-created at the seam)")
	}
	// The hook fires BEFORE the mark takes b.mu; the observer reports AFTER
	// it is set. Release the acknowledgement only on the second signal, or it
	// can beat the mark and turn it into a no-op (codex round 3 P1).
	deadline := time.Now().Add(5 * time.Second)
	for obs.unconfirmedCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the hook ran but the unconfirmed mark was never reported; this test could not have discriminated")
		}
		time.Sleep(time.Millisecond)
	}
	proxy.waitParked(t)
	if proxy.forcedOpen.Load() {
		t.Fatal("the failsafe opened the gate before the test released it; the acknowledgement was not held by construction, so this test could not have discriminated")
	}
	proxy.release()

	// And it reaches the joiner as a reconcile signal once the acknowledgement
	// finally lands.
	select {
	case <-jr.gaps:
	case <-time.After(10 * time.Second):
		t.Error("the joiner was never told to reconcile after the late acknowledgement")
	}
}

// TestCancelledJoinerLeavesTheEstablisherAlone is the mirror case: the caller
// that leaves is a WAITER, not the establisher. It must stop being counted
// without disturbing an establishment that is still someone else's.
func TestCancelledJoinerLeavesTheEstablisherAlone(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 300*time.Millisecond)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joined := make(chan SubscribeOutcome, 1)

	b.beforeInstallSubscription = func(string) {
		go func() {
			_, _, outcome := b.SubscribeIfAllowed(joinerCtx, "ws-1", 0)
			joined <- outcome
		}()
		// Wait for the joiner to be registered and waiting, then send it away.
		deadline := time.Now().Add(3 * time.Second)
		for b.WorkspaceSubscriberCount("ws-1") < 2 {
			if time.Now().After(deadline) {
				t.Error("the joiner never registered; this test never exercised the joiner path")
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancelJoiner()
	}

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
		t.Fatalf("establisher outcome = %v, want SubscribeOK — a joiner leaving must not refuse the establisher", outcome)
	}
	defer b.Unsubscribe(ch)

	select {
	case got := <-joined:
		if got != SubscribeCancelled {
			t.Errorf("joiner outcome = %v, want SubscribeCancelled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled joiner never returned")
	}

	if n := b.WorkspaceSubscriberCount("ws-1"); n != 1 {
		t.Errorf("workspace subscriber count = %d, want 1 (the establisher only) — the departed joiner's slot is still held", n)
	}
	if !b.hasWorkspaceSub("ws-1") {
		t.Error("the joiner's departure tore down the establisher's subscription")
	}
}

// TestAnAlreadyCancelledCallerRegistersNothing covers the cheap entry case on
// both implementations, so the handler's cancellation branch is reachable
// without Redis.
//
// THE END STATE ALONE CANNOT SEE THIS GUARD (CONVE-12). Remove the entry check
// and a dead caller still ends up with SubscribeCancelled and a zero count —
// it just registers, establishes a whole Redis subscription, and then undoes
// all of it on the way out. So the Redis leg asserts what the missing guard
// would DO rather than what it leaves behind: an establishment attempt that
// should never have been started.
func TestAnAlreadyCancelledCallerRegistersNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("MemoryBus", func(t *testing.T) {
		b := New()
		defer b.Close()
		if _, _, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0); outcome != SubscribeCancelled {
			t.Fatalf("outcome = %v, want SubscribeCancelled", outcome)
		}
		if n := b.WorkspaceSubscriberCount("ws-1"); n != 0 {
			t.Errorf("workspace subscriber count = %d, want 0", n)
		}
	})

	t.Run("RedisBus", func(t *testing.T) {
		b, _ := newCancelTestBus(t)
		attempted := make(chan struct{}, 1)
		b.beforeInstallSubscription = func(string) {
			select {
			case attempted <- struct{}{}:
			default:
			}
		}

		if _, _, _, outcome := b.SubscribeAndReplaySince(ctx, "ws-1", 5, 0); outcome != SubscribeCancelled {
			t.Fatalf("outcome = %v, want SubscribeCancelled", outcome)
		}
		if n := b.WorkspaceSubscriberCount("ws-1"); n != 0 {
			t.Errorf("workspace subscriber count = %d, want 0", n)
		}
		select {
		case <-attempted:
			t.Error("a caller that was already gone still opened a Redis subscription; the entry guard is what stops the dial happening at all")
		default:
		}
	})
}

// TestTheDialUsesTheCallersContext is a WIRING assertion (CONVE-19): the
// timing benefit of dialling on the caller's context cannot be observed
// against a Redis that answers instantly, so this pins the BINDING instead of
// the duration.
//
// It matters because the binding is the whole of what the dial half of this
// fix does, and it is invisible to every other test here — they all cancel at
// seams that run after the dial has returned.
//
// WHAT IT DOES NOT CLAIM: that cancellation actually shortens a real dial.
// go-redis derives its per-attempt dial deadline from the context passed in,
// so it does — on plaintext through net.Dialer.DialContext, and since BUG-2754
// on TLS too, through the dialer Pad installs (internal/redisdial). Neither is
// tested here; the TLS half has its own tests in that package, against a server
// that accepts and then never completes a handshake.
func TestTheDialUsesTheCallersContext(t *testing.T) {
	mr := miniredis.RunT(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ASSERTED FROM INSIDE THE DIALER, because the context handed to the dial
	// is scoped to establishment and is released as soon as it returns —
	// inspecting it afterwards would find it Done no matter what it was
	// derived from, which is a check that cannot fail.
	var (
		observedLive   bool
		observedClosed bool
		dialled        = make(chan struct{}, 1)
	)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		Dialer: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			select {
			case dialled <- struct{}{}:
				// The control: live before the caller is cancelled. Without it
				// a dial handed an already-dead context would satisfy the
				// check below for the wrong reason.
				observedLive = dialCtx.Err() == nil
				cancel()
				observedClosed = dialCtx.Err() != nil
			default:
			}
			return (&net.Dialer{}).DialContext(context.Background(), "tcp", mr.Addr())
		},
	})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ch, _, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0)
	if ch != nil {
		defer b.Unsubscribe(ch)
	}
	_ = outcome

	select {
	case <-dialled:
	default:
		t.Fatal("no dial was observed; this test never exercised the path it is named for")
	}
	if !observedLive {
		t.Fatal("the dial context was already done before the caller was cancelled")
	}
	if !observedClosed {
		t.Error("cancelling the caller did not end the dial's context: the dial is not derived from the caller, so a client leaving mid-dial goes on paying for it")
	}
}

// TestACancelledEstablisherNeverStrandsTheNextSubscriber is the regression test
// for the sharpest failure this unit can produce, and the one its filing named
// in advance: a caller that owns a workspace's establishment record and returns
// without putting it down.
//
// The record stays in pendingSubs with nobody behind it. The next subscriber
// for that workspace joins it and waits on a done channel nothing will ever
// close — permanently, because its own registration keeps wsCounts non-zero, so
// no later caller becomes an establisher either. A silently dead stream that
// looks alive.
//
// THE CANCELLATION IS PLACED IN THE OWNING WINDOW, between the record's
// creation and the start of establishment (see afterRegisterBeforeEstablish).
// Cancelling anywhere else exercises a path that unwinds correctly anyway, and
// would pass against the defect.
//
// Fails against the draft that broke out of the establish loop on a cancelled
// context: the second subscriber below never returns.
func TestACancelledEstablisherNeverStrandsTheNextSubscriber(t *testing.T) {
	b, _ := newCancelTestBus(t)

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	b.afterRegisterBeforeEstablish = func(string) {
		// Only the first caller — the one that owns the record.
		once.Do(cancel)
	}

	if _, _, outcome := b.SubscribeIfAllowed(ctx, "ws-1", 0); outcome != SubscribeCancelled {
		t.Fatalf("establisher outcome = %v, want SubscribeCancelled", outcome)
	}

	// Premise: the establisher really did own a record to put down. Without
	// this the test would pass against a build where the first caller never
	// became the establisher at all, proving nothing about strands.
	if n := b.pendingCount(); n != 0 {
		t.Fatalf("pendingSubs = %d after a cancelled establisher, want 0 — the record was left with nobody behind it", n)
	}
	if n := b.WorkspaceSubscriberCount("ws-1"); n != 0 {
		t.Fatalf("workspace subscriber count = %d, want 0", n)
	}

	// The consequence, asserted from the next subscriber's side rather than
	// from the map: a strand is only a bug because of what it does to them.
	done := make(chan SubscribeOutcome, 1)
	go func() {
		ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
		if ch != nil {
			b.Unsubscribe(ch)
		}
		done <- outcome
	}()

	select {
	case outcome := <-done:
		if outcome != SubscribeOK {
			t.Errorf("next subscriber outcome = %v, want SubscribeOK", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next subscriber never returned: it is waiting on an establishment record the cancelled establisher abandoned without retiring")
	}
}

// TestClosingTheBusStillEndsAnInFlightDial pins the half of the dial's context
// that BUG-2749 nearly removed (codex round 2 P2).
//
// Before this unit the dial ran on the bus's own context, so Close() could cut
// a stalled one short. Routing it to the caller instead would have handed that
// power away: a caller who stays connected across a shutdown would leave the
// dial running to DialTimeout with nothing able to interrupt it. The dial waits
// on both, and this asserts the bus half specifically — the caller's context
// here is never cancelled.
func TestClosingTheBusStillEndsAnInFlightDial(t *testing.T) {
	mr := miniredis.RunT(t)

	var (
		b              *RedisBus
		observedLive   bool
		observedClosed bool
		dialled        = make(chan struct{}, 1)
	)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		Dialer: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			select {
			case dialled <- struct{}{}:
				observedLive = dialCtx.Err() == nil
				// The bus shuts down mid-dial. The CALLER's context is never
				// cancelled in this test, so only the bus half can end this.
				b.cancel()
				// The bus half arrives through context.AfterFunc, which runs
				// its callback on its own goroutine — so this is polled, not
				// read once.
				deadline := time.Now().Add(3 * time.Second)
				for {
					if dialCtx.Err() != nil {
						observedClosed = true
						break
					}
					if time.Now().After(deadline) {
						break
					}
					time.Sleep(time.Millisecond)
				}
			default:
			}
			return (&net.Dialer{}).DialContext(context.Background(), "tcp", mr.Addr())
		},
	})
	t.Cleanup(func() { _ = client.Close() })

	b = NewRedisBus(client)
	t.Cleanup(b.Close)

	ch, _, _ := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if ch != nil {
		b.Unsubscribe(ch)
	}

	select {
	case <-dialled:
	default:
		t.Fatal("no dial was observed; this test never exercised the path it is named for")
	}
	if !observedLive {
		t.Fatal("the dial context was already done before the bus was closed")
	}
	if !observedClosed {
		t.Error("shutting the bus down did not end the dial's context: a shutdown can no longer interrupt a stalled dial, which it could before this unit")
	}
}
