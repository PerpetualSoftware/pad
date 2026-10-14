package server

import (
	"errors"
	"log/slog"
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
func ownsWriteDeadline(r *http.Request) bool {
	p := r.URL.Path
	switch {
	// SSE (both event endpoints) frames under sseStream (BUG-3637), and MCP's
	// streamable HTTP under its own bound (BUG-3808).
	case strings.HasPrefix(p, "/api/v1/events"), p == "/mcp" || strings.HasPrefix(p, "/mcp/"):
		return true
	// The collab WebSocket, and any other upgrade: the connection is hijacked
	// and is not an HTTP response body. collab sets its own deadlines.
	case strings.HasPrefix(p, "/api/v1/collab/"), r.Header.Get("Upgrade") != "":
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
// armed here outlives the request (TestBUG3821_AConnectionCarriesNoDeadline-
// IntoItsNextRequestOrAHijack holds it to that).
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

// deadlineWriter re-arms the write deadline before every write and flush.
type deadlineWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
}

func (d *deadlineWriter) arm() error {
	return d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
}

// Write fails rather than writing unbounded when the deadline cannot be set.
func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.arm(); err != nil {
		return 0, err
	}
	return d.ResponseWriter.Write(p)
}

// Flush satisfies http.Flusher, which the compressor and streaming handlers
// type-assert for.
func (d *deadlineWriter) Flush() {
	if d.arm() != nil {
		return
	}
	_ = d.rc.Flush()
}

// Unwrap lets http.ResponseController and middleware see the writer beneath.
func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
