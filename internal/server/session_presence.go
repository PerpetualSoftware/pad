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
// WHICH IMPLEMENTATION YOU GET, and what each one can and cannot see.
// cmd/pad/cmd_server.go picks on PAD_REDIS_URL, the same switch both
// buses use:
//
//   - No PAD_REDIS_URL — MemorySessionPresence. The deployment is
//     single-process by definition, so per-process presence is complete
//     presence and nothing below applies.
//   - PAD_REDIS_URL set — RedisSessionPresence (BUG-2698,
//     session_presence_redis.go). Every instance reads and writes one
//     shared registry, so a session held on B is visible to A.
//
// THAT SPLIT USED TO BE A DEFECT, and the history is worth keeping
// because the three symptoms looked unrelated. BUG-2651 gave watchevents
// a Redis bus, which made DELIVERY cross-instance while this registry
// stayed per-process — and a shared bus with a per-process registry was
// worse than either being consistent:
//
//   - BROADCAST DELIVERY was fixed by the bus alone, but its REPORTED
//     count was not: delivered_sessions counted the answering instance's
//     sessions while the bus delivered to all of them.
//   - TARGETED DELIVERY was not fixed by the bus at all, because
//     handlers_push.go gates on THIS registry BEFORE publishing and
//     skipped the publish entirely when the id was absent locally. The
//     bus would have carried it; it was never put on the bus.
//   - VISIBILITY was not fixed either: GET /api/v1/sessions answered
//     from one instance's registry, so a user whose session was held on
//     B and who asked A had nothing to select in the picker.
//
// One shared registry closes all three, and in an order worth noticing:
// making the registry global makes the snapshot right, which makes the
// picker complete AND restores the push gate's original premise — the
// skip then means what it was written to mean ("nothing is listening")
// rather than "this instance cannot see who is listening". The tempting
// shortcut of publishing unconditionally for targeted pushes would have
// fixed delivery while making delivered_sessions:0 a lie in the other
// direction.
//
// (Two earlier versions of this note were wrong in opposite directions —
// one claimed BUG-2651 fixed targeted delivery, written from reading the
// bus and this file without reading the push handler's gate; the one
// before it claimed presence and delivery were blind in the same
// direction and had to be fixed together. Kept as a reminder that a
// claim about a path you have not read is a claim, not a caveat.)
//
// SessionPresence has been an interface since S1 precisely so the
// shared-state implementation could slot in without rewriting the stream
// handler or the endpoint, and that held: the STREAM HANDLER is unchanged.
//
// The ENDPOINT is not, and an earlier draft of this note claimed both were
// (codex round 6). handleListSessions gained a 503 branch, because the
// out-of-process implementation made "I could not find out" a reachable
// answer that the in-process one never had — the interface absorbed the
// implementation swap, not the new failure mode the implementation brought
// with it. Those are different claims and only the first one was true.

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
// A DEAD INSTANCE IS A SECOND, LONGER WINDOW, and it exists only for the
// shared registry (codex round 1, P2 on BUG-2698). The ~30s above is
// about a dead CLIENT, detected when the keepalive write fails and the
// handler's defer deregisters. If the SERVER PROCESS dies instead,
// MemorySessionPresence loses its entries instantly — they lived in the
// process that died — while RedisSessionPresence's outlive it and clear
// only when their TTL lapses, up to ~90s (sessionKeyTTL). During that
// window a picker can offer a session on an instance that no longer
// exists, and a push targeted at it will publish and reach nobody while
// reporting one delivery.
//
// Documented rather than shortened, deliberately: a TTL close to the
// renewal interval would start evicting LIVE sessions on any hiccup — a
// GC pause, a briefly slow Redis — and an evicted live session is
// invisible to the picker AND makes the push gate skip a genuinely
// connected target. Do not fix a staleness window by creating an eviction
// failure. See sessionKeyTTL for the arithmetic.
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
	// BearerAuth is whether the request that OPENED this stream carried
	// bearer/API-token auth rather than a session cookie (BUG-2725).
	// Server-observed, never client-declared — it comes from
	// SessionOrigin, not SessionIdentity, and the split between those two
	// types is the trust boundary.
	//
	// WHY A REGISTRY FIELD IS ADMISSIBLE HERE WHEN A VISIBILITY SNAPSHOT
	// IS NOT. Dave's day-49 ruling on this bug rejected storing each
	// session's resolved visibility and required re-resolution at push
	// time instead. That ruling is about STALENESS: membership,
	// CollectionAccess and grants are DERIVED and REVOCABLE, so a value
	// cached at Add goes wrong exactly when it matters most (access
	// revoked mid-connection), which is why watchNotificationVisible
	// re-reads per notification.
	//
	// Auth transport is a different kind of fact. It is a property of the
	// connection itself, fixed by the request that opened it, and there
	// is no operation that changes it for a live stream — a cookie
	// session cannot become a bearer one without reconnecting, at which
	// point Add runs again and mints a new id. It cannot go stale, so the
	// ruling's reasoning does not reach it. Armed is the standing
	// precedent: a per-connection fact living here and consulted by
	// deliveredSessionCount.
	//
	// THE RULE FOR ANYONE EXTENDING THIS STRUCT: connection properties
	// are admissible; derived authorization state is not. If the value
	// can be revoked by an action taken elsewhere while the stream stays
	// open, it does not belong in the registry — re-resolve it instead.
	//
	// Not omitempty, for the same reason Armed isn't: an explicit
	// `"bearer_auth":false` is the honest answer for a cookie-opened
	// stream, and omitting the key on false would make a mixed-version
	// reader guess whether absence means false or means "this server
	// predates the field". It must also survive json.Marshal round trips
	// through Redis (RedisSessionPresence.Add persists this struct), so
	// a `json:"-"` would silently drop it on exactly the multi-instance
	// deployment that needs it most.
	BearerAuth bool `json:"bearer_auth"`
	// ConnectedAt is when the stream opened, UTC.
	ConnectedAt time.Time `json:"connected_at"`
}

