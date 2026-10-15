package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3808: the /mcp sibling of BUG-3637. mcp-go's GET stream writes and
// flushes with no deadline, so a client that stops reading pinned the
// handler, its session registration and the connection. mcpBoundWrites
// serves the transport through the deadline-arming stream writer and cancels
// the request context when the stream dies.
//
// A real mcp-go transport behind the real router and middleware chain, over
// a real TCP connection (CONVE-19): the deadline reaches the socket only if
// every writer in between unwraps. internal/mcp imports this package, so the
// transport is built here with mcp-go's own options rather than
// NewRemoteTransport.

type mcpStallFixture struct {
	srv          *Server
	mcp          *mcpgo.MCPServer
	token        string
	registered   atomic.Int32
	unregistered atomic.Int32
}

func newMCPStallFixture(t *testing.T) *mcpStallFixture {
	t.Helper()
	f := &mcpStallFixture{srv: testServer(t)}
	user, err := f.srv.store.CreateUser(models.UserCreate{Email: "mcp-stall@example.com", Name: "Stall", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "MCP stall"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.store.CreateAPIToken(user.ID, models.APITokenCreate{Name: "stall", WorkspaceID: ws.ID}, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok.Token

	hooks := &mcpgo.Hooks{}
	hooks.AddOnRegisterSession(func(context.Context, mcpgo.ClientSession) { f.registered.Add(1) })
	hooks.AddOnUnregisterSession(func(context.Context, mcpgo.ClientSession) { f.unregistered.Add(1) })
	f.mcp = mcpgo.NewMCPServer("bug3808", "1", mcpgo.WithHooks(hooks))
	transport := mcpgo.NewStreamableHTTPServer(f.mcp,
		mcpgo.WithEndpointPath("/mcp"),
		mcpgo.WithDisableLocalhostProtection(true),
	)
	f.srv.SetCloudMode("test-secret")
	f.srv.SetMCPTransport(transport, "https://mcp.test.example", "https://app.test.example", nil)
	return f
}

// openGET opens GET /mcp over a raw connection and waits for the session to
// register. It returns the connection and a reader positioned after the
// response head. A non-nil wf watches this connection for a failed write.
func (f *mcpStallFixture) openGET(t *testing.T, base string, shrink bool, wf *writeFailures) (net.Conn, *bufio.Reader) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if wf != nil {
		wf.watch(conn.LocalAddr().String())
	}
	if tc, ok := conn.(*net.TCPConn); ok && shrink {
		_ = tc.SetReadBuffer(4096)
	}
	req := "GET /mcp HTTP/1.1\r\nHost: " + u.Host + "\r\nAccept: text/event-stream\r\nAuthorization: Bearer " + f.token + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
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
	deadline := time.Now().Add(5 * time.Second)
	for f.registered.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the GET stream's session never registered (%d)", f.registered.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return conn, br
}

// pump broadcasts notifications whose payload is size bytes, one per
// millisecond, until stop closes. A full session channel drops the
// notification, as mcp-go does.
func (f *mcpStallFixture) pump(size int, stop <-chan struct{}) {
	payload := strings.Repeat("x", size)
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Millisecond):
			f.mcp.SendNotificationToAllClients("notifications/message", map[string]any{"data": payload})
		}
	}
}

