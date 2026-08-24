package events

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// subscribeDelayProxy sits between the bus and Redis and holds the SUBSCRIBE
// command IN TRANSIT, widening the window BUG-2747 lives in from microseconds
// to something a test can be deterministic about.
//
// IT MUST DELAY THE COMMAND IN FLIGHT, NOT THE CLIENT'S WRITE, and the
// difference is what makes this instrument discriminate at all. go-redis writes
// SUBSCRIBE synchronously inside client.Subscribe, so a delay applied to the
// write is paid by the unfixed code too — Subscribe would return late there as
// well, the publish would land after registration, and the test would pass
// against the very defect it exists to catch. Parking the bytes in the proxy
// lets the client's write return at once, which is exactly the production
// shape: the command is gone, and Redis has not seen it yet.
//
// Publishes are untouched — they travel on the pooled connection and carry no
// "subscribe" token — so a publish issued in the window reaches Redis FIRST,
// which is the whole defect.
type subscribeDelayProxy struct {
	ln    net.Listener
	delay time.Duration
	// held counts SUBSCRIBE commands actually parked, so a test can assert its
	// own premise rather than passing because the instrument never armed.
	held atomic.Int64
}

func newSubscribeDelayProxy(t *testing.T, backend string, delay time.Duration) *subscribeDelayProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &subscribeDelayProxy{ln: ln, delay: delay}
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
			go p.forward(client, server)
			go func() { _, _ = io.Copy(client, server) }()
		}
	}()
	return p
}

func (p *subscribeDelayProxy) addr() string { return p.ln.Addr().String() }

// forward copies client → server, parking the first chunk that carries a
// SUBSCRIBE.
func (p *subscribeDelayProxy) forward(client, server net.Conn) {
	defer func() { _ = server.Close() }()
	buf := make([]byte, 4096)
	parked := false
	for {
		n, err := client.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !parked && bytes.Contains(bytes.ToLower(chunk), []byte("\r\n$9\r\nsubscribe\r\n")) {
				parked = true
				p.held.Add(1)
				time.Sleep(p.delay)
			}
			if _, werr := server.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestSubscribeDoesNotReturnBeforeRedisHasRegisteredIt is BUG-2747's
// regression test.
//
// The bus has NO LOCAL FAN-OUT — every event, including one this very process
// publishes, goes out to Redis and comes back through the subscription — so a
// publish that reaches Redis before the SUBSCRIBE does reaches nobody at all.
// A fresh subscriber (sinceID == 0) is the population that cannot detect it:
// its buffer's coverage starts at the first event that DOES arrive, so the
// hole is below everything the buffer ever claimed.
//
// Fails without the fix: Subscribe returns while the SUBSCRIBE is still in
// flight, and the publish below is swallowed.
func TestSubscribeDoesNotReturnBeforeRedisHasRegisteredIt(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 300*time.Millisecond)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("SubscribeIfAllowed refused a connection with no limit set")
	}
	defer b.Unsubscribe(ch)

	// Published the instant Subscribe returns. The claim under test is that
	// this instant is already past registration.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("an event published after Subscribe returned was lost: the subscription was not yet registered with Redis")
	}

	// PREMISE, asserted last on purpose. Checked BEFORE the publish it would
	// race the unfixed code, which returns from Subscribe while the command is
	// still on its way into the proxy — so the test would fail complaining
	// about its own instrument instead of reporting the defect. Here it can
	// only fire when delivery SUCCEEDED without the window ever being widened,
	// which is the vacuous pass it exists to prevent.
	if held := proxy.held.Load(); held == 0 {
		t.Fatal("the SUBSCRIBE was never delayed; this test could not have discriminated")
	}
}

// TestConcurrentFirstSubscribersShareOneRedisSubscription pins the wall the
// confirmation wait opens: two callers arriving for the same workspace with no
// live subscription must not each establish one.
//
// Two subscriptions would mean two connections, two receive loops and every
// event fanned out TWICE — and the second loop's first acknowledgement would
// read as a resubscription, ending the workspace's coverage for no reason.
func TestConcurrentFirstSubscribersShareOneRedisSubscription(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 200*time.Millisecond)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	const callers = 4
	var wg sync.WaitGroup
	chans := make([]chan Event, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
			if !ok {
				t.Errorf("caller %d was refused", i)
				return
			}
			chans[i] = ch
		}()
	}
	wg.Wait()

	// One establishment, therefore one Redis subscriber for the channel.
	if n := mr.PubSubNumSub("pad:events:ws-1")["pad:events:ws-1"]; n != 1 {
		t.Fatalf("Redis reports %d subscribers on the channel, want 1 — concurrent first subscribers each opened their own", n)
	}
	b.mu.Lock()
	pending := len(b.pendingSubs)
	b.mu.Unlock()
	if pending != 0 {
		t.Fatalf("establishment record was not retired: %d pending", pending)
	}

	// And every caller is genuinely on the one subscription.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	for i, ch := range chans {
		if ch == nil {
			t.Fatalf("caller %d never got a channel", i)
		}
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("caller %d did not receive the event", i)
		}
		b.Unsubscribe(ch)
	}
}

