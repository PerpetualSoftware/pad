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
// The identity fields are deliberately thin in S1. Label is empty until
// S2 teaches the stream connection to carry the `pad session register`
// payload (cwd basename, pid) it already writes to
// ~/.pad/sessions/<pid>.json; until then a session is an opaque id and
// a connect time, which is all a COUNT needs. Note that S2 must send
// the label only — never messaging_socket_path, which is local-machine
// state with no business crossing the wire.
type LiveSession struct {
	// ID is server-generated per connection, not per client. A client
	// that reconnects (including the Last-Event-ID resume path) gets a
	// NEW id, because from the presence registry's point of view a
	// resumed stream is a different open connection. Callers must not
	// treat this as a stable client identity.
	ID string `json:"id"`
	// Label is a human-meaningful name for the session ("docapp"),
	// populated in S2. Empty means "unlabelled", not "unknown user" —
	// consumers should fall back to the id, never hide the session.
	Label string `json:"label,omitempty"`
	// ConnectedAt is when the stream opened, UTC.
	ConnectedAt time.Time `json:"connected_at"`
}

// SessionPresence tracks which of a user's event streams are open right
// now. Implementations must be safe for concurrent use: Add/Remove are
// called from each stream connection's own goroutine while ListForUser
// is served from unrelated request goroutines.
type SessionPresence interface {
	// Add registers a newly-opened stream for userID and returns the
	// session's generated id, which the caller MUST pass to Remove.
	Add(userID string, label string) string
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
func (p *MemorySessionPresence) Add(userID string, label string) string {
	id := uuid.NewString()
	// Stamp the time OUTSIDE the lock — time.Now can be comparatively
	// slow on some platforms and there is no correctness reason to hold
	// writers behind it.
	sess := LiveSession{ID: id, Label: label, ConnectedAt: time.Now().UTC()}

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
