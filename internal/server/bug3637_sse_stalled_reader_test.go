package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// BUG-3637: a client that connects to an event stream and then stops reading
// must not pin its handler. Once the socket buffers fill, a frame write blocks;
// before the fix nothing bounded that block (the server sets no WriteTimeout),
// and the handler is the goroutine that releases the admission slot.
//
// Through the real router and a real TCP connection (CONVE-19), because the
// write deadline reaches the socket only if every middleware writer in between
// unwraps. Both ends' buffers are shrunk and keepalives are scheduled every
// millisecond, so the buffers fill quickly, though how quickly depends on the
// scheduler (see stallFillCeiling).

// stalledStreamServer starts srv behind a listener whose accepted connections
// have a small send buffer and report their first failed write, which is the
// moment a frame write overran its deadline.
func stalledStreamServer(t *testing.T, srv *Server) (*httptest.Server, *writeFailures) {
	t.Helper()
	srv.sseKeepaliveOverride = time.Millisecond
	srv.sseWriteTimeoutOverride = 200 * time.Millisecond
	ts := httptest.NewUnstartedServer(srv)
	wf := &writeFailures{failed: make(chan struct{})}
	ts.Listener = &writeFailureListener{Listener: ts.Listener, wf: wf}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, wf
}

// writeFailures records the first write that failed on the stalled stream's
// connection, the one whose client address watch named. A failure on any
// other connection the server accepted is ignored, so it cannot stand in for
// the stream's (codex r1). failed is closed when it happens.
type writeFailures struct {
	mu     sync.Mutex
	client string
	once   sync.Once
	failed chan struct{}
	err    error
	at     time.Time
}

// watch names the client address of the connection whose failure counts.
func (wf *writeFailures) watch(client string) {
	wf.mu.Lock()
	defer wf.mu.Unlock()
	wf.client = client
}

func (wf *writeFailures) record(remote net.Addr, err error) {
	wf.mu.Lock()
	watched := wf.client != "" && remote.String() == wf.client
	wf.mu.Unlock()
	if !watched {
		return
	}
	wf.once.Do(func() {
		wf.err, wf.at = err, time.Now()
		close(wf.failed)
	})
}

type writeFailureListener struct {
	net.Listener
	wf *writeFailures
}

func (l *writeFailureListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return &writeFailureConn{Conn: c, wf: l.wf}, nil
}

// writeFailureConn passes everything through, SetWriteDeadline included, and
// reports a failed Write.
type writeFailureConn struct {
	net.Conn
	wf *writeFailures
}

func (c *writeFailureConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.wf.record(c.RemoteAddr(), err)
	}
	return n, err
}

// openStalledStream sends one GET for path over a raw connection, reads the
// response head, and then never reads again. The connection stays open until
// the test ends, so a release cannot come from the client going away.
func openStalledStream(t *testing.T, ts *httptest.Server, wf *writeFailures, path string, header http.Header) http.Header {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if wf != nil { // nil: a server not built by stalledStreamServer
		wf.watch(conn.LocalAddr().String())
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}

	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n", path, u.Host)
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read the head only, through a reader that buffers no more than it must.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReaderSize(conn, 16)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status line %q, want 200", strings.TrimSpace(status))
	}
	got := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":"); ok {
			got.Add(k, strings.TrimSpace(v))
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return got
}

// How long the buffers may take to fill. This bounds the harness, not the
// code under test: the 1ms keepalives fill both 4KB buffers in about 1.1s
// under -race on an idle 8-core box (BUG-3824), and a loaded CI runner drops
// ticks, which is what made a single end-to-end budget flaky.
const stallFillCeiling = 60 * time.Second

// How long the handler may take, after its frame write fails, to return and
// release its slot. The release is a deferred call on that return, so this is
// scheduling slack only; it stays well under stallFillCeiling so a slot that
// is never released cannot pass as a slow fill.
const stallReleaseMargin = 5 * time.Second

// assertSlotReleased requires the stream's admission slot to be held first
// (otherwise the release check passes against a handler that never admitted
// the stream), then waits for the server's write to the stalled socket to fail
// on its deadline, and then requires the slot released while the client is
// still connected.
//
// The two phases are timed apart (BUG-3824). Filling the buffers takes as
// long as the scheduler lets the keepalive ticker run; that is the harness.
// From the failed write on, the release is the code under test. Without a
// write deadline no write ever fails, and the first wait ends the test.
func assertSlotReleased(t *testing.T, srv *Server, wf *writeFailures) {
	t.Helper()
	if held := srv.admission().heldTotal(); held != 1 {
		t.Fatalf("admission slots held once the stream opened = %d, want 1: the slot this test is about was never reserved", held)
	}
	start := time.Now()
	select {
	case <-wf.failed:
	case <-time.After(stallFillCeiling):
		t.Fatalf("no write to the stalled client failed within %v (write timeout %v): "+
			"a blocked frame write pins the handler, or the buffers never filled", stallFillCeiling, srv.sseWriteTimeoutOverride)
	}
	if !errors.Is(wf.err, os.ErrDeadlineExceeded) {
		t.Fatalf("the first failed write returned %v, want a write deadline overrun", wf.err)
	}
	deadline := wf.at.Add(stallReleaseMargin)
	for srv.admission().heldTotal() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the admission slot was still held %v after a frame write overran its deadline: "+
				"the handler did not return", stallReleaseMargin)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("buffers filled and a write failed after %v; slot released %v later",
		wf.at.Sub(start).Round(time.Millisecond), time.Since(wf.at).Round(time.Millisecond))
}

