package server

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// Session-identity headers on GET /api/v1/events/stream (PLAN-2558 S2,
// TASK-2560). A connecting client uses them to say what it is, so the
// presence registry holds nameable sessions ("docapp") rather than
// opaque uuids — which is what turns S1's count into S5's target picker.
//
// WHY HEADERS AND NOT A QUERY STRING. The task body sketched "the
// stream connect carries it" without picking a transport; three were
// on the table (query param, header, separate registration POST) and
// this is the reasoning, recorded because the choice is otherwise
// invisible:
//
//   - A query param would put the label and pid in every access log
//     line — this server logs `path=` for each request — and in any
//     proxy log in front of it. The whole point of the privacy line
//     here (basename, never the full cwd; never the socket path) is to
//     stop local-machine detail from travelling further than it needs
//     to, and a durable log entry is exactly travelling further.
//   - A separate registration POST would need its own correlation to
//     the connection it describes, and a lifecycle to match — the
//     registry entry lives and dies with the stream, so anything that
//     can arrive independently of the stream can also fail to.
//   - A header rides the request that already exists, is trivially set
//     by the one client that has this data (the Go CLI monitor), and
//     sits alongside Last-Event-ID, which is already doing exactly this
//     job on exactly this endpoint.
//
// The known cost, stated so the next person doesn't discover it as a
// surprise: a browser EventSource cannot set request headers. Nothing
// in the web UI opens this stream today (it uses the workspace-scoped
// /api/v1/events), but if a browser tab ever wants to appear in
// presence, it will need either a query-param fallback added
// deliberately — with the logging consequence above accepted — or a
// fetch-based SSE reader.
const (
	sessionLabelHeader = "X-Pad-Session-Label"
	sessionPIDHeader   = "X-Pad-Session-Pid"
)

// sessionArmedQueryParam is PLAN-2613 S1's consent declaration —
// deliberately a QUERY PARAM, not a third header alongside label/pid.
// The header choice above is privacy-driven: a label or pid is local-
// machine detail that shouldn't travel into an access log. Armed is the
// opposite of private — it's a plain boolean, and a durable log trail of
// "this session declared consent" is a reasonable audit artifact for a
// security-relevant gate, not a leak. A query param also sidesteps the
// header doc's own noted gap (a browser EventSource can't set request
// headers), so a future non-CLI client can declare arming without
// needing a transport change. Only "true" (exact match, mirroring this
// codebase's existing boolean-query convention — e.g. handlers_items.go's
// include_archived) counts as armed; anything else, including absence,
// is the legacy/unarmed shape.
const sessionArmedQueryParam = "armed"

// maxSessionLabelLen bounds a label in RUNES, not bytes: the value is a
// directory basename, which is user-controlled text that ends up
// rendered in a picker. 64 is far more than any real project directory
// needs and short enough that no UI has to plan for it. Over-long
// labels are TRUNCATED rather than rejected — a session that shows up
// with a clipped name is strictly better than one that silently doesn't
// show up at all, which is the failure this whole slice exists to
// prevent.
const maxSessionLabelLen = 64

// parseSessionIdentity reads the self-declared session identity off a
// stream request. It never fails: every malformed input degrades to the
// S1 behaviour (an unlabelled session) rather than rejecting the
// connection, because presence is a convenience and event delivery is
// not — a client with a garbled header still deserves its events.
func parseSessionIdentity(r *http.Request) SessionIdentity {
	return SessionIdentity{
		Label: sanitizeSessionLabel(r.Header.Get(sessionLabelHeader)),
		PID:   parseSessionPID(r.Header.Get(sessionPIDHeader)),
		Armed: r.URL.Query().Get(sessionArmedQueryParam) == "true",
	}
}

// sanitizeSessionLabel makes a client-supplied label safe to store and
// hand to a UI: printable characters only, collapsed whitespace,
// truncated to maxSessionLabelLen runes.
//
// Control characters are dropped rather than escaped — a label is
// destined for terminal output (`pad` listing sessions) as much as for
// a browser, and an escape sequence there rewrites what the reader
// sees rather than merely looking ugly.
//
// Measured, not assumed: that particular arm is UNREACHABLE over HTTP
// today. Go's server rejects a request whose header value contains a
// control byte with 400 Bad Request before any handler runs (verified
// with a raw socket against httptest, since Go's own client refuses to
// send one either — neither behaviour is something to take on faith).
// It is kept as defence in depth for the next caller in: a WebSocket
// frame, a registration POST body, or a non-Go client on a laxer stack
// would all reach this function without that protection. The
// whitespace and length arms, by contrast, are reachable right now —
// tabs are legal in header values and nothing bounds their length.
//
// Either way this is hygiene, not a security boundary: the label is
// self-declared and only ever appears in its own owner's session list
// (see SessionIdentity).
func sanitizeSessionLabel(raw string) string {
	if raw == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(raw))
	lastWasSpace := false
	for _, r := range raw {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			// Collapse any run of whitespace to a single space; leading
			// runs are dropped by the lastWasSpace seed below.
			if b.Len() > 0 && !lastWasSpace {
				b.WriteRune(' ')
				lastWasSpace = true
			}
		case !unicode.IsPrint(r):
			// Dropped entirely, and deliberately NOT treated as a word
			// boundary: "do\x00capp" is far likelier to be a mangled
			// "docapp" than two words.
			continue
		default:
			b.WriteRune(r)
			lastWasSpace = false
		}
	}

	label := strings.TrimRight(b.String(), " ")
	runes := []rune(label)
	if len(runes) > maxSessionLabelLen {
		label = strings.TrimRight(string(runes[:maxSessionLabelLen]), " ")
	}
	return label
}

// parseSessionPID parses the pid header, returning 0 for anything that
// isn't a plausible process id. 0 is the sentinel for "not stated"
// (LiveSession omits it from JSON), so every rejection path lands
// there rather than propagating a nonsense number into a picker.
func parseSessionPID(raw string) int {
	if raw == "" {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
