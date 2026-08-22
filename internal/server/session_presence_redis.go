package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisSessionPresence is the shared-state SessionPresence (BUG-2698) —
// the implementation session_presence.go's interface has been waiting for
// since PLAN-2558 S1, and the one that makes a multi-instance deployment
// tell the truth about who is listening.
//
// WHAT IT FIXES, precisely, because the obvious reading is too broad.
// BUG-2651 gave watchevents a Redis bus, so a notification PUBLISHED on
// any instance now reaches a stream held on any other. What stayed broken
// is everything that depends on knowing WHICH sessions exist:
//
//   - A session-targeted push was resolved against the presence registry
//     of whichever instance handled the POST, and handlers_push.go SKIPS
//     the publish entirely when the target is absent from that snapshot.
//     With per-process presence, a session connected to B was invisible to
//     A, so a POST that the load balancer sent to A dropped the
//     instruction and answered 200 delivered_sessions:0. Reachable through
//     the intended flow: GET /api/v1/sessions answered by B lists the
//     session, and the POST that follows is free to land on A.
//   - delivered_sessions was wrong in BOTH directions for a broadcast
//     push — a local count describing a global delivery.
//   - GET /api/v1/sessions under-reported, so the web push dialog's target
//     picker could not offer a session on another instance at all.
//
// One shared registry closes all three, and the ORDER matters: making the
// registry global makes the snapshot right, which makes the picker
// complete AND restores the push gate's original premise — at which point
// the existing skip is correct for the reason it was written, rather than
// being something to work around. Publishing unconditionally for targeted
// pushes would have fixed delivery and immediately made
// delivered_sessions:0 a lie in the other direction.
//
// THE REAPING STORY, which session_presence.go's interface doc requires of
// any out-of-process implementation and does not let this one inherit.
// MemorySessionPresence's entries die with the process that wrote them —
// there is nothing to reap, and Server.Shutdown leaving SSE handlers (and
// their presence entries) hanging is harmless for it. Redis entries
// OUTLIVE their writer, so a crash or a hard kill would otherwise strand
// them permanently, and the list would name sessions belonging to an
// instance that no longer exists.
//
// This implementation reaps with Redis's own expiry rather than with
// bookkeeping of its own:
//
//   - Each session is a key with a TTL (sessionKeyTTL), renewed every
//     sessionRenewInterval by a goroutine that lives exactly as long as
//     the connection's Add..Remove span. A process that dies stops
//     renewing, and Redis deletes the entries — no sweeper, no instance
//     ownership records, no startup scan.
//   - The per-user index is a SET carrying the same TTL, written in the
//     SAME atomic step as the entry it indexes (see writeScript), so a
//     live session is never briefly absent from the only enumeration of
//     it. A member whose session key has since EXPIRED is pruned by the
//     next reader, so the index still self-heals without a background job.
//
// Native expiry is deliberate: the alternative — storing an expiry
// timestamp per entry and filtering at read time — compares a timestamp
// written by one instance's clock against another instance's clock, and
// "two instances disagree about presence" is the exact bug class this type
// exists to end.
//
// EVICTION LOOKS EXACTLY LIKE EXPIRY, and that is survivable by design
// rather than by luck (codex round 15). Under a maxmemory policy that can
// evict live keys — `allkeys-lru`, which the repo's own docker-compose
// configures — Redis may drop a session entry or the index while the
// session is still connected. Nothing here can tell that from a TTL
// lapsing, so the session goes briefly unlisted and a targeted push at it
// is skipped.
//
// The repair is already the renewal's job: renewLoop re-SETs the full
// payload rather than issuing a bare EXPIRE, precisely so a renewal
// RESTORES a vanished entry instead of no-oping against a missing key. So
// an eviction costs at most one renewal interval of invisibility, and the
// same mechanism covers a Redis restart. Operators who want to avoid even
// that should keep Pad's Redis off an evicting policy — see
// docs/deployment.md.
//
// A COMPROMISED REPLICA IS NOT IN THIS TYPE'S THREAT MODEL, and saying so
// is more useful than implying otherwise (codex round 17). Anything holding
// the shared Redis credentials can enumerate these keys, delete real
// entries, insert fake armed sessions, or grow a victim's index without
// bound — none of which this type can prevent, since it has no ownership
// or integrity mechanism and adding one would not help against an attacker
// who also holds the credentials.
//
// It is worth being precise that this does not WIDEN that exposure. The
// same credentials already reach internal/watchevents' shared bus, where an
// attacker can both READ every notification crossing the deployment —
// including the instruction text of every user's pushes — and PUBLISH
// arbitrary ones into any user's session. Reading presence metadata and
// corrupting a picker are strictly smaller capabilities than injecting
// instructions into someone's agent. The boundary that matters is the
// Redis credential, and it is the same boundary it was before this type
// existed.
//
// The authenticated-user case — one user holding unbounded streams — is a
// different question, and this type still does not answer it. It is
// answered UPSTREAM, by the admission gate in stream_admission.go
// (BUG-2726): both SSE endpoints acquire a slot before subscribing, so a
// refusal costs one connection rather than making a live session
// untargetable, which is why a registry-side cap was rejected.
//
// SINGLE-NODE REDIS ONLY, like the rest of Pad's Redis integration:
// cmd/pad/cmd_server.go dials with redis.NewClient, not a cluster client,
// and this type's keys are not hash-tagged, so a user's index and entries
// would land in different slots. That is the whole integration's
// constraint rather than this type's, and changing it belongs with the
// keyspace work in BUG-2724, not here — one keyspace growing hash tags the
// others lack is the same divergence that note already forbids.
//
// STALENESS IS UNCHANGED, and must not be read as improved. A session
// still disappears from this list only when its stream handler returns
// (clean disconnect) or the keepalive write fails (up to ~30s after an
// ungraceful one) — see LiveSession's doc comment. The TTL is a crash
// backstop, not a liveness probe: it is deliberately several renewal
// intervals long, so it never expires a session whose process is merely
// busy. Consumers must keep treating this list as "connected as far as the
// server can tell", never as a delivery guarantee.
type RedisSessionPresence struct {
	// presenceObservable carries the optional operational-event seam
	// (BUG-2727). Embedded so SetObserver is part of the type's surface.
	presenceObservable

	client *redis.Client

	// keys builds this installation's Redis names (BUG-2724). The zero
	// value is the historical un-namespaced keyspace.
	keys redisns.Keys

	// sessionKeyTTL and renewInterval are fields rather than constants so
	// tests can drive the expiry path without sleeping through it. Prefer
	// NewRedisSessionPresence, which sets the production pair.
	sessionKeyTTL time.Duration
	renewInterval time.Duration
	// opTimeout bounds every Redis call, defaulting to presenceOpTimeout.
	// A field for the same reason the two above are: a test needs to drive
	// the stalled-Redis path without waiting out the production bound.
	opTimeout time.Duration
	// drainTimeout bounds Close's wait for renewal goroutines, defaulting
	// to closeDrainTimeout. A field so a test can park a renewal and assert
	// Close still returns — see closeDrainTimeout for why that could not be
	// asserted while it was a constant.
	drainTimeout time.Duration

	// onRenewWrite, when non-nil, is called by renewLoop immediately
	// before each renewal write. Always nil in production — it exists so a
	// test can hold a renewal INSIDE its write and prove that Remove waits
	// for it (codex round 1, P2).
	//
	// A seam rather than a timing loop, deliberately: the probabilistic
	// version of that test — 50µs renewal interval, 200 add/remove
	// iterations — passed 3/3 against the UNFIXED Remove, so it was
	// evidence of nothing. An instrument that cannot fail on broken code
	// is not an instrument.
	onRenewWrite func()

	mu     sync.Mutex
	closed bool
	// renewLog rate-limits the renewal-failure warning. A Redis outage
	// fails EVERY session's renewal on every tick, so at a thousand
	// sessions the unthrottled version emitted ~33 warnings a second per
	// replica — enough to page on volume while saying nothing an operator
	// could act on, and enough to bury the errors that matter (codex round
	// 33). One line per interval carries the same information plus how many
	// it stands for.
	renewLog struct {
		last       time.Time
		suppressed int
	}
	renewals map[string]*renewal // userID|sessionID -> its live renewal
	wg       sync.WaitGroup
}

