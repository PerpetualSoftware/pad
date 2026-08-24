package events

import (
	"bytes"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// blackholeProxy is a TCP proxy that can stop delivering server→client bytes on
// the connections it already holds, while continuing to accept and forward
// client→server on them and serving new connections normally.
//
// THAT ASYMMETRY IS THE WHOLE POINT, and it is what no other instrument in this
// package can produce. miniredis is a working Redis, so every unit test here
// has to SIMULATE a wedge by advancing a clock and stamping fields. This
// reproduces the real thing: a route that stopped carrying traffic without
// closing — no FIN, no RST, writes still accepted — which is precisely the
// failure go-redis's health check cannot see, because PubSub.Ping writes the
// command and never reads a reply.
//
// New connections keep working, so the replacement subscription can succeed
// and the test can assert RECOVERY rather than only detection.
type proxiedConn struct {
	dark     *atomic.Bool
	isPubSub *atomic.Bool
}

type blackholeProxy struct {
	ln      net.Listener
	backend string
	mu      sync.Mutex
	conns   []proxiedConn
}

func newBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &blackholeProxy{ln: ln, backend: backend}
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

			// PER-CONNECTION, NOT A GLOBAL FLAG, and the first version of this
			// proxy got that wrong in a way that made the test vacuous: a
			// global "dead" bool consulted at read time meant that re-enabling
			// delivery for FUTURE connections also revived the ones that were
			// supposed to be dark, so nothing was ever wedged and the test
			// failed for the wrong reason. The connections open when blackhole()
			// is called are the ones that go silent, permanently; anything
			// dialled afterwards is healthy.
			dark := &atomic.Bool{}
			isPubSub := &atomic.Bool{}
			p.mu.Lock()
			p.conns = append(p.conns, proxiedConn{dark: dark, isPubSub: isPubSub})
			p.mu.Unlock()

			// OUTBOUND ALWAYS FLOWS, and the connection is CLASSIFIED as it
			// does. go-redis puts PUBLISH on its ordinary connection pool and
			// each subscription on a connection from a separate pub/sub pool;
			// darkening both would break the probe as well as the delivery,
			// and the test could then pass on an implementation that treats a
			// failed probe as evidence of a dead peer — the exact defect the
			// premise check exists to prevent (codex round 14). Only a
			// connection that has carried a SUBSCRIBE goes dark.
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := client.Read(buf)
					if n > 0 {
						if bytes.Contains(bytes.ToLower(buf[:n]), []byte("subscribe")) {
							isPubSub.Store(true)
						}
						if _, werr := server.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			go func() {
				defer func() { _ = client.Close(); _ = server.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := server.Read(buf)
					if n > 0 && !dark.Load() {
						if _, werr := client.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

// blackhole stops inbound delivery on every connection currently open, for
// good. Writes on those connections keep succeeding, which is what makes this
// a half-open route rather than a disconnection — and is exactly the state
// go-redis reports as healthy, because PubSub.Ping writes without reading.
//
// Connections opened afterwards are unaffected, so the replacement subscription
// can succeed and the test can assert RECOVERY rather than only detection.
func (p *blackholeProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		if c.isPubSub.Load() {
			c.dark.Store(true)
		}
	}
}

// TestAWedgedRouteIsDetectedEndToEnd is the integration test for BUG-2738's
// central claim, and the only test here that exercises a REAL half-open socket
// rather than a simulated one (codex round 13, P3).
//
// Everything else in this package drives the mechanism through a fake clock:
// necessary, because the threshold is 90 seconds by construction and miniredis
// always answers, but it means every one of those tests assumes the wedge
// rather than producing it. This one produces it — the bus's own heartbeats
// keep reaching Redis while nothing comes back — and asserts both halves of
// the claim: the connection is cycled, and delivery resumes on the replacement.
//
// It runs on real time with a compressed cadence, so it is deliberately the
// slowest test in the file.
func TestAWedgedRouteIsDetectedEndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newBlackholeProxy(t, mr.Addr())

	client := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	t.Cleanup(func() { _ = client.Close() })

	b := NewRedisBusWithKeys(client, redisns.Default, false, true)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	t.Cleanup(b.Close)

	ch, _, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("subscribe: %v", outcome)
	}
	defer b.Unsubscribe(ch)

	// Prove the route works before breaking it, so a test that never delivered
	// anything cannot pass by looking like a successful detection.
	publisher := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = publisher.Close() })
	b.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1", ItemID: "before"})
	select {
	case ev := <-ch:
		if ev.ItemID != "before" {
			t.Fatalf("fixture: unexpected event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the route never worked, so wedging it proves nothing")
	}

	// Break the receive direction only. Writes keep succeeding, so the bus goes
	// on publishing heartbeats it will never see come back — exactly the state
	// no health check in go-redis can observe.
	proxy.blackhole()

	b.setMaintenanceCadence(50*time.Millisecond, 200*time.Millisecond)

	deadline := time.Now().Add(20 * time.Second)
	// THE PROBE MUST KEEP SUCCEEDING while nothing comes back — that pairing IS
	// the half-open case, and without asserting it this test would also pass on
	// an implementation that cycles because it could not publish at all
	// (codex round 14). Only the subscription's connection is darkened, so the
	// publish path stays healthy and this stays at zero.
	defer func() {
		if got := obs.probeFailureCount(); got != 0 {
			t.Fatalf("%d heartbeat publishes failed: this run exercised the cannot-probe path, not a half-open route", got)
		}
	}()
	for obs.cycledCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a wedged route was never detected in 20s (probe failures: %d): go-redis cannot see this and neither can we",
				obs.probeFailureCount())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ...and the replacement actually delivers, which is the half that
	// distinguishes recovery from a resync loop.
	for {
		if time.Now().After(deadline) {
			t.Fatal("the workspace was cycled but the replacement never delivered anything")
		}
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "after"})
		select {
		case ev := <-ch:
			if ev.ItemID == "after" {
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
}
