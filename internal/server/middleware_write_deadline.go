package server

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Response write deadlines (BUG-3821). http.Server.WriteTimeout is 0 because
// the event streams are long-lived, so before this nothing bounded a response
// write: a handler writing a body larger than the socket buffers to a client
// that had stopped reading blocked in Write until TCP gave up, and a client
// that kept ACKing zero-window probes held it forever. Cancelling the request
// context does not unblock a socket write. Unauthenticated share-link
// attachments were reachable that way.
//
// boundResponseWrites arms a ROLLING deadline before every write and flush,
// the way sseStream does for a frame. Rolling, never an absolute route
// deadline: an export can run for minutes and must keep going while its
// client reads.

// responseWriteTimeout is how long one write or flush may block. A client
// that reads at all resets it; one that reads nothing for this long loses the
// connection.
const responseWriteTimeout = 60 * time.Second

// ownsWriteDeadline reports whether a request's route bounds its own writes,
// so the middleware must leave its connection alone. compressSkips exempts the
// same routes, from this one predicate, so the two cannot drift.
//
// It decides by PATH only, never by a header the client chooses: an Upgrade
// header on an ordinary route (a share-link attachment, say) would otherwise
// opt that response out of its deadline (codex r4). A connection that really
// is upgraded loses whatever was armed when the handler hijacks it, because
// net/http clears every deadline on hijack, so nothing is lost by bounding it
// until then. compressSkips keeps its own Upgrade case.
//
// And the path it reads is the ESCAPED one, the one chi routes on (RawPath
// when set). The decoded path can disagree with the route: an app item ref of
// "x%2Fattachments%2Fy%2Fcontent" routes to the items handler as one segment
// but decodes to a path that looks like the attachment download, which would
// exempt the items response (codex r5). Escaped, it matches nothing here, so
// a spelling the predicate does not recognise is bounded, never exempt.
func ownsWriteDeadline(r *http.Request) bool {
	p := r.URL.EscapedPath()
	switch {
	// SSE (both event endpoints) frames under sseStream (BUG-3637), and MCP's
	// streamable HTTP under its own bound (BUG-3808).
	case p == "/api/v1/events" || strings.HasPrefix(p, "/api/v1/events/"), p == "/mcp" || strings.HasPrefix(p, "/mcp/"):
		return true
	// The collab WebSocket: the connection is hijacked and is not an HTTP
	// response body. collab sets its own deadlines.
	case strings.HasPrefix(p, "/api/v1/collab/"):
		return true
	// An installed app's attachment download sets an ABSOLUTE deadline for the
	// whole stream (appDownloadAttachment), which a rolling one would
	// overwrite on the first write.
	case strings.HasPrefix(p, appAPIPrefix+"/") && strings.Contains(p, "/attachments/") && strings.HasSuffix(p, "/content"):
		return true
	}
	return false
}

// writeDeadlineUnsupported logs, once per process, that a response was served
// without a write deadline.
var writeDeadlineUnsupported sync.Once

// responseWriteTimeout is the constant, or a test's override.
func (s *Server) responseWriteTimeout() time.Duration {
	if s.responseWriteTimeoutOverride > 0 {
		return s.responseWriteTimeoutOverride
	}
	return responseWriteTimeout
}

// boundResponseWrites is the middleware. Register it BEFORE CompressResponses:
// chi runs middleware in registration order, so its writer then sits beneath
// the compressor, next to the socket, and each compressed write is armed.
// Above the compressor it would arm only the writes going in, and a
// compressor buffers.
//
// It arms once before the handler runs, which is also the probe for a writer
// that cannot take a deadline, and once more after the handler returns: a
// body smaller than net/http's buffer reaches the socket only when the server
// flushes it after the handler, which may be long after the last Write.
// net/http clears the deadline after each response and on hijack, so nothing
// armed here outlives the request (TestBUG3821_AKeepAliveConnectionStarts-
// ItsNextRequestWithNoDeadline and TestBUG3821_AHijackedConnectionCarriesNo-
// Deadline hold it to that).
func (s *Server) boundResponseWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ownsWriteDeadline(r) {
			next.ServeHTTP(w, r)
			return
		}
		dw := &deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: s.responseWriteTimeout()}
		if err := dw.arm(); errors.Is(err, http.ErrNotSupported) {
			// A writer that cannot take a deadline (a wrapper without Unwrap)
			// is served unbounded, as before: failing the request would turn
			// a missing bound into an outage.
			writeDeadlineUnsupported.Do(func() {
				slog.Warn("response served without a write deadline: the writer does not support one",
					"path", r.URL.Path, "error", err)
			})
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(dw, r)
		_ = dw.arm()
	})
}

// deadlineWriter re-arms the write deadline before every write and flush,
// until the handler sets one of its own.
type deadlineWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration

	// mu orders arm against the handler's SetWriteDeadline, so an arm from
	// a goroutine that is writing cannot land between the handler's deadline
	// and its ownership being recorded, and replace it (codex r11). It covers
	// the deadline update only, never the write.
	mu sync.Mutex
	// handlerOwned is set once the handler has set its own write deadline.
	handlerOwned bool
}

func (d *deadlineWriter) arm() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handlerOwned {
		return nil
	}
	return d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
}

// SetWriteDeadline is where a handler's http.ResponseController lands: the
// controller stops at the first writer that has the method. A handler that
// sets its own deadline owns it from then on, as it did before this
// middleware existed, and nothing here re-arms over it. Re-arming would
// replace a tighter bound with this one (a handler's 200ms became 60s, and
// its stalled flush blocked for a minute: BUG-3827's chain test), or an
// absolute one with a rolling one. A handler that clears the deadline has
// chosen to write unbounded, and that is its call too. The handler is code
// in this binary, never the client, so this cannot be used to opt a response
// out of its bound the way a request header could (codex r4).
func (d *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.rc.SetWriteDeadline(deadline); err != nil {
		return err
	}
	d.handlerOwned = true
	return nil
}

// Write fails rather than writing unbounded when the deadline cannot be set.
func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.arm(); err != nil {
		return 0, err
	}
	return d.ResponseWriter.Write(p)
}

// FlushError is what http.ResponseController.Flush calls, so a handler that
// flushes through one learns that the flush failed (the deadline expired on a
// client that stopped reading) instead of flushing into a dead connection
// forever. Without it the controller falls back to Flush, which has no error
// to return.
func (d *deadlineWriter) FlushError() error {
	if err := d.arm(); err != nil {
		return err
	}
	return d.rc.Flush()
}

// Flush satisfies http.Flusher, which the compressor and streaming handlers
// type-assert for. It has no error return; FlushError carries the error.
func (d *deadlineWriter) Flush() {
	_ = d.FlushError()
}

// Hijack satisfies http.Hijacker, which gorilla/websocket type-asserts for
// rather than going through http.ResponseController (codex r7). A WebSocket
// route that is not in ownsWriteDeadline would otherwise fail its upgrade
// with a 500. http.Hijacker's contract leaves any deadline on the connection
// to the caller, so the write deadline this middleware armed is cleared here
// before the connection is handed over. (Go's server happens to clear every
// deadline on hijack today, so no test through it can tell this clear from
// that one.) A connection whose deadline cannot be cleared would time out
// under its new owner, so it is closed and the hijack fails (codex r8).
func (d *deadlineWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := d.rc.Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("clear the hijacked connection's write deadline: %w", err)
	}
	return conn, rw, nil
}

// Unwrap lets http.ResponseController and middleware see the writer beneath.
func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