const (
	// sessionRenewInterval matches watchEventsKeepaliveInterval (30s), the
	// cadence at which the stream handler already proves the connection is
	// alive. Renewing more often would buy nothing: a connection that has
	// died between keepalives is not detectable here either.
	sessionRenewInterval = 30 * time.Second

	// sessionKeyTTL is THREE renewal intervals, not one. A TTL close to the
	// interval would evict live sessions on any hiccup — a GC pause, a
	// briefly slow Redis, a rescheduled goroutine — and an evicted session
	// is invisible to the target picker AND causes handlers_push.go to skip
	// the publish for a session that is genuinely connected, which is
	// exactly the failure this type exists to remove. Three intervals means
	// a process must miss three consecutive renewals before its sessions
	// vanish, while a crashed instance's entries still clear inside 90s
	// rather than lingering forever.
	//
	// THE COST OF THAT CHOICE, stated because it is a real regression
	// against MemorySessionPresence and codex round 1 was right to raise
	// it: for up to this long after an instance DIES, its sessions are
	// still listed, so a picker can offer one that no longer exists and a
	// push targeted at it publishes and reaches nobody. The in-memory
	// registry had no such window — its entries died with its process.
	// The trade is deliberate and it is not close: do not fix a staleness
	// window by creating an eviction failure. A shorter TTL trades a
	// bounded window in which a DEAD session looks alive for an unbounded
	// one in which a LIVE session looks dead, and the second is worse on
	// both surfaces — the picker hides a working target, and the push gate
	// skips a genuinely connected one. Documented in LiveSession's
	// staleness note and in the web dialog's header rather than tuned.
	sessionKeyTTL = 3 * sessionRenewInterval

	// presenceOpTimeout bounds every Redis call this type makes, including
	// ADD's registration write, which used to run on context.Background()
	// (codex round 2, P1).
	//
	// WHAT IT DOES NOT BOUND, measured rather than assumed: go-redis does
	// not apply a command context to CONNECTION ESTABLISHMENT. Against a
	// listener that accepts and then never answers, a first command with a
	// 150ms context took 5.0s to return `i/o timeout` — the client's own
	// DialTimeout, not the context. So this deadline governs commands on an
	// already-established connection; the first call after a stall is
	// bounded by the client's dial/read timeouts instead. Both are finite,
	// which is what shutdown needs, but do not read this constant as the
	// worst case.
	presenceOpTimeout = 5 * time.Second

	// closeDrainTimeout bounds how long Close waits for renewal goroutines.
	//
	// Close waits on a WaitGroup whose counter includes a goroutine that has
	// not started yet — Add increments it before the registration write,
	// deliberately, so a Close racing an Add cannot return before that
	// session is accounted for. The cost is that a stalled Redis puts Add's
	// write between Close and its own completion.
	//
	// WITH THE PRODUCTION CLIENT CONFIG this deadline never fires:
	// go-redis's own dial/read timeouts release the write first, and a
	// mutation test that stretched the deadline to 24 hours stayed green
	// for exactly that reason. It is still real behaviour rather than
	// decoration — a client reconfigured with a zero ReadTimeout has
	// nothing else to release it, and the goroutines own nothing that
	// outlives the process, so returning beats waiting forever.
	//
	// It is now EXERCISED rather than merely reasoned about:
	// TestRedisSessionPresence_CloseDoesNotWaitForeverOnAParkedRenewal
	// parks a renewal inside its write through the onRenewWrite seam and
	// asserts Close still returns; removing the deadline hangs that test.
	// The first version of this comment claimed no reachable failing case
	// existed, which was true only of the failing case I had bothered to
	// construct.
	closeDrainTimeout = 10 * time.Second
)

