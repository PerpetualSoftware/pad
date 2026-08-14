package server

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// This file implements the session-presence registry (PLAN-2558 S1,
// TASK-2559): the server's answer to "is anything actually listening
// right now?"
//
// WHY IT EXISTS. `pad push` (IDEA-2544 Phase 1, handlers_push.go) is
// fire-and-forget by design — no durable inbox, no "no session
// connected" warning. That is a defensible contract for a CLI verb
// typed by someone who knows whether their own session is running. It
// is NOT a defensible contract for a web-UI button: "Push to Claude"
// that silently goes nowhere is worse than the clipboard ferry it
// replaces, because the user has no way to tell the two outcomes
// apart. The presence registry lets the UI answer the question BEFORE
// the click (PLAN-2558's "Honesty" rationale), and — once sessions
// carry a label (S2, TASK-2560) — turns the same data into the target
// picker S5 needs.
//
// SINGLE-PROCESS LIMITATION, stated up front for the same reason
// internal/watchevents states its own: MemorySessionPresence tracks
// connections held open by THIS process only. In a multi-process padd
// deployment (Pad Cloud) a session connected to instance A is invisible
// to instance B, so /api/v1/sessions under-reports.
//
// The precise shape of that matters, because the obvious reading —
// "presence lies about delivery" — is not quite it. watchevents.Bus has
// the SAME per-process boundary: a push published on instance A never
// reaches a stream held on instance B either. So presence and delivery
// are blind in the same direction, and a request pair that lands on one
// instance gets a presence answer that correctly predicts what a push
// would do. What is NOT guaranteed is that the pair lands together — a
// load balancer is free to route the POST and the GET to different
// instances, and then the two disagree. So the honest statement is:
// per-instance presence is as accurate as per-instance delivery, and
// both stop being trustworthy at the same boundary, for the same
// reason, and only a shared-state implementation fixes either.
//
// Hence SessionPresence is an interface from day one: a Redis-backed
// implementation must be able to slot in without touching the stream
// handler or the endpoint — and it should land alongside the Redis
// watchevents.Bus that package's doc comment already anticipates, not
// separately, since fixing one without the other just moves the
// disagreement. Do not put the web-UI push surface (PLAN-2558 S3) in
// front of a multi-process deployment until both exist.

// LiveSession is one currently-connected user-scoped event stream —
// i.e. one `GET /api/v1/events/stream` connection being held open.
//
// STALENESS (moved here in S2 from where S1 left it, wedged above
// Label, where it read as documenting the name rather than the entry —
// it is a property of every field below). A session in this list is
// "connected" as of the last time the server could tell, which is not
// the same as "connected now". A CLEAN disconnect deregisters
// immediately (the stream handler's ctx.Done fires and its defer runs).
// An UNGRACEFUL one — half-open TCP, closed laptop, dropped network —
// is invisible to the server until the next keepalive write fails, and
// watchEventsKeepaliveInterval is 30s. So this list can name a listener
// that is already gone, for up to ~30 seconds.
//
// That bound is acceptable for push, which is fire-and-forget either
// way: a push to a session that died 5 seconds ago costs a message that
// would have been lost regardless. It is NOT acceptable as a delivery
// guarantee, and consumers must not upgrade it into one — "one session
// connected" is honest, "this will be delivered" is not (PLAN-2558 S3).
//
// Shortening the window means shortening the keepalive, which costs
// every idle connection real traffic. Not worth it for a
// fire-and-forget channel; revisit only if a consumer appears that
// genuinely needs delivery confidence, and give that consumer an ack
// rather than a faster heartbeat.
//
// Label and PID arrive from the client (S2) and are self-declared —
// see SessionIdentity.
type LiveSession struct {
	// ID is server-generated per connection, not per client. A client
	// that reconnects (including the Last-Event-ID resume path) gets a
	// NEW id, because from the presence registry's point of view a
	// resumed stream is a different open connection. Callers must not
	// treat this as a stable client identity.
	ID string `json:"id"`
	// Label is a human-meaningful name for the session ("docapp"),
	// populated in S2 from the connecting client's working-directory
	// basename. Empty means "unlabelled", not "unknown user" —
	// consumers should fall back to the id, never hide the session.
	Label string `json:"label,omitempty"`
	// PID is the connecting client process's own pid, or 0 when it
	// didn't say (S2). It exists to disambiguate two sessions that
	// share a label — two agents in the same checkout both report
	// "docapp" — not to identify a process the server can act on.
	PID int `json:"pid,omitempty"`
	// ConnectedAt is when the stream opened, UTC.
	ConnectedAt time.Time `json:"connected_at"`
}

