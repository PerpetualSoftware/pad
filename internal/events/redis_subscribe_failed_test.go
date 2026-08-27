package events

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// subscribeWriteFailer is a Dialer whose connections fail the WRITE of a
// SUBSCRIBE frame, deterministically, for the first `failures` such writes.
//
// WHY A CONN WRAPPER AND NOT A PROXY CLOSE (BUG-2764). The defect is that
// go-redis's Client.Subscribe discards the error of the SUBSCRIBE write, so the
// instrument has to make that write FAIL. A proxy that closes the connection
// on seeing the command races the kernel: the client's write has usually
// already succeeded into the socket buffer by the time the proxy reads it, and
// what fails is the later read, which is a different error on a different
// path. A net.Conn whose Write returns the error is the failure at the point
// the fix inspects, every time.
//
// Every connection is wrapped, including the bus's own pooled ones; only a
// frame carrying the SUBSCRIBE command is refused, so publishes and pings are
// untouched.
type subscribeWriteFailer struct {
	failures int64 // how many SUBSCRIBE writes to fail; <0 means all of them
	failed   atomic.Int64
}

var errInjectedSubscribeWrite = errors.New("injected: SUBSCRIBE write failed")

func (f *subscribeWriteFailer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &subscribeFailingConn{Conn: c, f: f}, nil
}

type subscribeFailingConn struct {
	net.Conn
	f *subscribeWriteFailer
}

func (c *subscribeFailingConn) Write(p []byte) (int, error) {
	if bytes.Contains(bytes.ToLower(p), []byte("\r\n$9\r\nsubscribe\r\n")) {
		if c.f.failures < 0 || c.f.failed.Load() < c.f.failures {
			c.f.failed.Add(1)
			return 0, errInjectedSubscribeWrite
		}
	}
	return c.Conn.Write(p)
}

func newFailingSubscribeBus(t *testing.T, failures int64) (*RedisBus, *subscribeWriteFailer, *recordingObserver) {
	t.Helper()
	mr := miniredis.RunT(t)
	f := &subscribeWriteFailer{failures: failures}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: f.dial})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	return b, f, obs
}

// TestAFailedSubscribeRefusesItsCallerInsteadOfAdmittingIt is BUG-2764's
// regression test.
//
// Before the fix a SUBSCRIBE whose write failed came back from go-redis as a
// PubSub indistinguishable from a healthy one; the bus installed it, waited
// for an acknowledgement that could never come, and admitted the caller as an
// unconfirmed subscription — SubscribeOK, a live-looking channel, a counter
// promising a reconcile when the acknowledgement landed. Nothing ever
// arrived. Every assertion below is stated as what that WRONG behaviour would
// leave behind (CONVE-12), because "nothing happened" is compatible with the
// bug.
func TestAFailedSubscribeRefusesItsCallerInsteadOfAdmittingIt(t *testing.T) {
	b, f, obs := newFailingSubscribeBus(t, -1)

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)

	// PREMISE: the instrument fired. The loop retries establishment once, so
	// a refusal with fewer than two failed writes is a refusal for some other
	// reason.
	if got := f.failed.Load(); got < 2 {
		t.Fatalf("SUBSCRIBE writes failed = %d, want at least 2 (initial attempt + the loop's retry); the instrument never armed", got)
	}

	if outcome != SubscribeFailed {
		t.Fatalf("outcome = %v, want failed — the caller was admitted into a subscription Redis never received", outcome)
	}
	if ch != nil {
		t.Fatal("a refused caller was handed a channel")
	}

	b.mu.Lock()
	_, live := b.wsSubs["ws-1"]
	_, inFlight := b.pendingSubs["ws-1"]
	count := b.wsCounts["ws-1"]
	b.mu.Unlock()
	if live {
		t.Fatal("a subscription was installed for a SUBSCRIBE that failed to reach Redis")
	}
	if inFlight {
		t.Fatal("an establishment record was left behind: the next caller would wait on it forever")
	}
	if count != 0 {
		t.Fatalf("workspace subscriber count = %d after a refusal, want 0 — the refused caller is still counted against the workspace limit", count)
	}
	// The bug's signature: an unconfirmed admission that will never be
	// reconciled. A refusal reports none.
	if got := obs.unconfirmedCount(); got != 0 {
		t.Fatalf("SubscriptionUnconfirmed reported %d times for a refused subscription, want 0 — the caller was admitted rather than refused", got)
	}
}

