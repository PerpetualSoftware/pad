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

	// gate, when non-nil, replaces delay: a parked SUBSCRIBE is released when
	// the channel is closed rather than after a fixed sleep (see
	// newSubscribeGateProxy). forcedOpen records that the failsafe opened it
	// instead of the test, so the test can name that as its own distinct
	// failure rather than reading it as a pass.
	gate        chan struct{}
	releaseOnce sync.Once
	forcedOpen  atomic.Bool
}

func newSubscribeDelayProxy(t *testing.T, backend string, delay time.Duration) *subscribeDelayProxy {
	t.Helper()
	return newSubscribeProxy(t, backend, delay, nil)
}

// newSubscribeProxy is the constructor behind both proxies. Every field the
// forward goroutine reads is set BEFORE the accept loop starts (codex round
// 5): a gate assigned after the goroutine is running is published to it
// without synchronisation, and a stale nil there takes the delay path with
// delay=0 — the gate silently not a gate.
func newSubscribeProxy(t *testing.T, backend string, delay time.Duration, gate chan struct{}) *subscribeDelayProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &subscribeDelayProxy{ln: ln, delay: delay, gate: gate}
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

// subscribeGateFailsafe bounds how long a gated SUBSCRIBE stays parked when
// nothing releases it. It exists so a test whose release never fires ends
// bounded instead of at the package timeout; it must NOT be short enough to
// open the gate on a slow but correct run, or it becomes a second way for
// the test to fail that looks like the first. 20s is four orders of
// magnitude past the loaded ack latency measured for BUG-2786 (sub-ms) and
// past every wait the gated tests contain. It is NOT self-reporting: a test
// that relies on the gate must check forcedOpen before it trusts the
// ordering the gate was holding (codex round 4), or a failsafe release
// shows up as an unrelated assertion failing. When it fires it opens the
// gate for every connection, so the test records ONE forced open rather
// than one per connection.
const subscribeGateFailsafe = 20 * time.Second

// newSubscribeGateProxy is the delay proxy with the release under the TEST'S
// control: the first SUBSCRIBE per connection is parked until release() is
// called — or the failsafe fires, which forcedOpen records — not for a fixed
// interval.
//
// WHY A SIGNAL AND NOT A DURATION (BUG-2786). A test that needs the
// establisher to take the TIMER arm of its confirmation select cannot get
// there by making the bound tiny: a 1ns bound only guarantees the timer is
// ready, not that the acknowledgement is NOT. Under load the receive goroutine
// lands the ack before the establisher reaches its select, both arms are
// ready, Go picks uniformly at random, and half the time the timer arm — and
// everything the test hooked onto it — is skipped. The measured shape was 10
// of 1000 loaded runs, every one of them the hook never running. Holding the
// ack at the wire until the timer arm has been ENTERED makes the interleave a
// construction instead of a coin toss.
func newSubscribeGateProxy(t *testing.T, backend string) *subscribeDelayProxy {
	t.Helper()
	p := newSubscribeProxy(t, backend, 0, make(chan struct{}))
	// A test that ends without releasing leaves forward parked on the gate,
	// holding both sides of the connection until the failsafe (codex round
	// 3). Release at cleanup so teardown is prompt whichever way the test
	// went.
	t.Cleanup(p.release)
	return p
}

// release lets every parked and future SUBSCRIBE through. Idempotent and
// safe to call concurrently — the hook and the failsafe, or two connections'
// failsafes, can race to be first (codex round 2).
func (p *subscribeDelayProxy) release() {
	p.releaseOnce.Do(func() { close(p.gate) })
}

