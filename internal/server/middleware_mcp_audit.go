package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// MCP audit log middleware (PLAN-943 TASK-960).
//
// Wraps the /mcp Streamable HTTP handler AFTER MCPBearerAuth runs, so
// the audit row carries the resolved user + token identity stashed on
// context. Records one row per HTTP request hitting /mcp:
//
//   - Successful tool calls (200 OK with a JSON-RPC result envelope).
//   - Tool calls that the inner handler rejected (200 OK with an error
//     envelope — JSON-RPC errors are still HTTP 200 per spec).
//   - Authorization gate misses (401 / 403).
//   - Server errors (5xx).
//
// Writes are async via a buffered channel + worker goroutine. The
// hot path captures the HTTP-level outcome, builds an MCPAuditEntry,
// and tries a non-blocking channel send; if the buffer is full
// (DB outage / writer too slow), the entry is dropped and the
// mcp_audit_dropped_total metric increments. The request never
// blocks — the spec is "audit is best-effort, /mcp latency is not."
//
// Body sniffing:
//
// To populate tool_name + args_hash without consuming the body that
// the inner JSON-RPC handler needs, we wrap r.Body in a tee that
// buffers up to mcpAuditBodyMaxBytes. The first frame is parsed
// after-the-fact (post-handler) because Streamable HTTP can carry
// either a single JSON-RPC envelope or an SSE stream — we only sniff
// the request body, not the response. tool_name defaults to "(unknown)"
// when the body wasn't a JSON-RPC envelope or was over the cap; that's
// rare and intentionally ugly so it stands out in audit reviews.

const (
	// mcpAuditBufferSize bounds the in-flight queue between the
	// hot-path producer and the disk-bound writer. Sized at 4096:
	// at our worst-case bursting rate (a few hundred req/s per node
	// during a tool-call cascade) this absorbs ~10–30 seconds of
	// backpressure before the drop path fires. Memory cost ~1MB at
	// full saturation (each entry ≈ 256B), which is cheap for the
	// resilience win.
	mcpAuditBufferSize = 4096

	// mcpAuditBodyMaxBytes caps how much of a request body we'll buffer
	// for tool_name / args_hash extraction. Tool args can be large
	// (item content posts), but we only need the JSON-RPC method +
	// params.name + params.arguments — typically <2KB. Anything past
	// the cap is opaque to audit and the hash collapses to "" rather
	// than partial data, which would produce misleading "matching"
	// hashes for unrelated calls.
	mcpAuditBodyMaxBytes = 64 * 1024

	// mcpAuditRetention is the age cutoff for the periodic sweep.
	// 90 days per TASK-960's spec.
	mcpAuditRetention = 90 * 24 * time.Hour

	// mcpAuditSweepInterval is how often the retention sweeper runs.
	// 24h is overkill for most deploys (audit growth is bounded by
	// MCP traffic) but keeps the table hot-set bounded after a
	// burst.
	mcpAuditSweepInterval = 24 * time.Hour

	// mcpAuditWriteTimeout caps how long a single InsertMCPAuditEntry
	// can block the writer goroutine. Longer than the SQLite busy
	// timeout (30s) so a rare contention spike doesn't drop the row,
	// but bounded so a runaway DB doesn't pin the writer forever.
	mcpAuditWriteTimeout = 60 * time.Second
)

// mcpAuditWriter owns the buffered queue + worker goroutine that
// drains it into the store. One per Server. Started in
// startMCPAuditWriter; stopped in Server.Stop via the bg WaitGroup
// + a stop channel.
type mcpAuditWriter struct {
	store   *store.Store
	queue   chan models.MCPAuditEntryInput
	stop    chan struct{}
	stopped sync.Once

	// dropped counts entries shed because the queue was full at
	// send time. Surfaced via Prometheus when metrics are wired
	// (read in mcpAuditDroppedSnapshot below). Atomic so the
	// hot path doesn't lock to bump it.
	dropped atomic.Uint64
}