// TestSubscribeDoesNotHoldTheBusLockAcrossTheRedisDial is BUG-2748's
// regression test.
//
// THE DIAL MUST HANG, NOT FAIL. A refused connection returns immediately and
// would pass against the unfixed code, which is why a closed port is the wrong
// instrument here.
func TestSubscribeDoesNotHoldTheBusLockAcrossTheRedisDial(t *testing.T) {
	mr := miniredis.RunT(t)

	release := make(chan struct{})
	var stalls atomic.Int64
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if stalls.Add(1) > 1 { // the first dial is the bus's own client setup
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: dial})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	b.confirmTimeout = 500 * time.Millisecond

	// A workspace with a healthy, already-established subscription, whose
	// stream operations are the thing that must not be held hostage.
	close(release)
	quiet, _ := b.Subscribe("ws-quiet")
	release = make(chan struct{})
	stalls.Store(1)

	stalled := make(chan struct{})
	go func() {
		ch, _ := b.Subscribe("ws-stalled")
		b.Unsubscribe(ch)
		close(stalled)
	}()

	// Give the stalled dial time to be underway. Before the fix it is holding
	// b.mu at this point and everything below blocks behind it.
	time.Sleep(150 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		b.Unsubscribe(quiet)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("an unrelated workspace's Unsubscribe blocked behind a stalled Redis dial: the dial is still inside b.mu")
	}

	close(release)
	select {
	case <-stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled subscribe never completed")
	}
}

