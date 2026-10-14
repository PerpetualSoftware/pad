package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
// unwraps. Both ends' buffers are shrunk and keepalives sent every
// millisecond, so the buffers fill in well under a second.

// stalledStreamServer starts srv behind a listener whose accepted connections
// have a small send buffer.
func stalledStreamServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	srv.sseKeepaliveOverride = time.Millisecond
	srv.sseWriteTimeoutOverride = 200 * time.Millisecond
	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetWriteBuffer(4096)
			}
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// openStalledStream sends one GET for path over a raw connection, reads the
// response head, and then never reads again. The connection stays open until
// the test ends, so a release cannot come from the client going away.
func openStalledStream(t *testing.T, ts *httptest.Server, path string, header http.Header) {
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
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// assertSlotReleased requires the stream's admission slot to be held first
// (otherwise the release check passes against a handler that never admitted
// the stream) and then released while the client is still connected.
func assertSlotReleased(t *testing.T, srv *Server) {
	t.Helper()
	if held := srv.admission().heldTotal(); held != 1 {
		t.Fatalf("admission slots held once the stream opened = %d, want 1: the slot this test is about was never reserved", held)
	}
	const within = 5 * time.Second
	deadline := time.Now().Add(within)
	for srv.admission().heldTotal() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the admission slot was still held %v after the client stopped reading (write timeout %v): "+
				"a blocked frame write pins the handler", within, srv.sseWriteTimeoutOverride)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSE_BUG3637_StalledReaderReleasesItsSlot(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	ts := stalledStreamServer(t, srv)
	slug := createTestWorkspace(t, ts.URL, "Stalled")

	openStalledStream(t, ts, "/api/v1/events?workspace="+slug, nil)
	assertSlotReleased(t, srv)
}

func TestWatchEvents_BUG3637_StalledReaderReleasesItsSlot(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	ts := stalledStreamServer(t, srv)

	openStalledStream(t, ts, "/api/v1/events/stream", http.Header{"Authorization": {"Bearer " + tok.Token}})
	assertSlotReleased(t, srv)
}