// TestATransientSubscribeFailureIsRetriedOnceAndServesEveryCaller pins the
// other half: a SUBSCRIBE that fails once is not a refusal. The establish
// loop's built-in second pass re-establishes, and both the establisher and a
// joiner that registered during the failed attempt end up on a live
// subscription — each receiving a published event EXACTLY once.
//
// The joiner is placed by the beforeInstallSubscription seam, which runs
// after the SUBSCRIBE has been issued and before the deciding lock: the one
// window in which it finds the failing establishment's record and genuinely
// waits on it.
func TestATransientSubscribeFailureIsRetriedOnceAndServesEveryCaller(t *testing.T) {
	b, f, obs := newFailingSubscribeBus(t, 1)

	type result struct {
		ch      chan Event
		outcome SubscribeOutcome
	}
	joined := make(chan result, 1)
	var placed atomic.Bool
	b.beforeInstallSubscription = func(string) {
		if !placed.CompareAndSwap(false, true) {
			return
		}
		go func() {
			ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
			joined <- result{ch, outcome}
		}()
		// Let the joiner register and find the record before the abandon
		// retires it.
		deadline := time.Now().Add(3 * time.Second)
		for b.WorkspaceSubscriberCount("ws-1") < 2 {
			if time.Now().After(deadline) {
				t.Error("the joiner never registered; this test never exercised the joiner path")
				break
			}
			time.Sleep(time.Millisecond)
		}
	}

	ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if got := f.failed.Load(); got != 1 {
		t.Fatalf("SUBSCRIBE writes failed = %d, want exactly 1; the instrument did not behave as configured", got)
	}
	if outcome != SubscribeOK {
		t.Fatalf("establisher outcome = %v, want ok — one transient failure became a refusal", outcome)
	}
	defer b.Unsubscribe(ch)

	var jr result
	select {
	case jr = <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner never returned — the failed establishment's record was not retired and stranded it")
	}
	if jr.outcome != SubscribeOK {
		t.Fatalf("joiner outcome = %v, want ok", jr.outcome)
	}
	defer b.Unsubscribe(jr.ch)

	if got := obs.unconfirmedCount(); got != 0 {
		t.Fatalf("SubscriptionUnconfirmed reported %d times, want 0 — a caller was admitted into the FAILED attempt rather than the retry", got)
	}

	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1"})
	for name, c := range map[string]chan Event{"establisher": ch, "joiner": jr.ch} {
		select {
		case <-c:
		case <-time.After(3 * time.Second):
			t.Fatalf("the %s never received an event published after both were admitted: it is holding a channel wired to nothing", name)
		}
		select {
		case dup := <-c:
			t.Fatalf("the %s received a second copy (%d): the failed attempt's subscription is alive alongside the retry's", name, dup.ID)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// TestAJoinerOfAFailedEstablishmentIsRefusedToo covers the population the
// establisher's refusal does not reach on its own: a caller that registered
// while the failing establishment was in flight and waited on its record. It
// takes the establishment over once, fails the same way, and must return
// SubscribeFailed rather than SubscribeOK with a dead channel — and must
// leave the workspace with no subscribers counted, so the next caller's
// admission is not charged for two ghosts.
func TestAJoinerOfAFailedEstablishmentIsRefusedToo(t *testing.T) {
	b, _, _ := newFailingSubscribeBus(t, -1)

	type result struct {
		ch      chan Event
		outcome SubscribeOutcome
	}
	joined := make(chan result, 1)
	var placed atomic.Bool
	b.beforeInstallSubscription = func(string) {
		if !placed.CompareAndSwap(false, true) {
			return
		}
		go func() {
			ch, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
			joined <- result{ch, outcome}
		}()
		deadline := time.Now().Add(3 * time.Second)
		for b.WorkspaceSubscriberCount("ws-1") < 2 {
			if time.Now().After(deadline) {
				t.Error("the joiner never registered; this test never exercised the joiner path")
				break
			}
			time.Sleep(time.Millisecond)
		}
	}

	_, _, outcome := b.SubscribeIfAllowed(context.Background(), "ws-1", 0)
	if outcome != SubscribeFailed {
		t.Fatalf("establisher outcome = %v, want failed", outcome)
	}

	var jr result
	select {
	case jr = <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("the joiner never returned — stranded on a record nobody retired")
	}
	if jr.outcome != SubscribeFailed {
		t.Fatalf("joiner outcome = %v, want failed — it was handed a channel wired to nothing", jr.outcome)
	}
	if jr.ch != nil {
		t.Fatal("a refused joiner was handed a channel")
	}
	if got := b.WorkspaceSubscriberCount("ws-1"); got != 0 {
		t.Fatalf("workspace subscriber count = %d after both refusals, want 0", got)
	}
}