// assertUnregistered waits for the server's write to the stalled client to
// fail on its deadline, then requires the GET stream's session unregistered
// within stallReleaseMargin. The two phases are timed apart, as
// assertSlotReleased does (BUG-3824): filling the buffers is the harness and
// takes as long as the scheduler lets it; the release is the code under test.
func (f *mcpStallFixture) assertUnregistered(t *testing.T, wf *writeFailures) {
	t.Helper()
	select {
	case <-wf.failed:
	case <-time.After(stallFillCeiling):
		t.Fatalf("no write to the stalled client failed within %v (write timeout %v): "+
			"a blocked write or flush pins the GET handler, or the buffers never filled", stallFillCeiling, f.srv.sseWriteTimeoutOverride)
	}
	if !errors.Is(wf.err, os.ErrDeadlineExceeded) {
		t.Fatalf("the first failed write returned %v, want a write deadline overrun", wf.err)
	}
	deadline := wf.at.Add(stallReleaseMargin)
	for f.unregistered.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the GET stream's session was still registered %v after a write overran its deadline: the handler did not return", stallReleaseMargin)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two frame sizes, so the blocked call is a Write (large frames overflow
// net/http's buffer inside Write) or a Flush (small ones never do). Either
// way the deadline fails it. A failed Write ends the library's loop on the
// error; a failed Flush reports nothing, and here the frames still queued in
// mcp-go's channels (16 each) make the next Write fail on the stream's
// sticky error. The case with nothing queued after a failed Flush is
// TestMCP_BUG3808_DeadStreamCancelsTheRequestContext.
func TestMCP_BUG3808_StalledReaderReleasesTheGETStream(t *testing.T) {
	for _, c := range []struct {
		name       string
		size       int
		stopOnFail bool // stop pumping once a write fails
	}{
		{"blocked-in-write", 64 << 10, false},
		{"blocked-in-flush", 200, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMCPStallFixture(t)
			ts, wf := stalledStreamServer(t, f.srv)
			f.openGET(t, ts.URL, true, wf)

			stop := make(chan struct{})
			done := make(chan struct{})
			go func() { defer close(done); f.pump(c.size, stop) }()
			var stopOnce sync.Once
			closeStop := func() { stopOnce.Do(func() { close(stop) }) }
			t.Cleanup(func() {
				closeStop()
				<-done
			})
			if c.stopOnFail {
				go func() {
					<-wf.failed
					closeStop()
				}()
			}
			f.assertUnregistered(t, wf)
		})
	}
}

// CONTROL: a client that keeps reading keeps its stream, for many times the
// write timeout, so the release above is the stall and not the bound firing
// on a healthy reader. Same bound, ordinary socket buffers: the shrunk ones
// above are a device for stalling fast, and on loopback they sit below the
// MSS, so even a reading client's transfer crawls at the persist timer.
func TestMCP_BUG3808_ReadingClientKeepsTheGETStream(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.sseWriteTimeoutOverride = 200 * time.Millisecond
	ts := httptest.NewServer(f.srv)
	t.Cleanup(ts.Close)
	_, br := f.openGET(t, ts.URL, false, nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); f.pump(64<<10, stop) }()
	defer func() { close(stop); <-done }()

	var read atomic.Int64
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := br.Read(buf)
			read.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(10 * f.srv.sseWriteTimeoutOverride)
	if n := f.unregistered.Load(); n != 0 {
		t.Fatalf("a reading client's stream ended (unregistered=%d) after %d bytes", n, read.Load())
	}
	if read.Load() < 1<<20 {
		t.Fatalf("instrument: the reader received only %d bytes, so the stream was not carrying load", read.Load())
	}
}

// A writer that cannot take a write deadline is refused, on every method,
// before the transport runs: its writes could not be bounded.
func TestMCP_BUG3808_WriterWithoutDeadlineIsRefused(t *testing.T) {
	f := newMCPStallFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mk := func(ctx context.Context) *http.Request {
			req := httptest.NewRequest(method, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.RemoteAddr = "192.0.2.1:1234"
			return req
		}
		// httptest.ResponseRecorder is a Flusher with no write deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rr := httptest.NewRecorder()
		f.srv.ServeHTTP(rr, mk(ctx))
		cancel()
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("%s through a writer without deadlines: %d %q, want 500", method, rr.Code, rr.Body.String())
		}
		// CONTROL: the same request through a writer that takes one reaches
		// the transport.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
		dr := newDeadlineRecorder()
		f.srv.ServeHTTP(dr, mk(ctx2))
		cancel2()
		if dr.Code == http.StatusInternalServerError {
			t.Errorf("control: %s through a deadline writer: 500 %q", method, dr.Body.String())
		}
	}
	if n := f.registered.Load(); n != 1 {
		t.Errorf("sessions registered = %d, want 1 (the control GET only): a refused request reached the transport", n)
	}
}

// flushFailingWriter takes a write deadline and fails every flush, the state
// of a stream whose last frame stalled in Flush past the deadline.
type flushFailingWriter struct{ *httptest.ResponseRecorder }

func (flushFailingWriter) SetWriteDeadline(time.Time) error { return nil }
func (flushFailingWriter) FlushError() error                { return errors.New("flush: i/o timeout") }

// A failed Flush returns no error to the library, and with nothing queued its
// loop waits on its channels and ctx.Done() only. mcpBoundWrites must cancel
// the request context when the stream dies, or that handler waits for a
// notification that may never come.
func TestMCP_BUG3808_DeadStreamCancelsTheRequestContext(t *testing.T) {
	srv := testServer(t)
	cancelled := make(chan bool, 1)
	h := srv.mcpBoundWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			cancelled <- true
		case <-time.After(5 * time.Second):
			cancelled <- false
		}
	}))
	h.ServeHTTP(flushFailingWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if !<-cancelled {
		t.Fatal("the request context was not cancelled after the stream's flush failed: a handler waiting on it is pinned")
	}
}