// SessionIdentity is what a connecting client tells the server about
// itself when it opens a stream (PLAN-2558 S2, TASK-2560).
//
// SELF-DECLARED, NOT VERIFIED — the same honesty-not-verification line
// BUG-2542 drew for actor attribution. Nothing here is checked against
// anything: a client can claim any label and any pid. That is
// acceptable precisely because the blast radius is the caller's own
// row — GET /api/v1/sessions is self-scoped with no admin view
// (handlers_sessions.go), so the only list a lying client corrupts is
// its own. Do not build anything on these fields that would need them
// to be trustworthy; if that day comes, the fix is a server-side
// identity, not tighter validation of a claim.
//
// What is deliberately NOT here: messaging_socket_path. `pad session
// register` records it locally (internal/cli/session_registry.go) and
// it must not cross the wire — a filesystem path on the user's machine
// is of no use to the server and every use to anyone who shouldn't
// have it. Nor does the full cwd cross: the basename is what a picker
// needs, while the absolute path leaks the home directory (and usually
// the account name) for no gain.
type SessionIdentity struct {
	// Label is a short human-meaningful name, already sanitized by
	// parseSessionIdentity before it reaches the registry.
	Label string
	// PID is the client's own process id, or 0 for "not stated".
	PID int
}

// SessionPresence tracks which of a user's event streams are open right
// now. Implementations must be safe for concurrent use: Add/Remove are
// called from each stream connection's own goroutine while ListForUser
// is served from unrelated request goroutines.
//
// CONSTRAINT ON ANY OUT-OF-PROCESS IMPLEMENTATION (codex round 2, P2,
// refined). Server.Shutdown delegates to http.Server.Shutdown, and Go's
// stdlib does NOT cancel an in-flight handler's request context there —
// it waits for the handler to return. SSE handlers only return on their
// own ctx.Done or a failed write, so a shutdown leaves stream handlers
// (and their presence entries) hanging until something else knocks them
// loose.
//
// For MemorySessionPresence this is harmless, and saying so is the
// point: the registry lives in the process that is going away, so its
// entries die with it. There is nothing to reap.
//
// A Redis-backed implementation does NOT inherit that mercy. Its
// entries outlive the process that wrote them, so a hard shutdown or a
// crash strands them, and the list starts naming sessions belonging to
// an instance that no longer exists — permanently, not for 30 seconds.
// Such an implementation MUST carry its own reaping story (TTL with
// heartbeat renewal, or ownership keyed by instance id and swept on
// startup). Do not port MemorySessionPresence's lifecycle assumptions
// across; they are load-bearing on the in-process case only.
type SessionPresence interface {
	// Add registers a newly-opened stream for userID and returns the
	// session's generated id, which the caller MUST pass to Remove.
	// The identity is whatever the client claimed about itself, already
	// sanitized — see SessionIdentity on why it is never trusted.
	Add(userID string, ident SessionIdentity) string
	// Remove deregisters a session. It must be idempotent and must not
	// panic on an unknown id — the stream handler calls it from a defer
	// that also runs on paths where Add may not have been reached.
	Remove(userID string, sessionID string)
	// ListForUser returns userID's live sessions, oldest connection
	// first. The returned slice is a copy and is safe to retain.
	ListForUser(userID string) []LiveSession
}

// MemorySessionPresence is the in-process SessionPresence — see this
// file's SINGLE-PROCESS LIMITATION note before deploying it behind more
// than one padd process.
type MemorySessionPresence struct {
	mu sync.RWMutex
	// byUser is userID -> sessionID -> session. Two levels rather than a
	// flat map keyed by session id because every read is user-scoped
	// (there is no "list all sessions on this server" consumer, and
	// there should not be one — see handleListSessions' doc comment on
	// why this endpoint is not admin-visible).
	byUser map[string]map[string]LiveSession
}

// NewMemorySessionPresence returns an empty in-process registry.
func NewMemorySessionPresence() *MemorySessionPresence {
	return &MemorySessionPresence{byUser: make(map[string]map[string]LiveSession)}
}

// Add implements SessionPresence.
func (p *MemorySessionPresence) Add(userID string, ident SessionIdentity) string {
	id := uuid.NewString()
	// Stamp the time OUTSIDE the lock — time.Now can be comparatively
	// slow on some platforms and there is no correctness reason to hold
	// writers behind it.
	sess := LiveSession{
		ID:          id,
		Label:       ident.Label,
		PID:         ident.PID,
		ConnectedAt: time.Now().UTC(),
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	sessions, ok := p.byUser[userID]
	if !ok {
		sessions = make(map[string]LiveSession)
		p.byUser[userID] = sessions
	}
	sessions[id] = sess
	return id
}

// Remove implements SessionPresence. Removing the user's last session
// deletes the user's bucket too, so an idle server doesn't accumulate
// an empty map per user who has ever connected.
func (p *MemorySessionPresence) Remove(userID string, sessionID string) {
	if sessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sessions, ok := p.byUser[userID]
	if !ok {
		return
	}
	delete(sessions, sessionID)
	if len(sessions) == 0 {
		delete(p.byUser, userID)
	}
}

// ListForUser implements SessionPresence, oldest connection first with
// the session id as a tiebreaker so the order is deterministic even for
// two connections that opened within the same clock tick (a real case
// under a coarse monotonic clock, and an unstable list order would make
// the S5 target picker jump around under the user's cursor).
func (p *MemorySessionPresence) ListForUser(userID string) []LiveSession {
	p.mu.RLock()
	sessions := p.byUser[userID]
	out := make([]LiveSession, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sess)
	}
	p.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	return out
}