// TestAnUnconfirmedAdmissionTellsItsSubscribersWhenTheAckLands covers the
// failure path: Redis does not acknowledge in time, callers are admitted
// anyway, and the span they sat through is reconciled to them rather than
// merely logged.
//
// Admitting rather than refusing is deliberate — before this fix EVERY
// subscriber was admitted into an unconfirmed subscription and told nothing —
// but an admitted stream whose coverage is unknown must not go on looking
// current, so the acknowledgement carries the mid-stream signal BUG-2730 built.
func TestAnUnconfirmedAdmissionTellsItsSubscribersWhenTheAckLands(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 400*time.Millisecond)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	// Shorter than the delay, so the wait gives up first.
	b.confirmTimeout = 50 * time.Millisecond

	ch, gaps, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("an unconfirmed subscription must ADMIT, not refuse")
	}
	defer b.Unsubscribe(ch)

	if got := obs.unconfirmedCount(); got != 1 {
		t.Fatalf("SubscriptionUnconfirmed reported %d times, want 1", got)
	}

	// The acknowledgement lands late; the subscriber is told to reconcile.
	select {
	case <-gaps:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber admitted before the acknowledgement was never told to reconcile when it arrived")
	}

	// Control: the same signal does not fire for a subscription that was
	// confirmed before admission.
	b.confirmTimeout = defaultSubscribeConfirmTimeout
	before := obs.unconfirmedCount()
	ch2, gaps2, _ := b.SubscribeIfAllowed("ws-2", 0)
	defer b.Unsubscribe(ch2)
	if got := obs.unconfirmedCount(); got != before {
		t.Fatalf("a confirmed subscription reported SubscriptionUnconfirmed (%d -> %d)", before, got)
	}
	select {
	case <-gaps2:
		t.Fatal("a confirmed subscription must not tell its subscriber to reconcile")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAJoinersReplayStopsWhereItsChannelStarts pins the replay ceiling.
//
// WHO ACTUALLY NEEDS IT, since the answer is not obvious and I got it wrong
// once: NOT the establishing caller. Establishing means the workspace had no
// live subscription, and losing the last subscriber deletes the buffer, so the
// establisher always registers with no coverage and its replay is nil — there
// is nothing there to duplicate. The exposed caller is a JOINER: one that
// arrives while somebody else's establishment is still in flight, after that
// establishment has been acknowledged and events have begun landing in a
// freshly created buffer.
//
// Such a joiner is registered in the fan-out map, so it receives everything
// published from that instant on its CHANNEL, and it then reads a replay. Both
// spans come out of the same buffer, and only the ceiling divides them. Without
// it the joiner is handed the post-registration events twice — the duplicate
// window BUG-2730 closed, reopened from the other end.
func TestAJoinersReplayStopsWhereItsChannelStarts(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	type joinResult struct {
		ch     chan Event
		missed []Event
	}
	joined := make(chan joinResult, 1)

	var armed atomic.Bool
	b.afterSubscriptionConfirmed = func() {
		if !armed.CompareAndSwap(false, true) {
			return
		}
		// E0 establishes the buffer's coverage floor, and is the position the
		// joiner resumes FROM. It has to exist: the buffer's knownFrom is set
		// by its first append, so without E0 there is no positive cursor below
		// the events the replay is supposed to carry, and the replay comes back
		// empty for a reason that has nothing to do with the ceiling.
		b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
		waitForBufferedCount(t, b, "ws-1", 1)
		cursor := b.EventsSince("ws-1", 0)[0].ID

		// E1 — published and buffered BEFORE the joiner registers, so it is
		// squarely in the joiner's replay span.
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		waitForBufferedCount(t, b, "ws-1", 2)

		// The joiner registers here. pendingSubs still holds this
		// establishment, so it waits rather than returning.
		go func() {
			ch, missed, _, ok := b.SubscribeAndReplaySince("ws-1", cursor, 0)
			if !ok {
				t.Error("the joiner was refused")
			}
			joined <- joinResult{ch: ch, missed: missed}
		}()
		waitForWorkspaceSubscribers(t, b, "ws-1", 2)

		// E2 — published AFTER the joiner registered, so it belongs to the
		// joiner's channel and must not appear in its replay.
		b.Publish(Event{Type: ItemArchived, WorkspaceID: "ws-1"})
		waitForBufferedCount(t, b, "ws-1", 3)
	}

	establisher, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(establisher)
	if !armed.Load() {
		t.Fatal("the seam never ran; this test could not have discriminated")
	}

	res := <-joined
	defer b.Unsubscribe(res.ch)

	// PREMISE: the replay must be non-empty, or the ceiling has nothing to cut
	// and a pass below proves nothing.
	if len(res.missed) == 0 {
		t.Fatal("the joiner's replay was empty; this test could not have discriminated")
	}

	replayed := map[int64]bool{}
	for _, e := range res.missed {
		replayed[e.ID] = true
	}

	select {
	case e := <-res.ch:
		if replayed[e.ID] {
			t.Fatalf("event %d was in the joiner's replay AND on its channel", e.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the joiner never received the event published after it registered")
	}
}

// waitForBufferedCount blocks until the workspace's replay buffer holds n
// events, so a test can sequence publishes rather than race them.
func waitForBufferedCount(t *testing.T, b *RedisBus, workspaceID string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.EventsSince(workspaceID, 0)) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s buffered fewer than %d events within the deadline", workspaceID, n)
}

// waitForWorkspaceSubscribers blocks until n local subscribers are registered
// for the workspace — registration, not admission, which is the moment the
// ceiling is captured.
func waitForWorkspaceSubscribers(t *testing.T, b *RedisBus, workspaceID string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.WorkspaceSubscriberCount(workspaceID) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s has %d local subscribers, want %d", workspaceID, b.WorkspaceSubscriberCount(workspaceID), n)
}

// TestAFreshSubscriberReceivesAnEventPublishedDuringEstablishment is the other
// half of the ceiling's job, and the one an earlier design got wrong.
//
// A fresh subscriber (sinceID == 0) reads NO REPLAY — SubscribeIfAllowed does
// not even return one. So any mechanism that withholds in-gap events from its
// channel on the theory that the replay will deliver them drops those events
// outright, for exactly the population BUG-2747 is about. The first version of
// this fix did that; codex round 1 found it.
func TestAFreshSubscriberReceivesAnEventPublishedDuringEstablishment(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	var armed atomic.Bool
	buffered := make(chan struct{})
	b.afterSubscriptionConfirmed = func() {
		if !armed.CompareAndSwap(false, true) {
			return
		}
		b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
		// Wait until it has been applied, so the subscriber below is genuinely
		// returning AFTER the in-gap event rather than racing it.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if got := b.EventsSince("ws-1", 0); len(got) > 0 {
				close(buffered)
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("the in-gap publish never reached the replay buffer")
		close(buffered)
	}

	ch, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("SubscribeIfAllowed refused")
	}
	defer b.Unsubscribe(ch)
	<-buffered

	if !armed.Load() {
		t.Fatal("the seam never ran; this test could not have discriminated")
	}

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("a fresh subscriber never received an event published while its subscription was being established: it was withheld from the channel and there is no replay to deliver it")
	}
}