// Both MCP mounts serve the same mcp-go transport type, so both are bounded:
// each refuses a writer without a write deadline before its transport runs,
// and reaches it through one that has (control).
func TestMCP_BUG3808_BothMountsAreBounded(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	sess := newOAuthSession(t, srv)
	mcpTok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	gptTok, _ := mintWithResource(t, srv, sess, testChatGPTResource)
	for _, c := range []struct{ path, token, transport string }{
		{"/mcp", mcpTok, "mcp"},
		{"/mcp/chatgpt", gptTok, "chatgpt"},
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError || rr.Body.String() == c.transport {
			t.Errorf("%s through a writer without deadlines: %d %q, want 500 before the transport", c.path, rr.Code, rr.Body.String())
		}
		if ok := postMCP(srv, c.path, c.token); ok.Code != http.StatusOK || ok.Body.String() != c.transport {
			t.Errorf("control: %s through a deadline writer: %d %q, want 200 from the %s transport", c.path, ok.Code, ok.Body.String(), c.transport)
		}
	}
}

// A DELETE answers with WriteHeader alone, so its response is committed by
// net/http after the transport returns, outside every Write and Flush the
// stream bounds. Pipelined DELETEs on one connection whose client never
// reads fill the socket with those header-only responses: without a deadline
// armed for that final write, the server blocks on it with nothing to end it,
// and the connection never closes.
func TestMCP_BUG3808_StalledReaderReleasesAHeaderOnlyDELETE(t *testing.T) {
	f := newMCPStallFixture(t)
	f.srv.sseWriteTimeoutOverride = 200 * time.Millisecond
	// The per-token MCP limiter would answer the later DELETEs with a 429
	// written by middleware, outside the stream, and the stall would land on
	// that response instead of the one under test.
	f.srv.rateLimiters = nil

	// CONTROL: a DELETE through the real transport answers a header-only 200,
	// so the pipelined responses below are that path and not a refusal.
	dr := newDeadlineRecorder()
	ctl := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	ctl.Header.Set("Authorization", "Bearer "+f.token)
	ctl.RemoteAddr = "192.0.2.1:1234"
	f.srv.ServeHTTP(dr, ctl)
	if dr.Code != http.StatusOK || dr.Body.Len() != 0 {
		t.Fatalf("control DELETE: %d with %d body bytes, want a header-only 200", dr.Code, dr.Body.Len())
	}

	var served atomic.Int32
	closed := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		f.srv.ServeHTTP(w, r)
	}))
	wf := &writeFailures{failed: make(chan struct{})}
	ts.Listener = &writeFailureListener{Listener: ts.Listener, wf: wf} // 4KB send buffer
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			close(closed)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	wf.watch(conn.LocalAddr().String())
	_ = conn.(*net.TCPConn).SetReadBuffer(4096)
	req := "DELETE /mcp HTTP/1.1\r\nHost: " + u.Host + "\r\nAuthorization: Bearer " + f.token + "\r\n\r\n"
	const pipelined = 400
	go func() { _, _ = conn.Write([]byte(strings.Repeat(req, pipelined))) }()

	// Timed in two phases, as assertSlotReleased is (BUG-3824): the buffers
	// fill as fast as the scheduler serves the requests, then the close is
	// the code under test.
	select {
	case <-wf.failed:
	case <-time.After(stallFillCeiling):
		t.Fatalf("no write to the stalled client failed within %v (%d of %d DELETEs served, write timeout %v): the header-only response's write is unbounded",
			stallFillCeiling, served.Load(), pipelined, f.srv.sseWriteTimeoutOverride)
	}
	if !errors.Is(wf.err, os.ErrDeadlineExceeded) {
		t.Fatalf("the first failed write returned %v, want a write deadline overrun", wf.err)
	}
	select {
	case <-closed:
	case <-time.After(time.Until(wf.at.Add(stallReleaseMargin))):
		t.Fatalf("the connection was still open %v after a write overran its deadline", stallReleaseMargin)
	}
	// Instrument: the stall came before the client's requests ran out, so the
	// close is the deadline firing on a blocked write, not the end of input.
	if n := served.Load(); n >= pipelined {
		t.Fatalf("instrument: all %d DELETEs were served, so no response blocked", n)
	}
}