// renewal is one session's renewal goroutine, held so Remove can both
// STOP it and WAIT for it.
//
// The wait is the load-bearing half (codex round 1, P2). Cancelling
// returns immediately, so a renewal already inside its write could
// complete AFTER Remove's DEL/SREM and re-create the entry — resurrecting
// a session that has just disconnected and leaving it in every instance's
// picker until the TTL lapses. A targeted push at that ghost publishes and
// reaches nobody while reporting delivery. Waiting is bounded: the
// goroutine's context is already cancelled when the wait begins, so any
// in-flight Redis command returns promptly rather than running to its own
// timeout.
type renewal struct {
	stop context.CancelFunc
	done chan struct{}
}

// pruneIndexScript removes index members whose session key is GONE,
// re-checking existence atomically inside Redis.
//
// ARGV[1] is the session-key prefix for this user, so the script can
// rebuild each candidate's key name; ARGV[2..] are the candidate session
// ids. Written this way rather than passing the full key names as KEYS
// because the member id is what SREM needs and the key name is derivable
// from it — passing both would let the two drift.
var pruneIndexScript = redis.NewScript(`
local removed = 0
for i = 2, #ARGV do
  if redis.call('EXISTS', ARGV[1] .. ARGV[i]) == 0 then
    removed = removed + redis.call('SREM', KEYS[1], ARGV[i])
  end
end
return removed
`)

// timeout is opTimeout with the production default applied, so a
// zero-valued struct (a test that only set the fields it cared about)
// still bounds its calls instead of blocking forever.
func (p *RedisSessionPresence) timeout() time.Duration {
	if p.opTimeout <= 0 {
		return presenceOpTimeout
	}
	return p.opTimeout
}

// renewLogInterval bounds how often a renewal failure is logged. Long
// enough that a sustained outage produces a readable trickle, short enough
// that a recovering one is visible within a minute.
const renewLogInterval = time.Minute

