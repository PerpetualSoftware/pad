package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// BUG-3835: the MCP transport's long-lived streams had no keepalive and no
// admission budget. An idle stream makes no write, so #2182's write deadline
// (BUG-3808) never fires on it: a client that stopped reading but held TCP
// open pinned a handler goroutine and its session until a notification
// happened to be written, and a signed-in client could open any number.
//
// Two parts, both in mcpBoundWrites:
//
//   - A ': keepalive' comment, at the /events cadence, on any response the
//     transport commits as 200 text/event-stream. The write goes through the
//     same bounded writer, so a stream whose peer is gone now ends within a
//     keepalive or two once the connection is known dead: after a reset (the
//     first write after one can still succeed), or once TCP gives up on a
//     half-open peer (hunter r4). A small
//     comment never blocks long enough for the write deadline to be the
//     detector, so that bound is not "one keepalive plus the write deadline"
//     except in the shrunk-buffer tests. A live client that holds
//     TCP open and stops reading is NOT caught this way with default socket
//     buffers: a comment per interval takes days to fill them, so no write
//     blocks (hunter r3; /events is the same). The admission budget below is
//     what bounds that client. mcp-go's own heartbeat is not used: it sends
//     JSON-RPC ping REQUESTS every client must answer.
//   - The shared stream admission gate (stream_admission.go), taken at request
//     entry for the two long-lived doors only: the legacy GET stream and
//     subscriptions/listen. A listen is looked for only on a POST whose protocol
//     header is a modern version the transport serves (mcpLongLivedRequest).
//     Pad's transport serves only the legacy versions today, so no listen is
//     counted and no POST body is read. A tools/call POST that mcp-go upgrades to SSE is
//     not counted: it upgrades mid-call, and a refusal there would land after
//     the call's side effects, so the client's retry would repeat them.

// mcpStreamWriter serializes mcp-go's writes with the keepalive, and tracks
// whether the response became an event stream and whether its final event
// has been written.
//
// Interleaving is safe at Write boundaries only because mcp-go writes each
// SSE event with ONE Write (writeSSEEvent's single fmt.Fprintf): a keepalive
// taken under mu can land between two events, never inside one. mcp-go's
// writeSSEEventRaw writes an event in TWO (its id: line, then the event), but
// only on the event-store (resumption) path, and Pad configures no event
// store. Configuring one (WithEventStore) breaks this premise: a keepalive
// could land between the two halves.
type mcpStreamWriter struct {
	stream *sseStream

	mu          sync.Mutex
	wroteHeader bool
	ended       bool          // final event written, or end() ran after the transport returned
	committed   chan struct{} // closed when the response is committed as 200 text/event-stream
}

func newMCPStreamWriter(stream *sseStream) *mcpStreamWriter {
	return &mcpStreamWriter{stream: stream, committed: make(chan struct{})}
}

func (w *mcpStreamWriter) Header() http.Header { return w.stream.Header() }

// Unwrap lets http.ResponseController reach the deadline-capable writer.
func (w *mcpStreamWriter) Unwrap() http.ResponseWriter { return w.stream }

func (w *mcpStreamWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeHeaderLocked(code)
}

func (w *mcpStreamWriter) writeHeaderLocked(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if code == http.StatusOK && isEventStream(w.stream.Header().Get("Content-Type")) {
			close(w.committed)
		}
	}
	w.stream.WriteHeader(code)
}

func (w *mcpStreamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.writeHeaderLocked(http.StatusOK)
	}
	n, err := w.stream.Write(p)
	if err == nil && isFinalSSEEvent(p) {
		// A JSON-RPC response ends a POST's stream, so no keepalive is
		// written after it in the gap before the handler returns. Only a
		// write that STARTS with the response is recognised: a write that
		// carries a notification first and the response after it (one
		// buffered replay) is not, and the stream may then get one more
		// comment before end(), which is valid SSE (r7 hunter L3).
		w.ended = true
	}
	return n, err
}

func (w *mcpStreamWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stream.Flush()
}

// keepalive writes one comment frame, and reports whether the stream can
// take another. It writes nothing once the stream has ended or failed.
func (w *mcpStreamWriter) keepalive() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended || w.stream.Err() != nil {
		return false
	}
	if _, err := w.stream.Write([]byte(": keepalive\n\n")); err != nil {
		return false
	}
	w.stream.Flush()
	return w.stream.Err() == nil
}

// end stops any further keepalive. Called once the transport returns, before
// mcpBoundWrites joins the keepalive goroutine and itself returns. It is not
// atomic with that return: a tick that takes mu between the two writes one
// more comment frame first, which is valid SSE on a response that is ending
// (Codex r1). Nothing is written after mcpBoundWrites returns.
func (w *mcpStreamWriter) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = true
}

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// sseEventPrefix is how writeSSEEvent opens every event.
var sseEventPrefix = []byte("event: message\ndata: ")

// isFinalSSEEvent reports whether p is one SSE event carrying a JSON-RPC
// response: an id and no method. Notifications and server-to-client
// requests carry a method; a response is the last thing a POST's stream says.
func isFinalSSEEvent(p []byte) bool {
	rest, ok := bytes.CutPrefix(p, sseEventPrefix)
	if !ok {
		return false
	}
	data, _, ok := bytes.Cut(rest, []byte("\n"))
	if !ok {
		return false
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return false
	}
	return msg.Method == "" && len(msg.ID) > 0
}

