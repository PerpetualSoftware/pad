package watchevents

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// blackholeProxy stops delivering server→client bytes on connections that have
// carried a SUBSCRIBE, while continuing to forward client→server on them and
// serving new connections normally.
//
// THIS PACKAGE ALREADY HAS A PROXY AND IT IS THE WRONG ONE. tcpCutter
// (redis_reconnect_test.go) SEVERS connections, which is what BUG-2739 needed:
// go-redis notices a close and resubscribes, which is the subscription_resumed
// path and is already detected. A half-open route never closes — writes keep
// succeeding, reads go silent, and nothing in the library notices. Using
// tcpCutter here would test the case that already works.
//
// Two details that cost real debugging in internal/events and carry with the
// harness rather than being rediscovered:
//
//  1. PER-CONNECTION darkening, not a global flag consulted at read time. A
//     shared bool meant re-enabling delivery for future connections also
//     revived the ones meant to be dark, so nothing was ever wedged.
//  2. Darken ONLY a connection that carried a SUBSCRIBE. go-redis puts PUBLISH
//     on its ordinary pool and the subscription on a connection from a separate
//     pub/sub pool; darkening both breaks the PROBE as well as the delivery,
//     and the test then passes against an implementation that treats a failed
//     probe as evidence of a dead peer — the exact defect the premise check
//     exists to prevent. Hence the zero-probe-failures assertion below.
type proxiedConn struct {
	dark     *atomic.Bool
	isPubSub *atomic.Bool
}

type blackholeProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []proxiedConn
}

func newBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &blackholeProxy{ln: ln}
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
			dark, isPubSub := &atomic.Bool{}, &atomic.Bool{}
			p.mu.Lock()
			p.conns = append(p.conns, proxiedConn{dark: dark, isPubSub: isPubSub})
			p.mu.Unlock()

			go func() {
				buf := make([]byte, 4096)
				for {
					n, rerr := client.Read(buf)
					if n > 0 {
						if bytes.Contains(bytes.ToLower(buf[:n]), []byte("subscribe")) {
							isPubSub.Store(true)
						}
						if _, werr := server.Write(buf[:n]); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}()
			go func() {
				defer func() { _ = client.Close(); _ = server.Close() }()
				buf := make([]byte, 4096)
				for {
					n, rerr := server.Read(buf)
					if n > 0 && !dark.Load() {
						if _, werr := client.Write(buf[:n]); werr != nil {
							return
						}
					}
					if rerr != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

// blackhole stops inbound delivery on every subscription connection currently
// open, for good. Writes keep succeeding, which is what makes it half-open
// rather than a disconnection — and is exactly the state go-redis reports as
// healthy.
func (p *blackholeProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		if c.isPubSub.Load() {
			c.dark.Store(true)
		}
	}
}

// TestAWedgedWatchRouteIsDetectedEndToEnd is the integration test for this
// unit's central claim, and the only test here that exercises a REAL half-open
// socket rather than a simulated one.
//
// Every other test drives the mechanism through a fake clock — necessary,
// because the threshold is 90 seconds by construction and miniredis always
// answers, but it means each of them ASSUMES the wedge rather than producing
// it. This one produces it: the bus's own heartbeats keep reaching Redis while
// nothing comes back, and both halves of the claim are asserted — the
// connection is cycled, and delivery resumes on the replacement.
func TestAWedgedWatchRouteIsDetectedEndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newBlackholeProxy(t, mr.Addr())

	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, 64, redisns.Default, true)
	obs := newRecordingObserver()
	b.SetObserver(obs)
	t.Cleanup(b.Close)

	ch, _ := b.Subscribe()
	defer b.Unsubscribe(ch)

	// Prove the route works before breaking it, so a test that never delivered
	// anything cannot pass by looking like a successful detection.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-before"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the route never worked, so wedging it proves nothing")
	}

	proxy.blackhole()
	b.setMaintenanceCadence(50*time.Millisecond, 200*time.Millisecond)

	// THE PROBE MUST KEEP SUCCEEDING while nothing comes back — that pairing IS
	// the half-open case. Only the subscription's connection is darkened, so the
	// publish path stays healthy and this stays at zero; a run that drifts into
	// the cannot-probe case fails here instead of passing quietly.
	defer func() {
		if got := obs.snapshot().probeFailures; got != 0 {
			t.Errorf("%d heartbeat publishes failed: this run exercised the cannot-probe path, not a half-open route", got)
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for obs.snapshot().resets[ResetReasonIdleTimeout] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a wedged route was never detected in 20s: go-redis cannot see this and neither can we")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ...and the replacement actually delivers, which is what separates
	// recovery from a resync loop.
	for {
		if time.Now().After(deadline) {
			t.Fatal("the subscription was cycled but the replacement never delivered anything")
		}
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-after"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case n := <-ch:
			if n.ItemRef == "TASK-after" {
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
}
