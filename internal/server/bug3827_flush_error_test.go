package server

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/metrics"
)

// BUG-3827: chi's WrapResponseWriter (logger, metrics, MCP audit) and its
// compressor implement Flush but not FlushError, so a ResponseController
// flush through them returned nil when the socket write failed. These pins
// stall a client and require the flush error to reach the handler.

// flushUntilError is a handler that writes and flushes through a
// ResponseController until a flush fails, and reports that error (or nil
// once giveUp passes with every flush succeeding).
func flushUntilError(contentType string, got chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		rc := http.NewResponseController(w)
		chunk := []byte(strings.Repeat("x", 1024) + "\n")
		giveUp := time.Now().Add(10 * time.Second)
		for time.Now().Before(giveUp) {
			_, _ = w.Write(chunk) // unchecked on purpose: the flush is under test
			if err := rc.Flush(); err != nil {
				got <- err
				return
			}
		}
		got <- nil
	}
}

func requireFlushError(t *testing.T, got <-chan error) {
	t.Helper()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("every flush to a client that stopped reading succeeded for 10s: the flush error was dropped on the way up")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the flush failed with %v, want the write deadline overrun", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the flushing handler never returned")
	}
}

// Through the router's whole outer chain (logger, metrics, recoverer,
// compressor), uncompressed and gzipped. The handler arms a write deadline
// through the same controller, as the event streams do, so a stalled write
// fails instead of blocking.
func TestBUG3827_AStalledFlushReturnsAnErrorThroughTheChain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		header      http.Header
		encoding    string // the Content-Encoding the response must carry
	}{
		{"identity", "", nil, ""},
		{"gzip", "text/plain", http.Header{"Accept-Encoding": {"gzip"}}, "gzip"},
		{"deflate", "text/plain", http.Header{"Accept-Encoding": {"deflate"}}, "deflate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testServer(t)
			srv.metrics = metrics.New()
			srv.ensureRouter()
			got := make(chan error, 1)
			flush := flushUntilError(tc.contentType, got)
			srv.router.Get("/__bug3827/flusher", func(w http.ResponseWriter, r *http.Request) {
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
				deadlined := &rearmingWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
				flush(deadlined, r)
			})
			ts, wf := stalledStreamServer(t, srv)
			head := openStalledStream(t, ts, wf, "/__bug3827/flusher", tc.header)
			if enc := head.Get("Content-Encoding"); enc != tc.encoding {
				t.Fatalf("Content-Encoding %q, want %q: the leg did not go through the encoder it names", enc, tc.encoding)
			}
			requireFlushError(t, got)
		})
	}
}

// rearmingWriter arms a fresh 200ms write deadline before every write and
// flush, so a stalled client fails the next one instead of blocking it.
type rearmingWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w *rearmingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *rearmingWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	return w.ResponseWriter.Write(p)
}

func (w *rearmingWriter) FlushError() error {
	_ = w.rc.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	return w.rc.Flush()
}

// The activity stream itself: its keepalive flush is the write that fails,
// and that failure now reaches the stream (Dead, Err) instead of being
// dropped by the chain while the request's context ending did the work.
func TestBUG3827_EventStreamSeesItsFailedFlush(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	srv.metrics = metrics.New()
	failed := make(chan error, 1)
	srv.sseStreamFailedForTesting = func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	ts, wf := stalledStreamServer(t, srv)
	slug := createTestWorkspace(t, ts.URL, "FlushErr")
	openStalledStream(t, ts, wf, "/api/v1/events?workspace="+slug, nil)
	select {
	case err := <-failed:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the stream failed with %v, want the write deadline overrun", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("no write or flush failure reached the event stream within 60s of its client stalling")
	}
}

// The MCP audit middleware's wrapper, which every /mcp request goes through
// (the GET stream included), passes the flush error up.
func TestBUG3827_MCPAuditLogPassesFlushErrors(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	// Without an audit writer MCPAuditLog passes the request straight
	// through, and the handler gets net/http's own writer, which has
	// FlushError: the pin would pass with the fix removed.
	srv.startMCPAuditWriter()
	got := make(chan error, 1)
	flush := flushUntilError("", got)
	h := srv.MCPAuditLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flush(&rearmingWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r)
	}))
	ts := httptest.NewUnstartedServer(h)
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetWriteBuffer(4096)
			}
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	openStalledStream(t, ts, nil, "/mcp", nil)
	requireFlushError(t, got)
}