// warnRenewFailure logs a renewal failure at most once per
// renewLogInterval, carrying the number of failures it stands for.
//
// Deliberately not silent in between: the count is what tells an operator
// whether this is one flapping session or the whole replica, which is
// exactly the Redis-problem-vs-Pad-problem question the raw stream could
// not answer. The session id is included because the un-throttled version
// omitted it — a warning naming only the user could not be tied to a
// specific stuck target.
func (p *RedisSessionPresence) warnRenewFailure(err error, userID, sessionID string) {
	now := time.Now()

	p.mu.Lock()
	p.renewLog.suppressed++
	if !p.renewLog.last.IsZero() && now.Sub(p.renewLog.last) < renewLogInterval {
		p.mu.Unlock()
		return
	}
	count := p.renewLog.suppressed
	p.renewLog.suppressed = 0
	p.renewLog.last = now
	p.mu.Unlock()

	slog.Warn("session presence: failed to renew session entry; affected sessions may be unlisted and untargetable until it recovers",
		"error", err, "user_id", userID, "session_id", sessionID,
		"failures_since_last_log", count, "log_interval", renewLogInterval)
}

// drain is drainTimeout with the production default applied, for the same
// zero-value reason as timeout.
func (p *RedisSessionPresence) drain() time.Duration {
	if p.drainTimeout <= 0 {
		return closeDrainTimeout
	}
	return p.drainTimeout
}

// userIDKeyPrefix is sessionKey's prefix for one user — everything before
// the session id. Kept next to sessionKey so the two cannot drift.
//
// A METHOD, not a package function, since BUG-2724: the name depends on
// the installation's namespace, which is per-registry config. A package
// function would have had to read a global to do that, and a global is
// exactly how the three keyspaces would drift apart again.
func (p *RedisSessionPresence) userIDKeyPrefix(userID string) string {
	return p.keys.Name("session:" + userID + ":")
}

// sessionKey is the per-session entry: one key, one TTL, one owner.
func (p *RedisSessionPresence) sessionKey(userID, sessionID string) string {
	return p.userIDKeyPrefix(userID) + sessionID
}

// DEPLOYMENT SCOPING, inherited from internal/watchevents/redis_bus.go's
// note of the same name and restated here because this is a THIRD
// keyspace living under the same rule (codex round 3).
//
// These names carry the installation's PAD_REDIS_NAMESPACE when one is
// set, exactly as `pad:events:` / `pad:event_seq` / `pad:watchevents` do
// since BUG-2724, and are byte-identical to the historical flat names
// when it is not. So the rule is no longer unconditional: one Redis
// endpoint per installation, OR one endpoint with a distinct namespace
// per installation.
//
// Selecting different logical DBs is still not an alternative — pub/sub
// is not namespaced by DB at all, so the buses cross-feed regardless, and
// two installations sharing one DB would merge these session registries
// too. The namespace is the only mechanism that separates them.
//
// What that would cost HERE, stated precisely rather than alarmingly:
// both keys are scoped by user id, and user ids are UUIDs minted per
// installation, so a merged registry only exposes one installation's
// sessions to another where the same UUID exists in both — which in
// practice means a cloned database, not two independent deployments. The
// same condition gates the bus's cross-feed, since delivery is filtered on
// TargetUserID. It is a real hazard for a cloned install and not one for a
// coincidental collision.
//
// FIXED, for the namespace half, in BUG-2724 — and fixed the way the note
// above demanded: not by growing a prefix here, but through
// internal/redisns, one value built in cmd/pad/cmd_server.go and passed
// into all three constructors at once. The operational rule is now
// stateable in one sentence for every keyspace: names carry
// PAD_REDIS_NAMESPACE when it is set, and are byte-identical to the
// historical ones when it is not.
//
// STILL TRUE, and the reason the interim rule below has not been retired:
// Redis CLUSTER remains unsupported. Pad dials redis.NewClient, and these
// names carry no hash tags — a user's index and their session entries
// would hash to different slots, so ListForUser's MGET and the Lua
// scripts that reach session keys through ARGV would fail CROSSSLOT
// rather than degrade. The watch bus's publish script has the same
// problem across four keys, so tagging this keyspace alone would buy
// nothing. Deferred as one future unit (cluster client + hash tags
// everywhere + publish-script restructuring) on BUG-2724's trail.
//
// So the interim rule stands, minus the sharing half: ONE SINGLE-NODE
// REDIS ENDPOINT PER PAD INSTALLATION — or one shared endpoint with a
// distinct PAD_REDIS_NAMESPACE per installation. Not a cluster.
//
// sessionIndexKey is the per-user SET of that user's session ids. Scoped
// per user because every read is user-scoped — there is no "list all
// sessions on this server" consumer and there should not be one (see
// handleListSessions' doc comment).
func (p *RedisSessionPresence) sessionIndexKey(userID string) string {
	return p.keys.Name("sessions:" + userID)
}

