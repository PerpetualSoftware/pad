package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
)

// defaultResponseMaxBytes caps the buffered response of a single MCP tool or
// resource call. The largest real full-body list measured on a
// production-sized workspace was 1,860,321 bytes (a docs collection, up to
// 300 items with full bodies); 32 MiB is about 17x that. It bounds one call's
// buffer; it does not bound how many calls run at once (that is the rate
// limiter's job).
const defaultResponseMaxBytes int64 = 32 << 20

// stderrMaxBytes caps subprocess stderr retained for error classification.
// Stderr beyond this is truncated silently (it only becomes error text),
// never an error by itself.
const stderrMaxBytes int64 = 1 << 20

func (d *HTTPHandlerDispatcher) responseMaxBytes() int64 {
	if d != nil && d.ResponseMaxBytes > 0 {
		return d.ResponseMaxBytes
	}
	return defaultResponseMaxBytes
}

func (d *ExecDispatcher) responseMaxBytes() int64 {
	if d != nil && d.ResponseMaxBytes > 0 {
		return d.ResponseMaxBytes
	}
	return defaultResponseMaxBytes
}

// serveCapped runs d.Handler.ServeHTTP into a response writer bounded at
// d.responseMaxBytes() and returns an *http.Response over the retained bytes
// along with whether the response body overran the limit.
func (d *HTTPHandlerDispatcher) serveCapped(req *http.Request) (resp *http.Response, exceeded bool) {
	return d.serveCappedWithLimit(req, d.responseMaxBytes())
}

func (d *HTTPHandlerDispatcher) serveCappedWithLimit(req *http.Request, limit int64) (resp *http.Response, exceeded bool) {
	w := newCappedResponseWriter(limit)
	d.Handler.ServeHTTP(w, req)
	status := w.status
	if !w.wroteHdr {
		status = http.StatusOK
	}
	body := w.body.Bytes()
	resp = &http.Response{
		StatusCode: status,
		Header:     w.Header(),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
	return resp, w.exceededCap()
}

// runCapped executes cmd with stdout capped at stdoutLimit and stderr capped
// at stderrMaxBytes. Both writers keep draining (discarding) past their limits
// so the child never blocks on a full pipe. exceeded reports whether stdout
// overran stdoutLimit.
func runCapped(cmd *exec.Cmd, stdoutLimit int64) (stdout []byte, stderr string, exceeded bool, err error) {
	if stdoutLimit <= 0 {
		stdoutLimit = defaultResponseMaxBytes
	}
	var outBuf, errBuf bytes.Buffer
	outCap := &cappedWriter{buf: &outBuf, limit: stdoutLimit}
	errCap := &cappedWriter{buf: &errBuf, limit: stderrMaxBytes}
	cmd.Stdout = outCap
	cmd.Stderr = errCap
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.String(), outCap.exceeded, err
}

func responseTooLargeResult(cmdKey string, limit int64) *CallToolResult {
	details, _ := json.Marshal(map[string]any{
		"reason":      "response_too_large",
		"limit_bytes": limit,
	})
	return NewErrorResult(ErrorPayload{
		Code:    ErrTooLarge,
		Message: fmt.Sprintf("pad %s: the result is larger than the %d-byte MCP response limit", cmdKey, limit),
		Hint:    "Ask for less: lower limit, drop full=true, or add filters.",
		Details: details,
	})
}

// overflowResult answers a call whose response overran the limit. A write
// (any method but GET or HEAD) that the handler accepted has already
// happened, so it is reported as done with its result left out: a too_large
// error there would invite a retry that writes twice (a duplicate create or
// import). Everything else is refused with too_large.
func overflowResult(cmdKey string, req *http.Request, status int, limit int64) *CallToolResult {
	if status < 400 && req != nil && req.Method != http.MethodGet && req.Method != http.MethodHead {
		msg := fmt.Sprintf("pad %s succeeded; its result is larger than the %d-byte MCP response limit and was not returned. Read the item to see it.", cmdKey, limit)
		return structuredResult(map[string]any{
			"ok":             true,
			"result_omitted": true,
			"reason":         "response_too_large",
			"limit_bytes":    limit,
			"message":        msg,
		}, msg)
	}
	return responseTooLargeResult(cmdKey, limit)
}

// responseTooLargeError propagates a response-cap overflow from internal
// prefetch helpers (such as resolveItemRef or lookupAssigneeID) back to the
// outer command dispatcher so it returns responseTooLargeResult for the outer
// command.
type responseTooLargeError struct {
	limit int64
}

func (e *responseTooLargeError) Error() string {
	return fmt.Sprintf("response exceeded %d-byte cap", e.limit)
}

func tooLargeResultIfExceeded(cmdKey string, err error) *CallToolResult {
	var tooLarge *responseTooLargeError
	if errors.As(err, &tooLarge) {
		return responseTooLargeResult(cmdKey, tooLarge.limit)
	}
	return nil
}

// cappedWriter buffers into buf until limit bytes, then keeps accepting
// (and silently discarding) further writes so io.Copy keeps draining the
// subprocess pipe — the child can't wedge on a full pipe — while memory
// stays bounded at limit. exceeded records whether anything was dropped.
type cappedWriter struct {
	buf      *bytes.Buffer
	limit    int64
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if room := c.limit - int64(c.buf.Len()); room < int64(len(p)) {
		c.exceeded = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

// cappedResponseWriter is an http.ResponseWriter that retains at most
// `limit` bytes of the response body (silently discarding the rest so the
// handler's io.Copy keeps draining) and records whether it overflowed. It
// reuses cappedWriter for the byte-capping algorithm and adds the
// http.ResponseWriter shims so the cap logic lives in one place.
type cappedResponseWriter struct {
	header   http.Header
	status   int
	wroteHdr bool
	body     bytes.Buffer
	capped   *cappedWriter
}

// newCappedResponseWriter wires the writer's body buffer into a cappedWriter
// bounded at limit. Must be used (not a bare struct literal) so `capped` is
// non-nil before the first Write.
func newCappedResponseWriter(limit int64) *cappedResponseWriter {
	w := &cappedResponseWriter{}
	w.capped = &cappedWriter{buf: &w.body, limit: limit}
	return w
}

func (w *cappedResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *cappedResponseWriter) WriteHeader(status int) {
	if !w.wroteHdr {
		w.status = status
		w.wroteHdr = true
	}
}

func (w *cappedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHdr {
		w.WriteHeader(http.StatusOK)
	}
	return w.capped.Write(p)
}

// exceededCap reports whether the response body overran the byte limit.
func (w *cappedResponseWriter) exceededCap() bool { return w.capped.exceeded }
