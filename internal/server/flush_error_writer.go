package server

import (
	"bufio"
	"io"
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
// A writer that does not unwrap, or is not a Flusher, is returned as is: there
// is nowhere to read the error from, or no flush to report on.
//
// The wrapper exposes exactly the optional interfaces w has among Hijacker,
// io.ReaderFrom and Pusher, so a handler's type assertions see what they saw
// before (io.Copy's ReaderFrom fast path, HTTP/2 push; #2186 codex r1). chi's
// writers come in the shapes {Hijacker, ReaderFrom} (HTTP/1), {Hijacker},
// {Pusher} (HTTP/2) and {} (wrap_writer.go), and its compressor is
// {Hijacker, Pusher}.
func withFlushError(w http.ResponseWriter) http.ResponseWriter {
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		return w
	}
	if _, ok := w.(http.Flusher); !ok {
		return w
	}
	f := &flushErrorWriter{ResponseWriter: w, below: u.Unwrap()}
	_, hj := w.(http.Hijacker)
	_, rf := w.(io.ReaderFrom)
	_, ps := w.(http.Pusher)
	switch {
	case hj && rf && !ps:
		return &flushErrorHijackReaderFrom{f}
	case hj && ps && !rf:
		return &flushErrorHijackPusher{f}
	case hj && !rf && !ps:
		return &flushErrorHijacker{f}
	case ps && !hj && !rf:
		return &flushErrorPusher{f}
	case !hj && !rf && !ps:
		return f
	}
	// A shape chi does not produce: leave the writer alone rather than hide
	// one of its interfaces.
	return w
}

// Unwrap returns the chi wrapper, so a ResponseController reaches the
// connection through it (write deadlines, BUG-3637).
func (f *flushErrorWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

func (f *flushErrorWriter) FlushError() error {
	f.ResponseWriter.(http.Flusher).Flush()
	return http.NewResponseController(f.below).Flush()
}

// Flush satisfies http.Flusher for callers that type-assert it.
func (f *flushErrorWriter) Flush() { _ = f.FlushError() }

func (f *flushErrorWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	return f.ResponseWriter.(http.Hijacker).Hijack()
}

func (f *flushErrorWriter) readFrom(r io.Reader) (int64, error) {
	return f.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func (f *flushErrorWriter) push(target string, opts *http.PushOptions) error {
	return f.ResponseWriter.(http.Pusher).Push(target, opts)
}

type flushErrorHijacker struct{ *flushErrorWriter }

func (f *flushErrorHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) { return f.hijack() }

type flushErrorHijackReaderFrom struct{ *flushErrorWriter }

func (f *flushErrorHijackReaderFrom) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return f.hijack()
}
func (f *flushErrorHijackReaderFrom) ReadFrom(r io.Reader) (int64, error) { return f.readFrom(r) }

type flushErrorHijackPusher struct{ *flushErrorWriter }

func (f *flushErrorHijackPusher) Hijack() (net.Conn, *bufio.ReadWriter, error) { return f.hijack() }
func (f *flushErrorHijackPusher) Push(target string, opts *http.PushOptions) error {
	return f.push(target, opts)
}

type flushErrorPusher struct{ *flushErrorWriter }

func (f *flushErrorPusher) Push(target string, opts *http.PushOptions) error {
	return f.push(target, opts)
}