// NewRedisSessionPresence returns a shared registry backed by client.
//
// The client is the SAME one the watch bus uses (cmd/pad/cmd_server.go
// dials once on PAD_REDIS_URL): they address the same server, and one
// connection pool serving two logical concerns is the shape internal/events
// already assumes.
func NewRedisSessionPresence(client *redis.Client) *RedisSessionPresence {
	return NewRedisSessionPresenceWithKeys(client, redisns.Default)
}

// NewRedisSessionPresenceWithKeys is NewRedisSessionPresence with an
// explicit key namespace (BUG-2724). cmd/pad/cmd_server.go uses this one,
// passing the value shared with both buses.
//
// Renaming presence keys is CHEAP, unlike the buses': entries are
// transient and expire on their TTL, so a namespace cutover strands
// nothing permanently. The orphaned entries under the OLD names live out
// that TTL — 90s, three renewal intervals, not the one an earlier version
// of this comment claimed — during which an old-keyspace replica still
// lists them.
func NewRedisSessionPresenceWithKeys(client *redis.Client, keys redisns.Keys) *RedisSessionPresence {
	return &RedisSessionPresence{
		client:        client,
		keys:          keys,
		sessionKeyTTL: sessionKeyTTL,
		renewInterval: sessionRenewInterval,
		opTimeout:     presenceOpTimeout,
		drainTimeout:  closeDrainTimeout,
		renewals:      make(map[string]*renewal),
	}
}

// Add implements SessionPresence.
//
// Returns the generated id even when the Redis write fails. That is
// deliberate and it is the same posture the rest of the push path takes
// toward presence: the caller is a live SSE handler that is about to serve
// a real connection, and it must not be torn down because an optional
// registry is unavailable. A session missing from the registry is
// under-reporting — the failure mode presence already documents — while
// refusing the connection would take away delivery the bus can still
// perform. The id is still the one watchNotificationVisible compares a
// targeted push against, so a broadcast push reaches this connection
// regardless.
func (p *RedisSessionPresence) Add(userID string, ident SessionIdentity) string {
	id := uuid.NewString()
	sess := LiveSession{
		ID:          id,
		Label:       ident.Label,
		PID:         ident.PID,
		Armed:       ident.Armed,
		ConnectedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(sess)
	if err != nil {
		// Cannot happen for this struct; logged rather than swallowed so a
		// future field that breaks marshalling is visible.
		slog.Error("session presence: marshal session", "error", err, "user_id", userID)
		return id
	}

	ctx, cancel := context.WithCancel(context.Background())
	rn := &renewal{stop: cancel, done: make(chan struct{})}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		return id
	}
	p.renewals[renewalKey(userID, id)] = rn
	p.wg.Add(1)
	p.mu.Unlock()

	writeCtx, writeCancel := context.WithTimeout(context.Background(), p.timeout())
	defer writeCancel()
	if err := p.write(writeCtx, userID, id, string(payload)); err != nil {
		// Logged, not fatal: renewLoop re-SETs the whole payload rather
		// than issuing a bare EXPIRE, so the next tick recovers a
		// registration that failed on a blip.
		slog.Warn("session presence: failed to register session; it will be missing from the picker until the next renewal",
			"error", err, "user_id", userID)
		p.reportOpFailed(PresenceOpRegister)
	}

	go p.renewLoop(ctx, rn, userID, id, string(payload))
	return id
}

// writeScript stores the session entry and indexes it, ATOMICALLY.
//
// Scripted rather than pipelined (codex round 14). A pipeline can apply
// partially — a dropped connection between commands leaves some sent and
// some not — and the two halves are not equally harmless, which an earlier
// version of this comment got wrong. An index member with no session key is
// pruned by the next reader, fine. But a session KEY with no INDEX MEMBER
// is a live session that ListForUser cannot see, because the index is the
// only enumeration: a targeted push at it is skipped and answers a clean
// delivered_sessions:0. That is precisely the failure BUG-2698 exists to
// remove, reintroduced by a partial write.
//
// It was self-healing — the next renewal re-runs this and re-indexes, so
// the window was one renewal interval rather than permanent — but a
// bounded reappearance of the bug is still the bug, and Redis executes a
// script atomically on its single thread, so there is no reason to accept
// the window at all.

var writeScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
redis.call('SADD', KEYS[2], ARGV[3])
redis.call('EXPIRE', KEYS[2], ARGV[2])
return 1
`)

// write stores the session entry and indexes it, both under the same TTL,
// in one atomic step. See writeScript.
func (p *RedisSessionPresence) write(ctx context.Context, userID, sessionID, payload string) error {
	return writeScript.Run(ctx, p.client,
		[]string{p.sessionKey(userID, sessionID), p.sessionIndexKey(userID)},
		payload, int(p.sessionKeyTTL.Seconds()), sessionID,
	).Err()
}

// renewLoop keeps this connection's entry alive for exactly as long as the
// connection is held open by THIS process.
//
// It re-SETs the payload rather than issuing a bare EXPIRE, so a renewal
// also RESTORES an entry that expired during a Redis outage or a long
// pause. A bare EXPIRE against a vanished key is a no-op that returns
// success, which would leave a live session permanently invisible with
// nothing in the logs.
func (p *RedisSessionPresence) renewLoop(ctx context.Context, rn *renewal, userID, sessionID, payload string) {
	defer p.wg.Done()
	// Closed AFTER the last write returns, which is what makes Remove's
	// wait meaningful — see the renewal type's doc comment.
	defer close(rn.done)
	ticker := time.NewTicker(p.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p.onRenewWrite != nil {
				p.onRenewWrite()
			}
			if err := p.write(ctx, userID, sessionID, payload); err != nil && ctx.Err() == nil {
				p.warnRenewFailure(err, userID, sessionID)
				// Counted on EVERY failure, while the log line above is
				// rate-limited (that throttle exists because a Redis
				// outage otherwise logs once per session per renewal —
				// roughly 33 lines a second per replica at a thousand
				// sessions). A counter has no such problem and wants the
				// true rate: throttling it too would make the metric
				// under-report exactly during the incident it exists for.
				p.reportOpFailed(PresenceOpRenew)
			}
		}
	}
}

// Remove implements SessionPresence: idempotent, and safe for a session id
// that was never added (the stream handler's defer runs on paths where Add
// may not have been reached).
func (p *RedisSessionPresence) Remove(userID string, sessionID string) {
	if sessionID == "" {
		return
	}
	p.mu.Lock()
	rn, ok := p.renewals[renewalKey(userID, sessionID)]
	if ok {
		delete(p.renewals, renewalKey(userID, sessionID))
	}
	closed := p.closed
	p.mu.Unlock()
	// After Close, the entry may already have been handed to Close's own
	// drain rather than found here. Waiting on the WaitGroup is what covers
	// that case: it does not return until every renewal goroutine has
	// finished its last write, which is the property the DEL below depends
	// on (codex round 6). Close bounds its own drain, so this cannot
	// outlive shutdown.
	if closed && !ok {
		// BOUNDED, like Close's own drain (codex round 10). An unbounded
		// wait here reintroduces exactly what the drain deadline exists to
		// prevent: if Close timed out on a parked renewal, that renewal is
		// still running, so waiting on it without a deadline would hold
		// this handler — and therefore http.Server.Shutdown — for as long
		// as the parked write lasts.
		//
		// Timing out means proceeding to the DEL below with a renewal
		// potentially still in flight, i.e. accepting the ghost this wait
		// exists to prevent. That is the right trade at this point: the
		// ghost expires on its TTL, while a hung shutdown does not resolve
		// at all.
		p.waitForDrain(p.drain())
	}
	if ok {
		// Stop AND wait, in that order and outside the lock. Stopping alone
		// leaves an in-flight renewal free to re-create the entry after the
		// delete below (codex round 1, P2).
		//
		// BOUNDED, and this is the branch that matters (codex round 11).
		// Round 10 bounded only the post-Close fallback below, which was
		// the wrong half: Close RETAINS its renewal entries, so a handler
		// unwinding during shutdown finds one here and takes this path —
		// and an unbounded `<-rn.done` against a parked renewal held
		// http.Server.Shutdown exactly as before. Same deadline, same
		// trade: a ghost that expires on its TTL beats a shutdown that
		// never returns.
		rn.stop()
		select {
		case <-rn.done:
		case <-time.After(p.drain()):
			slog.Warn("session presence: renewal did not stop before deregistration; the entry may briefly reappear until its TTL",
				"user_id", userID, "timeout", p.drain())
		}
	}

	// A fresh context, NOT the cancelled renewal one: this is the clean
	// deregistration a disconnect owes the registry, and it has to run
	// after the renewal has stopped. Reusing the cancelled context would
	// fail every command and leave the entry to expire on its TTL instead —
	// turning an immediate disconnect into a 90-second ghost in every other
	// instance's session picker.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()
	pipe := p.client.Pipeline()
	pipe.Del(ctx, p.sessionKey(userID, sessionID))
	pipe.SRem(ctx, p.sessionIndexKey(userID), sessionID)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("session presence: failed to deregister session; it will expire on its TTL",
			"error", err, "user_id", userID)
		p.reportOpFailed(PresenceOpDeregister)
	}
}

// ListForUser implements SessionPresence, oldest connection first with the
// session id as a tiebreaker — the same deterministic order
// MemorySessionPresence produces, because an unstable order would make the
// web target picker jump around under the user's cursor.
//
// A READ FAILURE IS RETURNED, never flattened into an empty list. An
// earlier version of this method did flatten it, on the reasoning that "a
// partial read is not an outage" — which conflated a nil registry (a
// configuration fact, known at startup) with a failed read (a runtime
// one), and no consumer can tell those apart from an empty slice anyway.
// Codex round 1 caught it: the flattened version made handleListSessions
// answer 200 with no sessions during a Redis outage, and made a targeted
// push skip its publish and lose the instruction while reporting success.
func (p *RedisSessionPresence) ListForUser(userID string) ([]LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout())
	defer cancel()

	ids, err := p.client.SMembers(ctx, p.sessionIndexKey(userID)).Result()
	if err != nil {
		// REPORTED, not swallowed into an empty list (codex round 1, P1).
		// An empty list means "nobody is listening", which makes a targeted
		// push skip its publish and lose the instruction; an outage means
		// "I cannot tell", which must make the caller decline to conclude
		// anything. Collapsing the two here is the same defect this type
		// was written to remove, one layer down.
		slog.Warn("session presence: failed to read session index", "error", err, "user_id", userID)
		p.reportOpFailed(PresenceOpList)
		return nil, fmt.Errorf("session presence: read index: %w", err)
	}
	if len(ids) == 0 {
		// EMPTY, not nil (codex round 5). MemorySessionPresence returns a
		// non-nil empty slice, and handleListSessions marshals whatever it
		// gets straight into `sessions` — so a nil here serialises as
		// `"sessions": null` where the other implementation produces
		// `"sessions": []`. A consumer that maps over the array gets a
		// runtime error against one implementation and not the other, which
		// is precisely the kind of cross-implementation divergence this
		// registry exists to remove.
		return []LiveSession{}, nil
	}

	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, p.sessionKey(userID, id))
	}
	values, err := p.client.MGet(ctx, keys...).Result()
	if err != nil {
		slog.Warn("session presence: failed to read session entries", "error", err, "user_id", userID)
		p.reportOpFailed(PresenceOpList)
		return nil, fmt.Errorf("session presence: read entries: %w", err)
	}

	out := make([]LiveSession, 0, len(values))
	var expired []string
	for i, v := range values {
		if v == nil {
			// The session key is GONE. Usually that means its TTL lapsed
			// because the process stopped renewing — it died without
			// deregistering — but it is not proof of that: an eviction
			// under maxmemory, a Redis restart, or a manual DEL produce
			// exactly the same nil, and this type's own doc comment says
			// eviction is indistinguishable from expiry. Nothing here
			// depends on telling them apart, since the response is the
			// same either way: treat the session as gone and prune the
			// index member so leftovers do not accumulate.
			//
			// The pruning itself is CONDITIONAL for precisely this
			// reason — a live session whose key was evicted can be
			// rewritten by its own renewal between this read and the
			// prune. See pruneIndexScript.
			expired = append(expired, ids[i])
			continue
		}
		raw, ok := v.(string)
		if !ok {
			// Same ruling as the decode failure below: a value we cannot
			// interpret is an UNKNOWN registry state, not an absent session.
			//
			// Counted as a list failure for consistency with the decode
			// branch below (codex round 1 P2), though this arm is
			// DEFENSIVE and not reachable through MGET: Redis answers nil
			// for a key holding a non-string value, so a wrong-typed entry
			// arrives as nil and is handled by the expired branch above.
			// Verified rather than assumed — a wrong-typed key returns
			// [nil] with no error. No test drives this line, and the
			// observer test says so rather than pretending otherwise.
			p.reportOpFailed(PresenceOpList)
			return nil, fmt.Errorf("session presence: session entry for %s is not a string", ids[i])
		}
		var sess LiveSession
		if err := json.Unmarshal([]byte(raw), &sess); err != nil {
			// REPORTED, not skipped (codex round 2, P2). An earlier version
			// dropped the row and returned a nil error, arguing that the
			// other sessions in the list were still real. That contradicted
			// the P1 ruling one round earlier, and in the same direction: a
			// silently-omitted session makes deliveredSessionCount report a
			// number that looks complete, so a targeted push at the omitted
			// session returns delivered_sessions:0 and is SKIPPED — the
			// instruction dropped while the caller is told nothing was
			// listening.
			//
			// The trade is deliberate: one corrupt entry now blinds this
			// user's whole listing (503 on the picker, null count on a
			// broadcast) until the row expires, at most one TTL. That is the
			// honest answer — we cannot say who is connected — and it
			// self-heals, where the silent version stayed wrong and looked
			// right.
			slog.Warn("session presence: undecodable session entry",
				"error", err, "user_id", userID, "session_id", ids[i])
			// Counted as a list failure (codex round 1 P2). This path
			// returns the same 503-producing error as a transport failure
			// and blinds the same picker; leaving it uncounted would make
			// pad_session_presence_failures_total under-report exactly the
			// corrupt-data case, which is the one an operator is least
			// likely to find any other way — a dead Redis is obvious, a
			// corrupt row is not.
			p.reportOpFailed(PresenceOpList)
			return nil, fmt.Errorf("session presence: decode entry %s: %w", ids[i], err)
		}
		out = append(out, sess)
	}
	if len(expired) > 0 {
		// CONDITIONAL, not a plain SREM (codex round 2, P2). Between the
		// MGet above and this call, a renewal can restore the very key we
		// observed missing — which happens exactly when it is most harmful:
		// a Redis outage long enough to expire entries, followed by
		// recovery, has every surviving instance rewriting its sessions at
		// once. An unconditional SREM would then evict a LIVE session from
		// the index, hiding it from the picker and making targeted pushes to
		// it skip, for up to a renewal interval. The script re-checks
		// existence inside Redis, where the check and the removal cannot be
		// interleaved.
		//
		// Still best-effort: a failure costs a retry on the next read, and
		// the members are already invisible to this listing either way.
		if err := pruneIndexScript.Run(ctx, p.client,
			[]string{p.sessionIndexKey(userID)},
			append([]interface{}{p.userIDKeyPrefix(userID)}, toAny(expired)...)...,
		).Err(); err != nil && !errors.Is(err, redis.Nil) {
			slog.Debug("session presence: failed to prune expired index members",
				"error", err, "user_id", userID)
			p.reportOpFailed(PresenceOpPrune)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	// Everything that could not be interpreted has already RETURNED an
	// error above — an undecodable entry included (codex round 2 changed
	// that, and an earlier version of this comment still described the
	// behaviour it replaced). Reaching here means every indexed session
	// either decoded or was pruned as expired, so this list is complete
	// as of the read.
	return out, nil
}

// Close stops every renewal goroutine and waits for them. It is NOT part of
// the SessionPresence interface — MemorySessionPresence has nothing to shut
// down — so callers hold the concrete type to call it. cmd/pad/cmd_server.go
// does exactly that during shutdown.
//
// Note what it deliberately does NOT do: it does not delete this instance's
// entries. A shutdown that raced a reconnect elsewhere would then delete a
// session that had already re-registered. Letting the TTL clear them is
// slower and correct.
func (p *RedisSessionPresence) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	// STOPPED, NOT DELETED (codex round 6). Deleting here removed the very
	// lookup Remove uses to decide whether to WAIT: a handler disconnecting
	// concurrently with Close found no renewal, skipped the wait, and ran
	// its DEL — leaving an already-cancelled-but-still-in-flight renewal
	// free to complete afterwards and re-create the key. That is round 1's
	// resurrection bug arriving by the other door, and the fix there (wait
	// for the goroutine) only works if Remove can still find it.
	//
	// The entries are dropped with the object instead. Close is the
	// shutdown path, so nothing is accumulating.
	for _, rn := range p.renewals {
		rn.stop()
	}
	p.mu.Unlock()

	if !p.waitForDrain(p.drain()) {
		// Bounded, not abandoned: every goroutine has already been
		// cancelled above, so this only fires when one is parked inside a
		// Redis call that its own client timeouts have not yet released.
		// Holding shutdown behind that is worse than leaving them to the
		// process exit — they own no state that outlives it.
		slog.Warn("session presence: renewal goroutines did not drain before shutdown; leaving them to process exit",
			"timeout", p.drain())
	}
}

// waitForDrain waits for every renewal goroutine to finish, up to timeout.
// Reports whether they all finished; false means at least one is still
// running and the caller is proceeding anyway.
// waitForDrain waits for renewal goroutines, bounded.
//
// The waiter goroutine LEAKS when the timeout fires with a renewal still
// blocked: nothing can cancel a wg.Wait, so it lives until the process
// exits. Accepted rather than fixed, and stated rather than left for the
// next reader to rediscover — this runs only from Close, so "until the
// process exits" is seconds away, and it is one goroutine per shutdown,
// not per session. The alternative (a cancellable wait) would mean
// tracking every renewal individually for a benefit that expires with
// the process.
func (p *RedisSessionPresence) waitForDrain(timeout time.Duration) bool {
	drained := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return true
	case <-time.After(timeout):
		return false
	}
}

func renewalKey(userID, sessionID string) string { return userID + "|" + sessionID }

func toAny(ss []string) []interface{} {
	out := make([]interface{}, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