// waitParked blocks until the proxy has parked a SUBSCRIBE, and fails the
// test if none arrives. A one-shot read of held is NOT a barrier (codex
// round 4): the client's write returns once the bytes are in the kernel,
// and under load the proxy's forward goroutine may not have read them yet
// when the test looks — so a plain `held == 0` premise check fails
// spuriously, which is the family of flake this instrument exists to end.
//
// What it proves is exactly that the proxy recognised and parked one
// command; it does not say WHY none was, and the causes it cannot tell
// apart (the marker split across two reads, a dial that never reached the
// proxy) all end the same way for the test — no held acknowledgement, so no
// discrimination — which is what its message says.
func (p *subscribeDelayProxy) waitParked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for p.held.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the SUBSCRIBE was never parked; this test could not have discriminated")
		}
		time.Sleep(time.Millisecond)
	}
}

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
				if p.gate != nil {
					select {
					case <-p.gate:
					case <-time.After(subscribeGateFailsafe):
						// OPENS THE GATE FOR EVERYONE, not just this chunk
						// (codex round 1 P2): a later connection — a
						// reconnect, a second workspace — would otherwise
						// park for another failsafe interval, and forcedOpen
						// would then be reporting a cause that belongs to
						// the first connection alone.
						p.forcedOpen.Store(true)
						p.release()
					}
				} else {
					time.Sleep(p.delay)
				}
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

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
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
			ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
			if outcome != SubscribeOK {
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
		// EXACTLY ONCE, not merely at least once (codex round 7). Two
		// establishments mean two receive loops fanning the same event out
		// twice; reading one and moving on would leave the duplicate sitting
		// in the channel undetected, and the subscriber-count assertion above
		// is topology rather than the behaviour anyone cares about.
		select {
		case dup := <-ch:
			t.Fatalf("caller %d received a second copy of the event (%d): the workspace has more than one subscription", i, dup.ID)
		case <-time.After(250 * time.Millisecond):
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
	quiet, _, _ := b.Subscribe(context.Background(), "ws-quiet")
	release = make(chan struct{})
	stalls.Store(1)

	stalled := make(chan struct{})
	go func() {
		ch, _, _ := b.Subscribe(context.Background(), "ws-stalled")
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
	// GATED, NOT DELAYED (BUG-2786): the acknowledgement is held until the
	// wait has given up, so the timer arm is the only one that can win —
	// a fixed delay only made that likely, by a margin CI load can eat.
	proxy := newSubscribeGateProxy(t, mr.Addr())
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	b.confirmTimeout = 50 * time.Millisecond

	ch, gaps, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
		t.Fatal("an unconfirmed subscription must ADMIT, not refuse")
	}
	defer b.Unsubscribe(ch)

	proxy.waitParked(t)
	if proxy.forcedOpen.Load() {
		t.Fatal("the failsafe opened the gate before the test released it; the acknowledgement was not held by construction, so this test could not have discriminated")
	}
	if got := obs.unconfirmedCount(); got != 1 {
		t.Fatalf("SubscriptionUnconfirmed reported %d times, want 1", got)
	}

	// RELEASED ONLY NOW, after the mark has been observed above — not from
	// the beforeUnconfirmedMark hook, which runs before the mark takes b.mu
	// and would let the acknowledgement beat it (codex round 3 P1). With
	// Subscribe returned and the count at 1, the acknowledgement is late by
	// construction rather than by margin.
	proxy.release()

	// The acknowledgement lands late; the subscriber is told to reconcile.
	select {
	case <-gaps:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber admitted before the acknowledgement was never told to reconcile when it arrived")
	}

	// Control: the same signal does not fire for a subscription that was
	// confirmed before admission. The gate is open now, so ws-2 is an
	// ordinary immediate confirmation — which is the property the control
	// is about; the fixed-delay proxy this test used to run on made it a
	// slow one as a side effect, and nothing here depended on that.
	b.confirmTimeout = defaultSubscribeConfirmTimeout
	before := obs.unconfirmedCount()
	ch2, gaps2, _ := b.SubscribeIfAllowed(context.Background(), "ws-2", 0)
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
			ch, missed, _, outcome := b.SubscribeAndReplaySince(context.Background(), "ws-1", cursor, 0)
			if outcome != SubscribeOK {
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

	establisher, _, _ := b.Subscribe(context.Background(), "ws-1")
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

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
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
		ch, _, _ := b.Subscribe(context.Background(), "ws-1")
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

	second, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
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
//
// AND THE ACKNOWLEDGEMENT IS HELD AT THE WIRE UNTIL THE TIMER ARM IS TAKEN
// (BUG-2786), because the seam alone was not enough. The hook runs only on the
// timer arm, and a 1ns bound guarantees that arm is ready — not that the
// acknowledgement is not. Under CI load the receive goroutine consumed the
// SUBSCRIBE reply before the establisher reached its select, both arms were
// ready, Go chose at random, and on the confirmed arm the hook never ran: the
// guard below then reported the acknowledgement as never landing when it had
// landed too EARLY for the interleave to exist. Measured before the gate: 10
// of 1000 loaded runs (16 busy loops, GOMAXPROCS=2, -race), every one the hook
// never having run; zero were a stalled acknowledgement. With the reply parked
// until the hook releases it, only the timer arm can be ready at the select,
// and the guard has exactly one cause left.
func TestAConfirmedSubscriptionIsNeverLeftMarkedUnconfirmed(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newSubscribeGateProxy(t, mr.Addr())
	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	// Expires immediately, so the timer branch is the one taken — and, with
	// the acknowledgement parked at the proxy, the ONLY one that can be.
	b.confirmTimeout = time.Nanosecond

	// Now let the acknowledgement through, and hold the mark until it has
	// definitively landed: that is the interleave under test.
	var raced, hookRan atomic.Bool
	b.beforeUnconfirmedMark = func() {
		hookRan.Store(true)
		proxy.release()
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

	ch, gaps, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	// PREMISES, IN AN ORDER WHERE EACH GUARD'S CAUSE IS WHAT REMAINS (codex
	// round 7): the proxy parked the SUBSCRIBE at all; the failsafe did not
	// open it before the test released it; given both, the only way the hook
	// did not run is that the timer arm was not taken (this is the cause the
	// old guard misreported); and given all three, the only way raced is
	// false is that the released acknowledgement never landed within the
	// wait.
	proxy.waitParked(t)
	if proxy.forcedOpen.Load() {
		t.Fatal("the failsafe opened the gate before the test released it; the acknowledgement was not held by construction, so this test could not have discriminated")
	}
	if !hookRan.Load() {
		t.Fatal("the timer arm was not taken, so the hook never ran and the interleave under test was never constructed")
	}
	if !raced.Load() {
		t.Fatal("the acknowledgement was released before the mark but never landed within the wait; this test could not have discriminated")
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

// TestAJoinerIsNotStrandedByAnAbandonedEstablishment covers codex round 2's
// second P1.
//
// establishSubscription abandons when the workspace emptied while it was
// dialling. If the establishment record outlives that decision by even an
// instant, a subscriber arriving in the gap registers, waits on a promise
// nobody will keep, and returns holding a channel wired to nothing — and
// PERMANENTLY, because its own registration makes wsCounts non-zero so no
// later caller establishes either. A dead stream that looks alive.
//
// IT FABRICATES THE STATE ON PURPOSE, and that is worth stating rather than
// hiding (codex round 7). It empties the maps directly instead of calling
// Unsubscribe because the establishing caller is blocked inside Subscribe and
// its channel does not exist yet — nobody can unsubscribe it. That is the same
// reason the retry it exercises is DEFENCE IN DEPTH: with the record retired in
// the same critical section as the decision, a joiner cannot reach this state
// through the public surface at all. So this pins the recovery path, not a
// reachable defect, and it is the honest way to have any coverage of a path
// whose unreachability is an argument rather than a measurement.
func TestAJoinerIsNotStrandedByAnAbandonedEstablishment(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)

	// Force the abandon: drop the establisher's own registration out from under
	// it while it is dialling, so its count check finds an empty workspace.
	var abandoned atomic.Bool
	b.beforeInstallSubscription = func(workspaceID string) {
		if !abandoned.CompareAndSwap(false, true) {
			return
		}
		b.mu.Lock()
		for ch, ws := range b.workspaceOf {
			if ws == workspaceID {
				delete(b.workspaceOf, ch)
				delete(b.subscribers[workspaceID], ch)
				b.wsCounts[workspaceID]--
			}
		}
		b.mu.Unlock()
	}

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	if !abandoned.Load() {
		t.Fatal("the abandon was never forced; this test could not have discriminated")
	}
	defer b.Unsubscribe(ch)

	// Whatever happened to the first caller, a subscriber that ends up holding
	// a channel must be connected to Redis.
	next, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeOK {
		t.Fatal("the next subscriber was refused")
	}
	defer b.Unsubscribe(next)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	select {
	case <-next:
	case <-time.After(3 * time.Second):
		t.Fatal("a subscriber returned with a channel wired to nothing: it waited on an establishment that had already abandoned, and nothing re-established")
	}
}

// TestCloseDuringEstablishmentDoesNotLeakTheSubscription covers codex round 2's
// first P1.
//
// Close cancels the context BEFORE it takes the lock and drains wsSubs, so an
// establishment that locks afterwards used to install into a map Close had
// already emptied. The receive loop then exits on the cancelled context and
// nothing ever runs subCancel or pubsub.Close — the PubSub and its health-check
// goroutine outlive the bus.
//
// Asserted through Redis rather than through goroutine counting: a leaked
// PubSub is a connection still subscribed to the channel after the bus is gone.
func TestCloseDuringEstablishmentDoesNotLeakTheSubscription(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)

	var closedMidFlight atomic.Bool
	b.beforeInstallSubscription = func(string) {
		if !closedMidFlight.CompareAndSwap(false, true) {
			return
		}
		b.Close()
	}

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	_ = ch
	if !closedMidFlight.Load() {
		t.Fatal("Close never ran mid-establishment; this test could not have discriminated")
	}

	// Nothing may still be listening on the workspace's channel.
	if n, ok := pollSubscriberCount(mr, "pad:events:ws-1", func(n int) bool { return n == 0 }); !ok {
		t.Fatalf("after Close, %d connection(s) remain subscribed to the workspace channel: an establishment in flight installed into a closed bus and nothing tore it down", n)
	}
}

// TestTheReplayCeilingIsAPositionNotAnID covers codex round 3's first P1
// directly on the buffer, where the out-of-order case can be constructed
// exactly rather than hoped for.
//
// This bus's IDs come from a counter shared across workspaces, and a phase-1
// publish assigns and publishes in two calls, so arrival order and numeric
// order genuinely disagree. Against an ID-valued ceiling both directions break
// at once: a pre-registration event carrying a HIGHER id is filtered out and
// never replayed, and a straggler arriving AFTER registration with a lower id
// is replayed even though it also went to the caller's channel.
//
// WHAT THIS TEST DOES NOT PROVE, stated because an earlier version of this
// comment claimed it did: it appends straight to the buffer, so no subscriber
// and no fan-out channel exist here and it says nothing about the
// replay-XOR-channel property. That is
// TestAJoinersReplayStopsWhereItsChannelStarts. What this one pins is the
// boundary arithmetic, in cases a live test cannot construct on demand.
func TestTheReplayCeilingIsAPositionNotAnID(t *testing.T) {
	b := &RedisBus{
		subscribers:   map[string]map[chan Event]*subscriber{},
		workspaceOf:   map[chan Event]string{},
		wsCounts:      map[string]int{},
		wsSubs:        map[string]*redisSub{},
		pendingSubs:   map[string]*pendingSub{},
		replayBuffers: map[string]*replayBuffer{},
		replaySize:    DefaultReplayBufferSize,
	}
	rb := newReplayBuffer(b.replaySize)
	b.replayBuffers["ws-1"] = rb

	// Pre-registration, appended out of numeric order — 10, then 30, then 20.
	rb.append(Event{ID: 10, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 30, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 20, WorkspaceID: "ws-1"})

	// The caller registers HERE. lastAppendedID is 20, which is below one of
	// the events it has never seen.
	mark := registrationMark{buffer: rb, appends: rb.appends}

	// A straggler arrives after registration, carrying a LOWER id than the
	// ceiling an ID-valued bound would have used.
	rb.append(Event{ID: 15, WorkspaceID: "ws-1"})

	b.mu.Lock()
	missed := b.eventsSinceMarkLocked("ws-1", 10, mark)
	b.mu.Unlock()

	got := map[int64]int{}
	for _, e := range missed {
		got[e.ID]++
	}
	if got[30] != 1 {
		t.Errorf("event 30 was appended before the caller registered and must be replayed exactly once, got %d", got[30])
	}
	if got[20] != 1 {
		t.Errorf("event 20 was appended before the caller registered and must be replayed exactly once, got %d", got[20])
	}
	if got[15] != 0 {
		t.Errorf("event 15 arrived after the caller registered, so it went to its channel and must not also be replayed, got %d", got[15])
	}
}

// TestAPostRegistrationStragglerBelowTheCursorDoesNotEatAnEarlierEvent is codex
// round 5's P1, and it is the same two-spaces mistake the ID-valued ceiling made,
// committed again inside the fix for it.
//
// Bounding by counting entries off the END of what since() returned mixes two
// spaces: since() has already dropped events at or below the cursor, so a
// post-registration straggler with a low id is missing from that slice while
// still being counted in the number to drop. The count then eats a legitimate
// pre-registration event instead. The bound has to be applied to the BUFFER,
// before the cursor filter, which is what sinceBounded does.
func TestAPostRegistrationStragglerBelowTheCursorDoesNotEatAnEarlierEvent(t *testing.T) {
	b := &RedisBus{
		subscribers:   map[string]map[chan Event]*subscriber{},
		workspaceOf:   map[chan Event]string{},
		wsCounts:      map[string]int{},
		wsSubs:        map[string]*redisSub{},
		pendingSubs:   map[string]*pendingSub{},
		replayBuffers: map[string]*replayBuffer{},
		replaySize:    DefaultReplayBufferSize,
	}
	rb := newReplayBuffer(b.replaySize)
	b.replayBuffers["ws-1"] = rb

	// Pre-registration.
	rb.append(Event{ID: 5, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 30, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 20, WorkspaceID: "ws-1"})
	mark := registrationMark{buffer: rb, appends: rb.appends}

	// Post-registration: one BELOW the cursor, one above.
	rb.append(Event{ID: 6, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 40, WorkspaceID: "ws-1"})

	b.mu.Lock()
	missed := b.eventsSinceMarkLocked("ws-1", 10, mark)
	b.mu.Unlock()

	got := map[int64]int{}
	for _, e := range missed {
		got[e.ID]++
	}
	if got[30] != 1 || got[20] != 1 {
		t.Errorf("both pre-registration events above the cursor must be replayed exactly once, got 30:%d 20:%d", got[30], got[20])
	}
	if got[40] != 0 || got[6] != 0 {
		t.Errorf("post-registration events went to the caller's channel and must not be replayed, got 40:%d 6:%d", got[40], got[6])
	}
}

// TestAnEvictedRegistrationSpanIsRefusedRatherThanPartiallyServed covers the
// residual an earlier round accepted and this bound closes.
//
// If enough events arrive during the wait to evict everything the buffer held
// at registration, the caller's span is gone. Serving what is left would be a
// partial replay presented as a complete one; refusing sends sync_required,
// which is true.
func TestAnEvictedRegistrationSpanIsRefusedRatherThanPartiallyServed(t *testing.T) {
	b := &RedisBus{
		subscribers:   map[string]map[chan Event]*subscriber{},
		workspaceOf:   map[chan Event]string{},
		wsCounts:      map[string]int{},
		wsSubs:        map[string]*redisSub{},
		pendingSubs:   map[string]*pendingSub{},
		replayBuffers: map[string]*replayBuffer{},
		replaySize:    4,
	}
	rb := newReplayBuffer(b.replaySize)
	b.replayBuffers["ws-1"] = rb

	rb.append(Event{ID: 1, WorkspaceID: "ws-1"})
	rb.append(Event{ID: 2, WorkspaceID: "ws-1"})
	mark := registrationMark{buffer: rb, appends: rb.appends}

	// Control: before eviction, the span is served.
	b.mu.Lock()
	served := b.eventsSinceMarkLocked("ws-1", 1, mark)
	b.mu.Unlock()
	if len(served) != 1 || served[0].ID != 2 {
		t.Fatalf("the pre-registration span must be served while it is still held, got %v", served)
	}

	// Five more appends into a 4-deep ring evicts everything held at
	// registration.
	for id := int64(3); id <= 7; id++ {
		rb.append(Event{ID: id, WorkspaceID: "ws-1"})
	}
	b.mu.Lock()
	after := b.eventsSinceMarkLocked("ws-1", 1, mark)
	b.mu.Unlock()
	if after != nil {
		t.Fatalf("a registration span that has been evicted cannot be vouched for, got %d events", len(after))
	}
}

// TestAReplacedBufferVoidsTheCallersCoverageClaim covers codex round 3's second
// P1.
//
// An ID-space reset during the wait replaces the buffer wholesale. A position
// in the old buffer describes nothing in the new one, and knownFrom may still
// accept an adjacent cursor, so the mismatch does not announce itself — the
// caller would be handed new-space events as though they preceded its
// registration.
func TestAReplacedBufferVoidsTheCallersCoverageClaim(t *testing.T) {
	b := &RedisBus{
		subscribers:   map[string]map[chan Event]*subscriber{},
		workspaceOf:   map[chan Event]string{},
		wsCounts:      map[string]int{},
		wsSubs:        map[string]*redisSub{},
		pendingSubs:   map[string]*pendingSub{},
		replayBuffers: map[string]*replayBuffer{},
		replaySize:    DefaultReplayBufferSize,
	}
	old := newReplayBuffer(b.replaySize)
	b.replayBuffers["ws-1"] = old
	old.append(Event{ID: 10, WorkspaceID: "ws-1"})
	mark := registrationMark{buffer: old, appends: old.appends}

	// Control: the same buffer still answers.
	b.mu.Lock()
	same := b.eventsSinceMarkLocked("ws-1", 9, mark)
	b.mu.Unlock()
	if same == nil {
		t.Fatal("the buffer captured at registration must still answer its own caller")
	}

	// The reset: a new buffer, whose first event's id happens to sit adjacent
	// to the caller's cursor so knownFrom would accept it.
	fresh := newReplayBuffer(b.replaySize)
	b.replayBuffers["ws-1"] = fresh
	fresh.append(Event{ID: 10, WorkspaceID: "ws-1"})

	b.mu.Lock()
	after := b.eventsSinceMarkLocked("ws-1", 9, mark)
	b.mu.Unlock()
	if after != nil {
		t.Fatalf("a buffer replaced since registration cannot vouch for the caller's span, got %d events", len(after))
	}
}