// mcpKeepaliveEvery is the /events keepalive cadence, or a test's override.
func (s *Server) mcpKeepaliveEvery() time.Duration {
	if s.sseKeepaliveOverride > 0 {
		return s.sseKeepaliveOverride
	}
	return sseKeepaliveInterval
}

// runMCPKeepalive writes keepalives once w is committed as an event stream,
// until stop is closed or a write fails.
func (s *Server) runMCPKeepalive(w *mcpStreamWriter, stop <-chan struct{}) {
	select {
	case <-w.committed:
	case <-stop:
		return
	}
	t := time.NewTicker(s.mcpKeepaliveEvery())
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !w.keepalive() {
				return
			}
		case <-stop:
			return
		}
	}
}

// mcpProtocolVersions is what mcp-go's StreamableHTTPServer exposes about the
// protocol versions it serves, and what admission keys the listen door on.
type mcpProtocolVersions interface {
	SupportsProtocolVersion(version string) bool
}

// mcpServedVersion reports whether the transport behind a mount serves
// version. A transport that does not say (a test stub, a wrapper that does not
// forward the method) is taken to serve every version, which over-counts and
// never lets a served listen skip the budget.
func mcpServedVersion(transport http.Handler) func(string) bool {
	if v, ok := transport.(mcpProtocolVersions); ok {
		return v.SupportsProtocolVersion
	}
	return func(string) bool { return true }
}

// MCPMountServesVersion is the stream admission gate's answer to "does the
// transport wired for this mount serve version": /mcp/chatgpt when chatgpt is
// true, /mcp otherwise. It reads the transport the server holds, the one the
// route is built from, so cmd/pad can check the binding as `pad server` wires
// it rather than a transport its test constructs (BUG-3835 r8 L2, CONVE-19).
// An unwired mount serves nothing.
func (s *Server) MCPMountServesVersion(chatgpt bool, version string) bool {
	transport := s.mcpTransport
	if chatgpt {
		transport = s.chatGPTTransport
	}
	if transport == nil {
		return false
	}
	return mcpServedVersion(transport)(version)
}

// mcpLongLivedRequest reports whether r opens one of the transport's
// long-lived streams: the legacy GET stream, or a subscriptions/listen POST.
// served reports whether the mount's transport serves a protocol version.
//
// A listen is served only in the modern protocol era, and mcp-go serves a
// modern POST only when its Mcp-Protocol-Version header is a modern version
// that equals the body's _meta version and that it serves
// (validateModernRequest); a legacy-era listen is answered METHOD_NOT_FOUND at
// once, and an unserved version gets its 400. So a POST whose header is not a
// served modern version is never a listen: its body is not read here, and it
// reaches mcp-go, whose 400 is what tells a modern client to fall back (r7
// ruling: a 429 there would keep a probing client from ever falling back).
// Pad serves the legacy versions only (mcp.ServedProtocolVersions), so today
// no POST body is read for admission (r6 hunter M1).
//
// When the header is modern, the whole body is read to decide it, and
// replayed to the transport. Not a fixed-size peek: JSON key order is free,
// so "method" may follow a large "params", and a peek would let a listen skip
// the budget. The body is already bounded by limitMCPBody, which runs before
// this.
func mcpLongLivedRequest(r *http.Request, served func(string) bool) bool {
	switch r.Method {
	case http.MethodGet:
		return true
	case http.MethodPost:
	default:
		return false
	}
	if v := r.Header.Get(mcp.HeaderProtocolVersion); !mcp.IsModernProtocol(v) || !served(v) {
		return false
	}
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		// The transport answers a body it cannot read with its own error, so
		// hand it the same failure (limitMCPBody does the same).
		r.Body = replayBody{io.MultiReader(bytes.NewReader(buf), errReader{err}), r.Body}
		return false
	}
	r.Body = replayBody{bytes.NewReader(buf), r.Body}
	return jsonRPCHasMethod(buf, string(mcp.MethodSubscriptionsListen))
}

// jsonRPCHasMethod reports whether body names method, decoded EXACTLY as
// mcp-go's handlePost decodes it: encoding/json into a struct with a "method"
// field (streamable_http.go handlePost, request_handler.go HandleMessage).
// Not a hand scanner: encoding/json matches keys case-insensitively and lets
// the last duplicate win, and any disagreement with the transport would let a
// listen through uncounted. An array body fails this decode, as it fails
// mcp-go's, which rejects it without serving anything.
func jsonRPCHasMethod(body []byte, method string) bool {
	var msg struct {
		Method mcp.MCPMethod `json:"method"`
	}
	return json.Unmarshal(body, &msg) == nil && string(msg.Method) == method
}

// admitMCPStream takes a stream admission slot for a long-lived MCP request,
// or answers the refusal. The returned release is nil when nothing was taken.
// ok is false when the request was refused and must not be served.
func (s *Server) admitMCPStream(w http.ResponseWriter, r *http.Request, served func(string) bool) (release func(), ok bool) {
	if !mcpLongLivedRequest(r, served) {
		return nil, true
	}
	principal := streamPrincipal(r, "")
	release, refusal := s.admission().acquire(principal)
	if refusal != admissionRefusalNone {
		total, user := s.admission().counts(principal)
		slog.Warn("MCP stream connection limit reached", "method", r.Method,
			"refused_by", string(refusal), "total", total, "user_count", user)
		writeStreamLimitExceeded(w)
		return nil, false
	}
	return release, true
}
