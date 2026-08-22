package server

import (
	"net/http"
	"strconv"
	"sync"
)

// streamAdmission bounds how many long-held streaming connections this
// process serves, globally and per user (BUG-2726).
//
// WHY IT IS NOT ON THE BUSES. Pad has two SSE endpoints, backed by two
// different buses: /api/v1/events (workspace-scoped, events.EventBus) and
// /api/v1/events/stream (user-scoped, watchevents.Bus). Each bus can bound
// its OWN subscribers atomically, and events.EventBus already does. Neither
// can bound the two together, because a shared global budget is a property
// of the PROCESS — the goroutine, the socket, the subscription and (for
// the watch stream) the presence registration cost the same whichever
// endpoint opened them. Putting the global on one bus would have let a
// user exhaust the machine through the other one while every configured
// limit still read as satisfied.
//
// So the global bound moves HERE and is passed to the events bus as 0
// (unlimited) — see handleSSE. The per-WORKSPACE bound stays on that bus,
// because it is genuinely workspace-scoped and the watch stream has no
// coherent workspace to count against.
//
// COMPATIBILITY: PAD_SSE_MAX_CONNECTIONS now covers both endpoints, so
// an operator who tuned it for one is bounding both and may reach the
// limit sooner. Deliberate — a knob that silently bounded half the
// connections it named fails invisibly, where this one announces itself
// and is tunable. The startup log reports the effective limits.
//
// TOCTOU: acquire takes the lock, checks and RESERVES in one critical
// section, exactly as events.EventBus.SubscribeIfAllowed does. Two
// concurrent requests cannot both pass a check for the last slot.
type streamAdmission struct {
	mu       sync.Mutex
	total    int
	perUser  map[string]int
	maxTotal int
	maxUser  int
}

func newStreamAdmission(maxTotal, maxUser int) *streamAdmission {
	return &streamAdmission{
		perUser:  map[string]int{},
		maxTotal: maxTotal,
		maxUser:  maxUser,
	}
}

// setLimits updates the bounds in place.
//
// In place rather than by replacing the gate, because a replacement
// silently OVER-GRANTS capacity: the new gate starts at zero while the
// old one still holds every open connection's slot, so those connections
// stop counting against the limit.
//
// The GAUGE would not expose that — a replaced gate is still reachable
// through Server.admission(), so the scrape reads the new gate and looks
// plausible while the budget is wrong. Which is why the test asserts
// admission behaviour instead.
func (a *streamAdmission) setLimits(maxTotal, maxUser int) {
	a.mu.Lock()
	a.maxTotal = maxTotal
	a.maxUser = maxUser
	a.mu.Unlock()
}

// admissionRefusal names which bound refused a connection, for the log
// line and the error message. Bounded values, safe to expose.
type admissionRefusal string

const (
	admissionRefusalNone   admissionRefusal = ""
	admissionRefusalGlobal admissionRefusal = "global"
	admissionRefusalUser   admissionRefusal = "per_user"
)

// acquire reserves one stream slot for a PRINCIPAL. The returned release
// MUST be called when the connection ends; it is idempotent so a handler
// can defer it unconditionally.
//
// The principal is a user id where there is one. Where there is not —
// legacy workspace-scoped tokens and the fresh-install no-auth window,
// both of which /api/v1/events accepts — the caller supplies a
// workspace-derived key instead (see streamPrincipal). Skipping the bound
// for those callers would let ONE legacy-token holder fill the budget and
// 429 everyone else; bucketing them under a single empty string would
// make unrelated anonymous callers evict each other. Per workspace is the
// finest granularity actually available.
//
// The residual trade, stated rather than hidden: two legacy tokens for
// the SAME workspace share a bucket and can evict each other at the
// per-user limit. That is strictly better than unbounded, and the
// per-workspace bound already treats them as one population anyway.
//
// An empty principal still skips the per-user bound. It should not occur
// — both call sites supply one — and the guard is there so a future
// caller that forgets cannot accidentally collapse every connection into
// a single bucket.
func (a *streamAdmission) acquire(principal string) (release func(), refusal admissionRefusal) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.maxTotal > 0 && a.total >= a.maxTotal {
		return nil, admissionRefusalGlobal
	}
	if a.maxUser > 0 && principal != "" && a.perUser[principal] >= a.maxUser {
		return nil, admissionRefusalUser
	}

	a.total++
	if principal != "" {
		a.perUser[principal]++
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.total--
			if principal != "" {
				a.perUser[principal]--
				// Delete at zero so the map does not grow without bound
				// on a deployment where many users connect once. Leaving
				// zero entries behind would make this a slow leak keyed
				// by user id.
				if a.perUser[principal] <= 0 {
					delete(a.perUser, principal)
				}
			}
		})
	}, admissionRefusalNone
}

// heldTotal returns the number of held slots. Backs
// pad_stream_connections_active as a scrape-time collector — see
// metrics.RegisterStreamConnectionsCollector for why that replaced a
// pushed gauge.
func (a *streamAdmission) heldTotal() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// streamLimitRetryAfterSeconds is the Retry-After a refused streaming
// connection is told to wait.
//
// A hint, not a promise: nothing reserves the slot, and a client that
// waits exactly this long may be refused again. It is here because a bare
// 429 tells a client only that it lost, and every client then picks its
// own guess — the CLI monitor's ladder starts at 5s, so this matches it
// rather than inventing a third number.
//
// Browsers' EventSource ignores Retry-After entirely and retries on its
// own schedule; that gap is BUG-2733, not something a header can close.
const streamLimitRetryAfterSeconds = 5

// writeStreamLimitExceeded answers a refused streaming connection.
//
// One helper for both endpoints, so the refusal contract cannot drift
// between them — status, error code, message and Retry-After are the
// same thing said once.
func writeStreamLimitExceeded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(streamLimitRetryAfterSeconds))
	writeError(w, http.StatusTooManyRequests, "sse_limit_exceeded", "Streaming connection limit reached")
}

// counts returns the current global total and this user's count, for log
// lines. Snapshot only — never use it to decide admission, which is
// acquire's job under one lock.
func (a *streamAdmission) counts(principal string) (total, user int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total, a.perUser[principal]
}

// streamPrincipal is the key a request is bounded under: its user id when
// one resolved, otherwise a workspace-derived key for the callers that
// have no user — legacy workspace-scoped tokens (whose token carries a
// workspace id) and the fresh-install no-auth window (which does not, so
// the resolved workspace stands in).
//
// The "ws:" prefix keeps the two namespaces from ever colliding: user ids
// are UUIDs, so an unprefixed workspace id could in principle be read as
// one.
func streamPrincipal(r *http.Request, workspaceID string) string {
	if uid := currentUserID(r); uid != "" {
		return uid
	}
	if tokenWS := tokenWorkspaceID(r); tokenWS != "" {
		return "ws:" + tokenWS
	}
	if workspaceID != "" {
		return "ws:" + workspaceID
	}
	return ""
}