func TestSSE_BUG3637_StalledReaderReleasesItsSlot(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	ts, wf := stalledStreamServer(t, srv)
	slug := createTestWorkspace(t, ts.URL, "Stalled")

	openStalledStream(t, ts, wf, "/api/v1/events?workspace="+slug, nil)
	if n := srv.events.SubscriberCount(); n != 1 {
		t.Fatalf("bus subscribers once the stream opened = %d, want 1", n)
	}
	assertSlotReleased(t, srv, wf)
	// The subscription and its buffers go with the handler, not only the
	// slot (codex final round 2). Unsubscribe is deferred on the same
	// return as the release, so it has run once the slot reads free.
	if n := srv.events.SubscriberCount(); n != 0 {
		t.Errorf("bus subscribers after the stalled stream ended = %d, want 0", n)
	}
}

func TestWatchEvents_BUG3637_StalledReaderReleasesItsSlot(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts, wf := stalledStreamServer(t, srv)

	openStalledStream(t, ts, wf, "/api/v1/events/stream", http.Header{"Authorization": {"Bearer " + tok.Token}})
	assertSlotReleased(t, srv, wf)
}

// deadlineRecorder is an httptest.ResponseRecorder that accepts write
// deadlines, for tests that stream into a recorder. A plain recorder does not,
// and the event streams refuse such a writer.
type deadlineRecorder struct{ *httptest.ResponseRecorder }

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{httptest.NewRecorder()}
}

func (deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

// A writer that cannot take a write deadline is refused before anything is
// streamed or subscribed: the frames could not be bounded (codex r1 High).
// httptest.ResponseRecorder is that writer: it is a Flusher with no deadline.
func TestSSE_BUG3637_WriterWithoutDeadlineIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	slug := createTestWorkspace(t, ts.URL, "NoDeadline")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+slug, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "event: connected") {
		t.Errorf("a refused stream wrote a connected event; body=%q", rr.Body.String())
	}
	if n := srv.events.SubscriberCount(); n != 0 {
		t.Errorf("SubscriberCount = %d after a refusal, want 0", n)
	}
	if held := srv.admission().heldTotal(); held != 0 {
		t.Errorf("admission slots held after a refusal = %d, want 0", held)
	}

	// Control: the same request through a writer that takes a deadline
	// streams, so the 500 above is the deadline check and not the request.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	rec := newDeadlineRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+slug, nil).WithContext(ctx2))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: connected") {
		t.Errorf("control: status %d, body %q; want 200 with a connected event", rec.Code, rec.Body.String())
	}
}

func TestWatchEvents_BUG3637_WriterWithoutDeadlineIsRefused(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "event:") || strings.Contains(rr.Body.String(), ": keepalive") {
		t.Errorf("a refused watch stream wrote stream frames; body=%q", rr.Body.String())
	}
	if held := srv.admission().heldTotal(); held != 0 {
		t.Errorf("admission slots held after a refusal = %d, want 0", held)
	}

	// Control, as above.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil).WithContext(ctx2)
	req2.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := newDeadlineRecorder()
	srv.ServeHTTP(rec, req2)
	if rec.Code != http.StatusOK {
		t.Errorf("control: status %d, body %q; want 200", rec.Code, rec.Body.String())
	}
}

// The production write deadline must outlast the keepalive cadence by a
// stated margin, twice, on both streams (lead ruling on codex final-round
// Low): the stalled-reader tests use a 200ms override, so on their own they
// would pass a production bound short enough to disconnect a client that is
// reading normally. Real constants, no overrides.
func TestSSE_BUG3637_WriteTimeoutOutlastsTheKeepaliveCadence(t *testing.T) {
	const margin = 2
	// An absolute floor too (bug-hunter r4): the first stream's interval is
	// what the bound is defined from, so the margin alone cannot fail for it
	// if both shrink together.
	const floor = 30 * time.Second
	if sseWriteTimeout < floor {
		t.Errorf("sseWriteTimeout = %v, want at least %v", sseWriteTimeout, floor)
	}
	for _, c := range []struct {
		stream    string
		keepalive time.Duration
	}{
		{"/api/v1/events", sseKeepaliveInterval},
		{"/api/v1/events/stream", watchEventsKeepaliveInterval},
	} {
		if sseWriteTimeout < margin*c.keepalive {
			t.Errorf("%s: sseWriteTimeout = %v, want at least %d x the %v keepalive interval",
				c.stream, sseWriteTimeout, margin, c.keepalive)
		}
	}
	// The bound a stream actually carries, from the constructor both handlers
	// call, which takes no timeout of its own.
	st, err := (&Server{}).openSSEStream(newDeadlineRecorder())
	if err != nil {
		t.Fatalf("openSSEStream: %v", err)
	}
	if st.timeout != sseWriteTimeout {
		t.Errorf("with no override a stream is bounded by %v, want sseWriteTimeout (%v)", st.timeout, sseWriteTimeout)
	}
}
