package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
// response head.
func (f *mcpStallFixture) openGET(t *testing.T, base string, shrink bool) (net.Conn, *bufio.Reader) {
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

func (f *mcpStallFixture) assertUnregisteredWithin(t *testing.T, d time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for f.unregistered.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the GET stream's session was still registered %v after the client stopped reading (write timeout %v): %s",
				d, f.srv.sseWriteTimeoutOverride, why)
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
		name      string
		size      int
		stopAfter time.Duration // 0: keep pumping until the end
	}{
		{"blocked-in-write", 64 << 10, 0},
		{"blocked-in-flush", 200, 1500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMCPStallFixture(t)
			ts := stalledStreamServer(t, f.srv)
			f.openGET(t, ts.URL, true)

			stop := make(chan struct{})
			done := make(chan struct{})
			go func() { defer close(done); f.pump(c.size, stop) }()
			t.Cleanup(func() {
				select {
				case <-stop:
				default:
					close(stop)
				}
				<-done
			})
			if c.stopAfter > 0 {
				time.Sleep(c.stopAfter)
				close(stop)
				<-done
			}
			f.assertUnregisteredWithin(t, 5*time.Second, "a blocked write or flush pins the GET handler")
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
	_, br := f.openGET(t, ts.URL, false)

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
