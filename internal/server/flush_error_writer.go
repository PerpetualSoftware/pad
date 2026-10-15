package server

import (
	"bufio"
	"net"
	"net/http"
)

// flushErrorWriter gives one of chi's response wrappers the FlushError it
// lacks (BUG-3827). chi's WrapResponseWriter (used by the logger, the metrics
// and the MCP audit middleware) and its compressor implement Flush but not
// FlushError, and http.ResponseController.Flush calls Flush when FlushError is
// missing and then returns nil. So a flush to a client that stopped reading
// "succeeded", and a handler that relies on its flushes failing (sseStream's
// Dead, BUG-3637) never learned the stream was gone.
//
// FlushError runs the chi writer's own Flush first, so chi does its
// bookkeeping (the logger's status, the compressor's encoder buffer), and then
// flushes the writer chi wraps through a ResponseController, which returns the
// error. A write failure at the socket is sticky in net/http's buffered
// writer, so that second flush reports it even when chi's flush already hit it
// and dropped it. The second flush is empty on a healthy connection.
type flushErrorWriter struct {
	http.ResponseWriter                     // the chi wrapper
	below               http.ResponseWriter // the writer the chi wrapper wraps
}

// withFlushError wraps w, a chi wrapper that unwraps to the writer beneath it.
// A writer that does not unwrap is returned as is: there is nowhere to read
// the error from.
func withFlushError(w http.ResponseWriter) http.ResponseWriter {
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		return w
	}
	return &flushErrorWriter{ResponseWriter: w, below: u.Unwrap()}
}

// Unwrap returns the chi wrapper, so a ResponseController reaches the
// connection through it (write deadlines, BUG-3637).
func (f *flushErrorWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

func (f *flushErrorWriter) FlushError() error {
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
	return http.NewResponseController(f.below).Flush()
}

// Flush satisfies http.Flusher for callers that type-assert it.
func (f *flushErrorWriter) Flush() { _ = f.FlushError() }

// Hijack is declared rather than left to Unwrap because some callers
// type-assert http.Hijacker instead of using a ResponseController.
func (f *flushErrorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(f.ResponseWriter).Hijack()
}
