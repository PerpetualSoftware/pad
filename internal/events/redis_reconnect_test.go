package events

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
// severed on demand. It exists because a pub/sub RESUBSCRIPTION is the thing
// under test, and nothing short of a real dropped connection produces one:
// go-redis reconnects and re-subscribes internally, which is exactly the
// silence BUG-2731's receive loop was changed to break.
//
// Testing the decision logic alone — "if we have seen a subscribe before, drop
// coverage" — would leave the load-bearing half unproven, namely that the loop
// uses Receive (which surfaces subscription confirmations) rather than Channel
// (which swallows them). CONVE-19: wiring is a claim.
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

// codex round 6 O1. A Redis failover drops whatever was published while the
// subscription was down. Before this, PubSub.Channel resubscribed silently and
// the replay buffer carried a hole it had no idea about, so a resume across
// the outage was answered "caught up".
func TestAResubscriptionEndsThisInstancesCoverage(t *testing.T) {
	mr := miniredis.RunT(t)
	cutter := newTCPCutter(t, mr.Addr())

	client := redis.NewClient(&redis.Options{Addr: cutter.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	waitForSubscribers(t, mr, "pad:events:ws-1", true)

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	drain(t, ch, 1)

	// Control: while the connection is healthy, a resume inside our coverage
	// is served and nothing is reported.
	if got := b.EventsSince("ws-1", 1); got == nil {
		t.Fatal("a caught-up cursor must be served while the subscription is healthy")
	}
	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a healthy subscription must report no reset, got %v", resets)
	}

	// The failover. Events published while we are down never reach us.
	cutter.cut()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, resets := obs.snapshot(); len(resets) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, resets := obs.snapshot()
	if len(resets) == 0 {
		t.Fatal("a pub/sub resubscription must end this instance's coverage; nothing was reported")
	}
	if resets[0] != ResetReasonSubscriptionResumed {
		t.Fatalf("expected %q, got %q", ResetReasonSubscriptionResumed, resets[0])
	}

	// And the coverage really is gone: the cursor that was served above is
	// now a gap, because we cannot account for the outage.
	if got := b.EventsSince("ws-1", 1); got != nil {
		t.Fatalf("after a resubscription the buffer must not vouch for the outage, got %d events", len(got))
	}

	// THE PROPERTY THAT MATTERS MOST, and the one the first version of this
	// fix got wrong: the loop must RECOVER. Reporting the break and then
	// exiting would leave the instance publishing fine and receiving nothing
	// — strictly worse than the silent reconnect it replaced, and invisible
	// from outside the process.
	deadline = time.Now().Add(10 * time.Second)
	var recovered bool
	for time.Now().Before(deadline) && !recovered {
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		select {
		case <-ch:
			recovered = true
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !recovered {
		t.Fatal("the receive loop must keep receiving after a reconnect; it stopped")
	}

	// ...and coverage restarts from there, so clients are not stuck resyncing.
	last := b.EventsSince("ws-1", 0)
	if len(last) == 0 {
		t.Fatal("coverage must be re-established after the reconnect")
	}
	if got := b.EventsSince("ws-1", last[len(last)-1].ID); got == nil {
		t.Fatal("a cursor at our newest post-reconnect event must be served")
	}
}

// The guard that keeps pad_event_sequence_resets_total meaningful: a
// reconnect on a workspace this instance has buffered NOTHING for is not a
// coverage break, because there was no coverage and no client that could have
// been told it was current. Without this, every idle workspace's reconnect
// would give the counter a baseline and "no benign baseline" — the sentence
// the operator table uses to justify alerting on it — would be false.
//
// Found by mutation: removing the guard left every existing test green.
func TestAReconnectOnAnIdleWorkspaceIsNotAReset(t *testing.T) {
	mr := miniredis.RunT(t)
	cutter := newTCPCutter(t, mr.Addr())

	client := redis.NewClient(&redis.Options{Addr: cutter.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	// Subscribed, but nothing has ever been published here, so there is no
	// replay buffer for this workspace.
	ch, _ := b.Subscribe("ws-idle")
	defer b.Unsubscribe(ch)
	waitForSubscribers(t, mr, "pad:events:ws-idle", true)

	cutter.cut()

	// Give the loop time to notice, redial, and (wrongly) report.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, resets := obs.snapshot(); len(resets) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a reconnect with nothing buffered must not report a reset, got %v", resets)
	}

	// Positive control in the same test, so a bus that never reports anything
	// at all cannot pass: once something IS buffered, the next reconnect does
	// report.
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-idle"})
	drain(t, ch, 1)
	cutter.cut()

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, resets := obs.snapshot(); len(resets) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("with a buffer in place, a reconnect must report a coverage break")
}

// The receive-loop-exit counter, wired end to end. It is not a
// should-never-fire alarm here — unlike the watch stream's twin, this bus has
// one loop PER WORKSPACE, so a loop exits every time a workspace's last local
// subscriber leaves. The test pins that reading, because a counter documented
// as "expected at zero" and one documented as "read it as a rate" lead an
// operator to opposite conclusions.
func TestTheReceiveLoopExitIsReported(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, _ := b.Subscribe("ws-1")
	waitForSubscribers(t, mr, "pad:events:ws-1", true)

	// Control: a live subscription's loop has not exited.
	if got := obs.exits(); got != 0 {
		t.Fatalf("a live subscription must not report a loop exit, got %d", got)
	}

	b.Unsubscribe(ch)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if obs.exits() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the last subscriber leaving must report a loop exit, got %d", obs.exits())
}

// codex round 7 #4. A receive loop can notice its connection died long after
// the workspace was unsubscribed and resubscribed under it. Without a
// generation check in the cleanup, the stale goroutine deletes the
// REPLACEMENT buffer, and a client that arrived after the outage ended is told
// to resync for an incident that never touched it.
//
// Driven by calling dropWorkspaceCoverage with the OLD generation, which is
// exactly what that goroutine holds — the race is real but not schedulable, so
// reproducing it through the transport would be a flake rather than a test.
func TestAStaleGoroutineCannotDropTheReplacementBuffer(t *testing.T) {
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, _ := b.Subscribe("ws-1")
	oldGen := b.currentSubGen("ws-1")

	// Everyone leaves: subscription torn down, buffer gone.
	b.Unsubscribe(ch)

	// A viewer returns and the workspace starts buffering again.
	ch2, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch2)
	newGen := b.currentSubGen("ws-1")
	if newGen == oldGen {
		t.Fatal("fixture drifted: resubscribing must produce a new generation")
	}
	b.fanOutFromRedis(newGen, 0, Event{ID: 500, Type: ItemUpdated, WorkspaceID: "ws-1"})

	// NOW the old goroutine finally notices its connection died.
	b.dropWorkspaceCoverage("ws-1", ResetReasonSubscriptionResumed, oldGen)

	got := b.EventsSince("ws-1", 500)
	if got == nil {
		t.Fatal("a stale subscription's cleanup must not drop the replacement buffer")
	}
	if len(got) != 0 {
		t.Fatalf("expected caught-up, got %d events", len(got))
	}
	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a stale cleanup must not report a reset the live client never experienced, got %v", resets)
	}

	// Control: the CURRENT generation's cleanup still works, or the check
	// would have disabled the mechanism it guards.
	b.dropWorkspaceCoverage("ws-1", ResetReasonSubscriptionResumed, newGen)
	if got := b.EventsSince("ws-1", 500); got != nil {
		t.Fatalf("the live subscription's cleanup must still end coverage, got %d events", len(got))
	}
	if _, resets := obs.snapshot(); len(resets) != 1 {
		t.Fatalf("expected exactly one reset, from the live generation, got %v", resets)
	}
}