// newMCPAuditWriter constructs a writer with a 4096-deep queue. The
// caller is responsible for kicking the worker via run() and for
// stopping it via stop().
func newMCPAuditWriter(s *store.Store) *mcpAuditWriter {
	return &mcpAuditWriter{
		store: s,
		queue: make(chan models.MCPAuditEntryInput, mcpAuditBufferSize),
		stop:  make(chan struct{}),
	}
}

// enqueue tries to push one entry without blocking. Returns true on
// success; on a full queue, increments the drop counter and returns
// false. The MCP middleware ignores the return value — it's strictly
// observability.
func (w *mcpAuditWriter) enqueue(in models.MCPAuditEntryInput) bool {
	select {
	case w.queue <- in:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// run is the worker goroutine. Drains queue → InsertMCPAuditEntry,
// logs (but doesn't crash on) DB errors, exits on stop. Tracked via
// Server.bg so Stop() can drain in-flight writes.
func (w *mcpAuditWriter) run() {
	for {
		select {
		case <-w.stop:
			// Drain whatever's already in the queue before exit so
			// a clean shutdown loses no rows. Bounded by the queue
			// depth so this doesn't hang forever on a stuck DB.
			for {
				select {
				case in := <-w.queue:
					w.write(in)
				default:
					return
				}
			}
		case in := <-w.queue:
			w.write(in)
		}
	}
}

// write performs a single insert with a per-call timeout. DB errors
// log + drop the row — there's no retry path because the audit log
// is best-effort by design. Repeated failures will show up in the
// structured logs as "mcp audit insert failed" warns; ops can alert
// on that pattern.
func (w *mcpAuditWriter) write(in models.MCPAuditEntryInput) {
	// Timeout via a goroutine wrapper because store methods don't
	// take a context today (refactoring every store method to
	// accept ctx is out of scope for this task). The timeout is
	// observability-only — if InsertMCPAuditEntry returns after
	// timeout, the row still lands; we just don't wait for it from
	// the worker.
	done := make(chan error, 1)
	go func() {
		done <- w.store.InsertMCPAuditEntry(in)
	}()
	select {
	case err := <-done:
		if err != nil {
			slog.Warn("mcp audit insert failed",
				"error", err,
				"user_id", in.UserID,
				"token_kind", string(in.TokenKind),
				"tool_name", in.ToolName)
		}
	case <-time.After(mcpAuditWriteTimeout):
		slog.Warn("mcp audit insert timed out",
			"user_id", in.UserID,
			"tool_name", in.ToolName)
	}
}

// shutdown signals the worker to drain + exit. Idempotent.
func (w *mcpAuditWriter) shutdown() {
	w.stopped.Do(func() {
		close(w.stop)
	})
}

// mcpAuditDroppedSnapshot returns the cumulative number of audit
// entries dropped due to buffer-full backpressure. Exported as a
// method on Server so admin / metrics surfaces can read it. Returns
// 0 when no audit writer is configured (self-hosted builds without
// MCP).
func (s *Server) mcpAuditDroppedSnapshot() uint64 {
	if s.mcpAudit == nil {
		return 0
	}
	return s.mcpAudit.dropped.Load()
}

// MCPAuditLog wraps next with audit recording. Mounts after
// MCPBearerAuth in registerMCPRoutes so currentUser + the token
// identity stashed by the auth middleware are already on context.
//
// No-op when the audit writer hasn't been configured (selfhost
// builds without MCP, tests that didn't call startMCPAuditWriter).
// In that case the middleware passes the request straight through —
// MCP still works, just without forensics.
func (s *Server) MCPAuditLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.mcpAudit == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now().UTC()

		// Sniff the request body BEFORE the inner handler runs.
		//
		// Why read up-front rather than tee on the inner handler's
		// read: a TeeReader only fills the sniffer buffer when
		// something reads from it. mcp-go's streamable HTTP server
		// reads via the JSON-RPC decoder, which IS a reader — but
		// when an upstream rejects the request early (auth gate,
		// rate limit) the body never gets touched and the sniffer
		// stays empty. Reading proactively guarantees the audit row
		// has tool_name even on rejected requests, which is exactly
		// the case forensics cares about most.
		//
		// MCP request bodies are bounded JSON-RPC envelopes (one
		// per POST), not streams — reading them in full is safe.
		// We cap at mcpAuditBodyMaxBytes so a pathologically large
		// body doesn't pin memory; past the cap the audit row sees
		// "(unknown)" tool but the inner handler still gets the
		// full body via the bytes.NewReader replacement.
		var snifferBytes []byte
		if r.Body != nil && r.Method != http.MethodGet {
			limited := io.LimitReader(r.Body, mcpAuditBodyMaxBytes)
			peeked, _ := io.ReadAll(limited)
			snifferBytes = peeked
			// Stitch the peeked bytes back together with whatever's
			// left past the cap so the inner handler sees the
			// original payload intact. r.Body's Closer survives
			// because we wrap, not replace.
			restored := struct {
				io.Reader
				io.Closer
			}{
				Reader: io.MultiReader(bytes.NewReader(peeked), r.Body),
				Closer: r.Body,
			}
			r.Body = restored
		}

		// Wrap the response writer so we can read the status code +
		// body length AFTER next.ServeHTTP returns. chi already
		// provides this — reuse rather than redo.
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		latencyMs := int(time.Since(start) / time.Millisecond)

		// Pull identity off context. If MCPBearerAuth didn't run
		// (shouldn't happen — we mount after it — but defensive),
		// skip the row. We don't write rows we can't attribute.
		user, _ := CurrentUserFromContext(r.Context())
		if user == nil {
			return
		}
		kind, ref := MCPTokenIdentityFromContext(r.Context())
		if kind == "" || ref == "" {
			return
		}

		// X-Request-ID is set by chi's RequestID middleware higher
		// up the stack. If somehow absent, fall back to a synthesized
		// "" so the column constraint (NOT NULL) doesn't reject the
		// insert; downstream forensics will see an empty string and
		// know to ignore correlation.
		reqID := chimiddleware.GetReqID(r.Context())
		if reqID == "" {
			reqID = strconv.FormatInt(start.UnixNano(), 36)
		}

		toolName, argsHash := parseMCPRequestBody(snifferBytes)

		status, errorKind := classifyMCPResult(ww.Status())

		entry := models.MCPAuditEntryInput{
			Timestamp:    start,
			UserID:       user.ID,
			WorkspaceID:  "", // workspace context isn't reliably on the request — populated only when present below
			TokenKind:    models.TokenKind(kind),
			TokenRef:     ref,
			ToolName:     toolName,
			ArgsHash:     argsHash,
			ResultStatus: status,
			ErrorKind:    errorKind,
			LatencyMs:    latencyMs,
			RequestID:    reqID,
		}

		s.mcpAudit.enqueue(entry)
	})
}