// SessionOrigin is what the SERVER observed about a connection when it
// opened, as opposed to SessionIdentity, which is what the CLIENT
// claimed about itself (PLAN-2558 S2's honesty-not-verification line).
//
// The two are separate types on purpose. SessionIdentity's doc comment
// says nothing in it is checked against anything and warns against
// building on it as if it were trustworthy; folding a server-derived,
// security-relevant fact into that struct would quietly retract that
// warning for one field and leave every reader to work out which fields
// are which. A caller cannot forge anything in here — it is filled from
// the request the server itself is holding.
type SessionOrigin struct {
	// BearerAuth mirrors LiveSession.BearerAuth: isBearerAuth of the
	// request that opened the stream. See that field for why this one
	// fact is admissible in the registry when resolved visibility is not.
	BearerAuth bool
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
	// sanitized — see SessionIdentity on why it is never trusted. The
	// origin is what the server observed about the connection instead,
	// and is trustworthy for exactly that reason (BUG-2725).
	Add(userID string, ident SessionIdentity, origin SessionOrigin) string
	// Remove deregisters a session. It must be idempotent and must not
	// panic on an unknown id — the stream handler calls it from a defer
	// that also runs on paths where Add may not have been reached.
	Remove(userID string, sessionID string)
	// ListForUser returns userID's live sessions, oldest connection
	// first. The returned slice is a copy and is safe to retain.
	//
	// THE ERROR IS NOT DECORATION, and an implementation that cannot fail
	// must still not drop it (codex round 1, P1 on BUG-2698). Without it,
	// "this user has no sessions" and "I could not find out" are the same
	// value to every consumer — and they demand OPPOSITE handling: the
	// first means a push has nothing to deliver to, the second means the
	// caller must not conclude anything. An out-of-process implementation
	// makes the difference reachable at runtime (a Redis outage), where
	// returning an empty list would make handleListSessions answer 200
	// with no sessions — the precise lie its 503 exists to avoid — and
	// would make a TARGETED push skip its publish and lose the
	// instruction while reporting success.
	//
	// MemorySessionPresence always returns a nil error; that is a property
	// of that implementation, not of this contract.
	ListForUser(userID string) ([]LiveSession, error)
}

// MemorySessionPresence is the in-process SessionPresence, and the one a
// deployment gets when PAD_REDIS_URL is unset — which is also the only
// deployment shape it is correct for. See this file's WHICH IMPLEMENTATION
// YOU GET note before putting it behind more than one padd process;
// RedisSessionPresence (session_presence_redis.go) is what that needs.
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
func (p *MemorySessionPresence) Add(userID string, ident SessionIdentity, origin SessionOrigin) string {
	id := uuid.NewString()
	// Stamp the time OUTSIDE the lock — time.Now can be comparatively
	// slow on some platforms and there is no correctness reason to hold
	// writers behind it.
	sess := LiveSession{
		ID:          id,
		Label:       ident.Label,
		PID:         ident.PID,
		Armed:       ident.Armed,
		BearerAuth:  origin.BearerAuth,
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
func (p *MemorySessionPresence) ListForUser(userID string) ([]LiveSession, error) {
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
	// Always nil: an in-process map read cannot fail. See the interface's
	// doc comment for why the error is in the signature anyway.
	return out, nil
}
