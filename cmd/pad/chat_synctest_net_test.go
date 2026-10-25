package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// memNet is an in-memory listener for a synctest bubble. Loopback TCP
// sockets do not block durably, so a bubble's clock never advances past an
// httptest.Server; net.Pipe does block durably (TASK-4067).
type memNet struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemNet() *memNet {
	return &memNet{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (m *memNet) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case <-m.closed:
		return nil, net.ErrClosed
	}
}

func (m *memNet) Close() error {
	m.once.Do(func() { close(m.closed) })
	return nil
}

func (m *memNet) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80} }

func (m *memNet) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case m.conns <- server:
		return client, nil
	case <-m.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runMonitorInBubble runs runWatchMonitor end to end in a synctest bubble,
// against handler served over a memNet, so its reconnect and identity
// backoffs cost virtual time only. setup runs inside the bubble before the
// monitor starts, with the bubble's ctx (bounded by timeout) and the
// server's URL. elapsed is the monitor's virtual run time.
//
// Every client on the monitor's path uses http.DefaultTransport (the stream's
// &http.Client{} and cli.newClientFromURL's clients), so the bubble swaps it
// for a memNet transport and restores it in Cleanup. A process-global swap is
// safe only from a sequential top-level test with no parallel subtests (a
// caller that uses t.Setenv, as the monitor tests do, cannot be parallel). No
// other test can run during it: a top-level t.Parallel test is resumed only
// after every sequential top-level test has returned, and a parallel subtest
// finishes inside its own parent, which returns before the next top-level
// test starts. The helper enforces the precondition with t.Setenv.
func runMonitorInBubble(t *testing.T, timeout time.Duration, handler http.Handler, setup func(ctx context.Context, cancel context.CancelFunc, serverURL string) error) (out string, elapsed time.Duration, runErr error) {
	t.Helper()
	// Enforces the no-parallel precondition above: t.Setenv panics in a
	// parallel test (or one with a parallel ancestor).
	t.Setenv("PAD_TEST_DEFAULT_TRANSPORT_SWAPPED", "1")
	// os/signal starts its process-wide watcher goroutine on the first
	// Notify. Started inside a bubble, that goroutine would belong to it
	// and block forever in a receive that is not durable, so the bubble
	// could never finish. Start it here, outside.
	_, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	stopSignals()
	// captureStdoutDraining's reader blocks on an os.Pipe, which is not
	// durable either, so the bubble runs inside the capture, not around it.
	out = captureStdoutDraining(t, func() {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			mn := newMemNet()
			h := httptest.NewUnstartedServer(handler)
			h.Listener = mn
			h.Start()
			oldTransport := http.DefaultTransport
			transport := &http.Transport{DialContext: mn.dial}
			http.DefaultTransport = transport
			// Cleanup runs inside the bubble, before synctest.Test returns.
			t.Cleanup(func() {
				http.DefaultTransport = oldTransport
				transport.CloseIdleConnections()
				h.Close()
			})
			if err := setup(ctx, cancel, h.URL); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			runErr = runWatchMonitor(ctx)
			elapsed = time.Since(start)
		})
	})
	return out, elapsed, runErr
}