// parseMCPRequestBody extracts (tool_name, args_hash) from the
// JSON-RPC envelope in body. Two cases:
//
//   - method == "tools/call" → tool_name = params.name,
//     args_hash = SHA-256 of canonical-JSON params.arguments.
//   - any other method → tool_name = method itself
//     (e.g. "initialize", "tools/list"), args_hash = "" because
//     these calls have no per-call arguments worth correlating.
//
// Returns ("(unknown)", "") on parse failure or empty body — gives
// the audit reader a visible signal rather than silently dropping
// the row.
func parseMCPRequestBody(body []byte) (toolName, argsHash string) {
	if len(body) == 0 {
		return "(unknown)", ""
	}
	var env struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Method == "" {
		return "(unknown)", ""
	}
	if env.Method != "tools/call" {
		return env.Method, ""
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(env.Params, &p); err != nil || p.Name == "" {
		return "tools/call", ""
	}
	return p.Name, hashCanonicalJSON(p.Arguments)
}

// hashCanonicalJSON returns a SHA-256 hex of a canonicalized form of
// the input JSON. "Canonical" here means: round-trip through
// encoding/json to sort object keys (Go's encoder does that natively
// since 1.12 for map[string]any). For arguments that are already a
// JSON object this produces stable hashes across calls with the same
// fields-in-different-order.
//
// Empty / null input returns "" so the column reflects "no args"
// rather than the SHA of "null" / "" — distinguishable in audit
// queries.
func hashCanonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Decode-then-encode to canonicalize key order. Failure just
	// hashes the raw bytes as-is — we'd rather have a hash that
	// might not group exactly than no audit signal at all.
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	canon, err := json.Marshal(v)
	if err != nil {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// classifyMCPResult maps an HTTP status to (audit_status, error_kind).
//
// JSON-RPC quirk: tool errors come back as 200 OK with an `error`
// field in the response body. We CAN'T see that here without
// snooping the response body too — and the spec for TASK-960 says
// "every MCP request results in exactly one audit-log row," not
// "every successful tool call." So we classify on transport-level
// status, with the understanding that result_status="ok" means
// "the gate let it through" rather than "the tool succeeded." That's
// the right granularity for forensics; per-tool success would need
// the dispatcher to emit its own audit signal post-execution, which
// is out of scope here (and probably belongs as a separate JSON-RPC
// success/failure stream).
func classifyMCPResult(httpStatus int) (models.MCPAuditResultStatus, string) {
	switch {
	case httpStatus == 0 || httpStatus == http.StatusOK:
		return models.MCPAuditResultOK, ""
	case httpStatus == http.StatusUnauthorized:
		return models.MCPAuditResultDenied, "unauthorized"
	case httpStatus == http.StatusForbidden:
		return models.MCPAuditResultDenied, "forbidden"
	case httpStatus == http.StatusTooManyRequests:
		return models.MCPAuditResultDenied, "rate_limited"
	case httpStatus >= 500:
		return models.MCPAuditResultError, "server_error_" + strconv.Itoa(httpStatus)
	case httpStatus >= 400:
		return models.MCPAuditResultError, "client_error_" + strconv.Itoa(httpStatus)
	}
	return models.MCPAuditResultOK, ""
}

// startMCPAuditWriter constructs the audit writer + spawns its
// worker goroutine + spawns the periodic retention sweeper. Called
// once at startup from cmd/pad/main.go (cloud mode only).
//
// No-op if the writer is already running — supports the test pattern
// where multiple Server instances over a shared store would otherwise
// double-spawn.
func (s *Server) startMCPAuditWriter() {
	if s.mcpAudit != nil {
		return
	}
	s.mcpAudit = newMCPAuditWriter(s.store)
	s.goAsync(func() {
		s.mcpAudit.run()
	})
	s.goAsync(func() {
		s.runMCPAuditSweeper()
	})
}

// runMCPAuditSweeper drives the 90-day retention sweep on a 24h
// ticker. Exits when the audit writer's stop channel closes (i.e.
// when Server.Stop() runs). One sweep on startup so a long-stopped
// server catches up immediately rather than waiting 24h for the
// first tick.
func (s *Server) runMCPAuditSweeper() {
	if s.mcpAudit == nil {
		return
	}
	sweep := func() {
		cutoff := time.Now().UTC().Add(-mcpAuditRetention)
		n, err := s.store.SweepMCPAuditOlderThan(cutoff)
		if err != nil {
			slog.Warn("mcp audit retention sweep failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("mcp audit retention swept", "rows", n, "cutoff", cutoff.Format(time.RFC3339))
		}
	}
	sweep()
	t := time.NewTicker(mcpAuditSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.mcpAudit.stop:
			return
		case <-t.C:
			sweep()
		}
	}
}

// stopMCPAuditWriter signals the writer to drain + exit. Called
// from Server.Stop. Idempotent.
func (s *Server) stopMCPAuditWriter() {
	if s.mcpAudit == nil {
		return
	}
	s.mcpAudit.shutdown()
}

// Compile-time guard: pin the middleware's signature so an accidental
// refactor that breaks the http.Handler chain doesn't slip in.
var _ func(http.Handler) http.Handler = (*Server)(nil).MCPAuditLog
