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
// THIS NOTE USED TO SAY presence and delivery were blind in the same
// direction, and that the two had to be fixed together or not at all.
// That was true when written and is no longer: BUG-2651 shipped
// watchevents.RedisBus, so DELIVERY is now cross-instance WHEN
// PAD_REDIS_URL IS SET — a push published on A reaches a stream held on
// B, and B matches it against its own live sessions, which is the only
// set B needs. Without that env var the deployment is single-process by
// definition and none of this applies. What remains per-process in a
// Redis-backed deployment is exactly this registry, and it is worth
// being precise about what that costs, because the two halves fail
// differently:
//
//   - DELIVERY (fixed): a broadcast push — TargetUserID with no session
//     id — used to reach only the sessions on whichever instance handled
//     the POST. It now reaches all of that user's sessions. A
//     session-targeted push likewise reaches the instance holding that
//     session, wherever it is.
//   - VISIBILITY (still open): GET /api/v1/sessions answers from ONE
//     instance's registry, so a user whose session is held on B and who
//     asks A sees nothing to target. That under-report is unchanged by
//     BUG-2651 — it is neither better nor worse — so nothing regressed;
//     a session a caller can SEE is by construction on the instance that
//     listed it, and pushing to it works.
//
// So the remaining defect is a picker that under-reports, not a push
// that lies. Do not put the web-UI push surface (PLAN-2558 S3) in front
// of a multi-process deployment until a shared-state SessionPresence
// exists: a picker that silently omits half a user's sessions is its own
// kind of dishonest, even though every session it DOES list is real and
// reachable. SessionPresence is an interface from day one so that
// implementation can slot in without touching the stream handler or the
// endpoint.

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
	// Armed mirrors SessionIdentity.Armed (PLAN-2613 S1): whether this
	// connection declared consent to receive KindPush notifications.
	// Deliberately NOT omitempty — an explicit `"armed":false` is the
	// honest answer for "connected but not accepting pushes" (the S4
	// target picker's "N connected, 0 accepting" case), and omitting the
	// key on false would make a mixed-version client guess whether
	// absence means false or means "this server predates the field".
	Armed bool `json:"armed"`
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
	// Armed is PLAN-2613 S1's consent declaration: true only for a
	// client that resolved its own consent config to enabled and said
	// so explicitly at connect (the `armed` query param — see
	// session_identity.go for why this one field is a query param
	// rather than a header like Label/PID). Absent or false is the
	// legacy shape and is not a lesser trust level to fix later — it is
	// the correct answer for a client that predates the consent model
	// entirely, or one that has it but the user hasn't opted in. False
	// is the zero value on purpose: a client that says nothing gets the
	// same treatment as one that explicitly declines.
	Armed bool
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
		Armed:       ident.Armed,
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