// TestASubscriberArrivingMidEstablishmentWaitsForTheAcknowledgement covers the
// overlap between pendingSubs and wsSubs.
//
// establishSubscription installs wsSubs BEFORE it waits for the
// acknowledgement, so for that interval the workspace has a live-looking
// subscription Redis has not confirmed. A second subscriber that reads wsSubs
// first is admitted straight into the unconfirmed window — the defect this
// whole change exists to close, reached through the fix's own seam. Found by
// codex round 1.
func TestASubscriberArrivingMidEstablishmentWaitsForTheAcknowledgement(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeDelayProxy(t, mr.Addr(), 300*time.Millisecond)
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	// The establishing caller, left in flight.
	first := make(chan chan Event, 1)
	go func() {
		ch, _ := b.Subscribe("ws-1")
		first <- ch
	}()

	// Wait until wsSubs is installed but the acknowledgement has not landed —
	// the overlap itself.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		sub, live := b.wsSubs["ws-1"]
		confirmed := live && sub.confirmClosed
		b.mu.Unlock()
		if live && !confirmed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never observed the install-before-acknowledgement overlap; this test could not have discriminated")
		}
		time.Sleep(time.Millisecond)
	}

	second, _, ok := b.SubscribeIfAllowed("ws-1", 0)
	if !ok {
		t.Fatal("the second subscriber was refused")
	}
	defer b.Unsubscribe(second)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("an event published after the second subscriber was admitted was lost: it was let in before Redis acknowledged the subscription")
	}
	b.Unsubscribe(<-first)
}

// TestAConfirmedSubscriptionIsNeverLeftMarkedUnconfirmed pins codex round 1's
// P2 — the interleave where establishSubscription's select chose the timer but
// the acknowledgement had already landed and confirmSubscription had already
// cleared the flag. Without a re-check under the lock that closes it, the flag
// is set again with nothing left to come and clear it: the subscriber is
// counted as unconfirmed and is never told to reconcile, because the
// acknowledgement it would have ridden on has been and gone.
//
// DRIVEN BY A SEAM, NOT BY REPETITION, and the numbers are why. The race needs
// the timer and the acknowledgement ready together; a near-zero bound makes the
// timer win OUTRIGHT almost every time instead, which is the ordinary timeout
// path and proves nothing. Measured against the mutation: 500 establishments per
// run caught it in 0 of 10 runs. A one-in-ten detector reads as coverage and
// is not.
func TestAConfirmedSubscriptionIsNeverLeftMarkedUnconfirmed(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	// Expires immediately, so the timer branch is the one taken.
	b.confirmTimeout = time.Nanosecond

	// ...and then hold the mark until the acknowledgement has definitively
	// landed, which is the interleave under test.
	var raced atomic.Bool
	b.beforeUnconfirmedMark = func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			b.mu.Lock()
			sub, live := b.wsSubs["ws-1"]
			done := live && sub.confirmClosed
			b.mu.Unlock()
			if done {
				raced.Store(true)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}

	ch, gaps := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)

	// PREMISE: the acknowledgement really did beat the mark, or the interleave
	// under test never happened.
	if !raced.Load() {
		t.Fatal("the acknowledgement never landed before the mark; this test could not have discriminated")
	}

	b.mu.Lock()
	sub, live := b.wsSubs["ws-1"]
	strandedFlag := live && sub.unconfirmedAdmitted
	b.mu.Unlock()
	if strandedFlag {
		t.Fatal("a subscription whose acknowledgement had already landed was left marked unconfirmed: nothing remains to clear it or to tell the subscriber")
	}

	// The observable consequence of the stranded flag, stated as behaviour
	// rather than as internal state: the subscriber is reported unconfirmed and
	// then never signalled.
	if obs.unconfirmedCount() > 0 {
		select {
		case <-gaps:
		case <-time.After(2 * time.Second):
			t.Fatal("reported an unconfirmed admission but never told the subscriber to reconcile")
		}
	}
}
