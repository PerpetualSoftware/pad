package watchevents

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
// SUBSCRIBE frame — the failure go-redis's Client.Subscribe discards
// (BUG-2764). A conn wrapper rather than a proxy close, because a close races
// the kernel's socket buffer and what then fails is a later read on a
// different path; the write error is the thing the fix inspects. Twin of the
// helper in internal/events, kept local: the two packages share no test
// code.
type subscribeWriteFailer struct {
	failed atomic.Int64
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
		c.f.failed.Add(1)
		return 0, errInjectedSubscribeWrite
	}
	return c.Conn.Write(p)
}

// TestAFailedSubscribeIsReportedAsItselfAndAtOnce pins BUG-2764's watchevents
// half. Before the fix a SUBSCRIBE whose write failed came back as a
// healthy-looking PubSub; the only sign was the confirmation wait expiring
// five seconds later as a read timeout — an error that named the wait, not
// the cause. Both sites are covered: the constructor (measured by
// how long it takes, since its contract is to log and continue) and
// resubscribe (measured by WHICH error it returns, which is the
// discriminating assertion: the timeout is what the unfixed code returns,
// and it is not the injected one).
func TestAFailedSubscribeIsReportedAsItselfAndAtOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	f := &subscribeWriteFailer{}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: f.dial})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	started := time.Now()
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	if f.failed.Load() == 0 {
		t.Fatal("the constructor's SUBSCRIBE write was never refused; this test could not have discriminated")
	}
	// The unfixed constructor waits out its 5s confirmation bound on a
	// subscription Redis never received. 2s is the loaded-CI margin against a
	// path that now does no waiting at all.
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("construction took %v with a refused SUBSCRIBE: the constructor waited for an acknowledgement that could not come", took)
	}
	// And nothing was installed: a retained dead PubSub would keep the
	// retry gate (cycleIfIdle's `b.pubsub == nil`) closed for the life of
	// the process (codex round 1 P1).
	if b.currentPubSub() != nil {
		t.Fatal("the constructor kept a PubSub whose SUBSCRIBE failed; the retry gate can never open")
	}

	// FABRICATES THE CYCLE'S TEARDOWN STATE ON PURPOSE: resubscribe installs
	// only into an empty slot, and driving a full idle cycle here would test
	// the cycle, not the subscribe. The retired PubSub is closed outside the
	// lock exactly as the cycle does it.
	b.mu.Lock()
	old := b.pubsub
	b.pubsub, b.subCancel = nil, nil
	b.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	before := f.failed.Load()
	started = time.Now()
	err := b.resubscribe()
	if f.failed.Load() == before {
		t.Fatal("resubscribe never issued a SUBSCRIBE; this test could not have discriminated")
	}
	if !errors.Is(err, errInjectedSubscribeWrite) {
		t.Fatalf("resubscribe error = %v, want the injected SUBSCRIBE write error — the failure was reported as something else (the unfixed code returns the confirmation wait's read timeout, five seconds later)", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("resubscribe took %v to report a refused SUBSCRIBE", took)
	}
	if b.currentPubSub() != nil {
		t.Fatal("a subscription was installed for a SUBSCRIBE that failed to reach Redis")
	}
}
