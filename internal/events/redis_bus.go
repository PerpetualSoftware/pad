package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// redisChannelSuffix is prepended to workspace IDs for Redis pub/sub
	// channels, under whatever namespace the installation configured —
	// "pad:events:" by default. See internal/redisns.
	redisChannelSuffix = "events:"

	// redisSeqSuffix names the global event sequence counter ("pad:event_seq"
	// by default). All instances share this counter so SSE event IDs are
	// globally ordered and Last-Event-ID is valid across any instance on
	// reconnect — which is also why RENAMING it (by introducing a namespace
	// on a running deployment) is a cutover rather than a config tweak:
	// the new counter starts from zero and connected clients' Last-Event-ID
	// values belong to the old space.
	redisSeqSuffix = "event_seq"

	// redisEpochSuffix identifies the CURRENT ID space ("pad:event_epoch" by
	// default), and exists because numeric detection alone cannot see a reset
	// that has already caught back up.
	//
	// A counter reset is detectable when an ID arrives at or below our
	// high-water mark. It is INVISIBLE when the new space has already climbed
	// past it: hold 100, lose the subscription, the counter resets and IDs
	// 1-101 are published, and the only one that reaches us is 101 — which
	// looks exactly like the contiguous successor of 100. The buffer then
	// mixes two ID spaces and a client resuming from OLD 100 is handed NEW 101,
	// having silently missed everything the old space had above 100.
	//
	// Same mechanism and the same reasoning as internal/watchevents'
	// watchevents_epoch, but NOT the same value shape: that one is an opaque
	// uuid minted by its publisher, this one is a monotonic generation minted
	// by Redis (see redisEpochGenSuffix for why comparability was needed here
	// and what an unordered token could not do). Deliberately a DISTINCT key
	// from that one for the same reason the counters are distinct: the two
	// buses carry independent Last-Event-ID spaces.
	redisEpochSuffix = "event_epoch"

	// redisEpochGenSuffix is the monotonic counter the epoch's value comes
	// from ("pad:event_epoch_gen" by default). It is INCRemented by Redis and
	// never deleted by Pad, which is what makes epochs COMPARABLE.
	//
	// Comparability is not decoration (codex round 3). Epochs were opaque
	// uuids at first, and an opaque token cannot say which of two spaces is
	// the later one — so a straggler carrying the OLD epoch, arriving on one
	// workspace's channel after a new-epoch message arrived on another's
	// (separate subscriptions, no ordering between them), flipped the bus back
	// to the dead space and dropped every buffer again. With a generation, an
	// arriving epoch is adopted only when it is strictly greater, and a
	// straggler from a space we have left is recognised as stale.
	//
	// A wall clock would have been the other way to order them and is the
	// wrong one: instances have different clocks, so a rotation minted on a
	// lagging machine could carry a LOWER stamp than the space it replaces and
	// be ignored forever — a silent failure, where this is a loud one.
	redisEpochGenSuffix = "event_epoch_gen"

	// redisDedupeSuffix namespaces the per-publish idempotency tokens.
	redisDedupeSuffix = "events:pub:"

	// redisDedupeTTLSeconds bounds how long a token is remembered. It only has
	// to cover a client-side retry burst — go-redis gives up after MaxRetries
	// with backoff measured in milliseconds — so a minute is generous, and the
	// keys are small and expire on their own.
	redisDedupeTTLSeconds = 60
)

// DEPLOYMENT SCOPING (BUG-2724). Every name above carries the installation's
// PAD_REDIS_NAMESPACE when one is set — see internal/redisns — and is
// byte-identical to the historical flat names when it is not. The rule, stated
// the same way in internal/watchevents and internal/server's presence registry
// because it belongs to all three at once: scoping comes from ONE shared
// config value built in cmd/pad/cmd_server.go, never from one package growing
// a prefix the others lack.
//
// STILL UNSCOPED, deliberately: Redis CLUSTER. No hash tags, and a non-cluster
// client. BUG-2736 adds TWO multi-key EVALs here — publishScript spans five
// keys (sequence, channel, epoch, dedupe, epoch generation) and assignScript
// spans two (sequence, epoch) — alongside the one already in
// internal/watchevents, so a cluster port now has three call sites that would
// fail CROSSSLOT rather than one. Same deferral, two more sites; BUG-2724
// holds the reasoning for shipping tags only alongside a cluster client that
// can test them.

// assignScript is the PHASE 1 id assignment: one INCR, plus the stale-epoch
// clear that must happen atomically with it. It publishes nothing — the bare
// wire form carries the id inside the JSON, so the caller marshals and
// publishes after this returns.
var assignScript = redis.NewScript(`
local id = redis.call('INCR', KEYS[1])
if id == 1 then
  redis.call('DEL', KEYS[2])
end
return id
`)

// generationRestartSeed is the value publishScript restarts the generation
// counter at when it finds that key corrupted (BUG-2740, ARGV[3]).
//
// Wall-clock SECONDS, per Dave's day-49 ruling: generations only have to be
// orderable among themselves, and a corrupted key cannot testify to what the
// previous generation was, so there is nothing to derive a safe increment
// from. Seconds are strictly above any plausible increment-from-1 history
// with no coordination and no stored state to trust.
//
// PASSED IN rather than read with redis.call('TIME') inside the script, for
// two reasons and not for a replication one: the script stays deterministic,
// and a test can inject a fixed seed and assert the exact restart value —
// which is what lets the mutation matrix tell "repaired" apart from
// "repaired to the wrong thing".
//
// Clock skew between publishers is harmless here. The script is atomic, so
// the first publisher to reach a corrupted key repairs it and every other
// publisher then finds a usable counter and simply increments; no two seeds
// are ever compared with each other.
//
// A seam rather than a direct call so tests can pin it.
func (b *RedisBus) generationRestartSeed() string {
	now := time.Now().Unix
	if b.nowUnix != nil {
		now = b.nowUnix
	}
	return clampGenerationSeed(now())
}

// clampGenerationSeed forces the seed into the shape publishScript's guards
// accept: a positive integer of at most 17 digits.
//
// THE CLOCK IS AN INPUT, AND A BROKEN ONE WAS FATAL (BUG-2740, codex round
// 7). An unset or misconfigured host clock can report zero or a negative
// second. The repair would then SET that at the generation key and return it
// as the epoch — and the epoch guard rejects anything not matching
// ^[1-9][0-9]*$, so it rotates, calls back into the repair, receives the SAME
// bad seed, and assigns it to the epoch WITHOUT revalidating. The result is
// an unparseable epoch on the wire, which every receiver rejects: the total,
// silent, unrecoverable drop the epoch guard exists to prevent, reached
// through the thing that was supposed to fix it.
//
// The floor is 1 rather than anything cleverer. It gives up the ruling's
// property — a seed above any counted history — because a broken clock cannot
// deliver that property at all, and the choice is then between a value that
// is merely LOW and one that is FATAL. A low generation is detected
// (epoch_regressed) and costs a round of resyncs; an invalid one drops every
// event until a human intervenes.
//
// The ceiling exists for the same reason as next_gen's: a value over 17
// digits is not usable as a generation, so seeding one would repair the key
// into a state the very next rotation rejects. Unix seconds cannot reach it
// by elapsing, but a misconfigured clock is exactly what this function exists
// to survive.
func clampGenerationSeed(unix int64) string {
	const maxSeed = 99999999999999999 // 17 digits, next_gen's ceiling
	switch {
	case unix < 1:
		return "1"
	case unix > maxSeed:
		return strconv.FormatInt(maxSeed, 10)
	default:
		return strconv.FormatInt(unix, 10)
	}
}

// publishScript assigns the ID and publishes in ONE atomic Redis call. It is
// PHASE 2 ONLY: an instance that has not been flipped still publishes through
// the two-call path in Publish, because the bare wire form carries the ID
// INSIDE the JSON and the JSON must therefore be marshalled after the ID is
// known. See config.EventsPublishEpoch for the rollout order.
//
// WHY ATOMIC. The two-call version does INCR and PUBLISH as separate
// round-trips, which lets two instances interleave — INCR 5, INCR 6, PUBLISH
// 6, PUBLISH 5 — so a receiving instance can append 6 before 5 and corrupt the
// ordering replayBuffer.since() assumes when it computes oldest and newest.
// That window is older than this fix and was already wrong; it becomes
// load-bearing here, because counter-backwards detection reads a descending ID
// as a RESET, and under the two-call version every interleave would look like
// one. Redis runs a script atomically on its single thread, so publish order
// equals ID order globally with no coordination on our side.
//
// THE EPOCH AND ID ARE PREPENDED as "<epoch>|<id>|<json>" rather than injected
// into the JSON. Two reasons, and the second is the one a future refactor to
// "cleaner JSON" would regress: string-editing JSON inside Lua is fragile, and
// an envelope object would be UNMARSHALLED SILENTLY by an older instance
// during a mixed roll — no matching keys, no error, a zero-valued Event
// delivered to that instance's clients. The prefix fails loudly instead. See
// decodePayload for the receiving side, which accepts both forms.
//
// THE EPOCH'S VALUE IS MINTED BY REDIS, from a monotonic generation counter,
// rather than proposed by the caller. Two reasons, and the first is a
// correctness one (codex round 3): a caller-proposed uuid cannot be COMPARED,
// so a straggler carrying an abandoned epoch was indistinguishable from a
// genuine rotation and flipped the bus back into a dead space. A generation
// makes "is this later than what I have" answerable. The second is that
// minting inside the script removes the propose-then-SET-NX race entirely —
// two publishers can no longer both believe they minted the space.
//
// A DEDUPE TOKEN, matching internal/watchevents' script, and it is REQUIRED BY
// THE SCRIPT rather than a separate improvement bundled in (codex round 7
// asked whether it should ship here at all — it must, and cutting it while
// keeping the script would ship a regression).
//
// The mechanism: go-redis retries a command whose reply was lost to a network
// error — a Redis failover being the obvious trigger — so the script can run,
// publish, and still return an error to its caller. THE TWO PATHS DIFFER IN
// WHAT A RETRY COSTS. Phase 1 retries a PUBLISH whose payload already carries
// its ID, so a duplicate arrives under the SAME ID and a client's cursor logic
// can see it for what it is. Phase 2's retry re-runs the assignment, so the
// duplicate arrives under a SECOND ID, ascending and correctly ordered, and
// nothing downstream can tell the two apart. Moving assignment into the script
// is what makes retries worse; the token is what keeps them from being.
//
// It is not merely a duplicate row: the web layout raises a toast for any
// externally-sourced item_created, so a duplicate is a duplicate toast plus a
// redundant fetch.
//
// The token is CHECKED FIRST AND WRITTEN LAST, which is not the obvious order
// and is the one that matters (codex round 11). Written first, any error later
// in the script — Redis runs Lua atomically against interleaving, NOT with
// rollback — would leave the token behind on a run that never published, and
// the retry would then decline: the event lost, permanently and silently, with
// the caller told it succeeded. Written after the PUBLISH, a script that dies
// early leaves no token and the retry does the right thing, while a script
// that completed and merely lost its reply leaves one and the retry declines.
// The remaining window is an error on the final SET itself, whose key is a
// fresh uuid and so cannot be wrong-typed; its cost would be a duplicate
// rather than a loss.
//
// WHAT IT DOES NOT COVER, so nobody reads it as a guarantee (codex round 6): a
// retry that lands on a DIFFERENT Redis. If the original primary executed the
// script and then failed over before the token replicated, the promoted
// replica has no such key and the retry publishes a second copy under a second
// ID. Nothing downstream can tell the two apart — both are valid and ascending
// — and the client is not told. The token is as durable as Redis replication
// and no more; this narrows the window rather than closing it.
var publishScript = redis.NewScript(`
-- next_gen returns the next generation for the id space, REPAIRING the
-- generation counter first if it holds something INCR cannot work with
-- (BUG-2740).
--
-- Every INCR of KEYS[5] goes through here, and the reason is that an INCR
-- against a corrupted key ABORTS THE SCRIPT — after the INCR of KEYS[1] has
-- already advanced the sequence. Redis is atomic against interleaving but
-- does not roll back a script's earlier writes on an error, so the failure
-- burns an id (a hole to every receiver), repeats on the next publish, and
-- never self-heals, because the branch that would rotate the generation is
-- the branch that cannot run.
--
-- FOUR WAYS IT ABORTS, all measured against the pinned miniredis rather than
-- assumed, because the filing named only the first two:
--
--   list  -> WRONGTYPE
--   hash  -> WRONGTYPE
--   string 'abc'                 -> ERR value is not an integer or out of range
--   string '9223372036854775807' -> ERR increment or decrement would overflow
--
-- So a TYPE check alone would have covered half of them. The value has to be
-- validated too, on exactly the terms the epoch key's guard uses next to it:
-- a positive integer of at most 18 digits, which is what the RECEIVER's
-- strconv.ParseInt can read back.
--
-- REPAIR IS SET, NOT DEL: SET replaces a key of any type (measured), and
-- BUG-2736's mutation matrix already established here that a DEL is
-- removable without any test noticing.
local function next_gen()
  local usable = false
  local t = redis.call('TYPE', KEYS[5])['ok']
  if t == 'none' then
    usable = true
  elseif t == 'string' then
    local v = redis.call('GET', KEYS[5])
    -- 17, NOT 18, and the difference is derived rather than chosen (codex
    -- round 4). This value is about to be INCREMENTED and the result becomes
    -- the EPOCH, which the guard further down rejects above 18 digits. Accept
    -- 18 here and a counter at 999999999999999999 increments to a 19-digit
    -- epoch, that guard fires, and the script rotates a SECOND time inside
    -- one publish — finding a 19-digit generation, repairing it to the
    -- wall-clock seed, and publishing a generation far BELOW the one
    -- receivers hold. Measured: seeded at 18 digits the published epoch came
    -- back as the seed; at 17 digits it came back as the ordinary increment.
    --
    -- So the ceiling for what is USABLE is one digit under the ceiling for
    -- what is PUBLISHABLE. A value at or above it is treated as corrupted and
    -- repaired once, which is a single detected rotation instead of two.
    if string.match(v, '^[1-9][0-9]*$') and #v <= 17 then
      usable = true
    end
  end
  if not usable then
    -- RESTARTED AT WALL-CLOCK SECONDS, not at 1 (Dave's ruling, day-49).
    -- Generations only have to be orderable among THEMSELVES, and the
    -- corrupted key is the only witness to what the previous one was, so it
    -- cannot testify. A wall-clock seed is strictly above any plausible
    -- increment-from-1 history with no coordination and nothing stored to
    -- trust. Restarting at 1 would make the new generation LOWER than ones
    -- receivers have already adopted, which BUG-2736's design reads as a
    -- regression.
    --
    -- The seed arrives as ARGV[3] rather than from redis.call('TIME') so the
    -- script stays deterministic and the exact restart value is assertable in
    -- a test. (TIME does work here — measured — but nothing needs it to.)
    --
    -- WHAT THE SEED DOES NOT GUARANTEE, stated because 'strictly above any
    -- increment-from-1 history' is the ruling's premise and is not the same
    -- as monotonic (codex round 2). A host clock stepped BACKWARDS, or the
    -- key corrupted a second time within the same second, can seed a
    -- generation at or below the one receivers already hold. Two things bound
    -- it. Consecutive repairs are already safe without the clock: the first
    -- one leaves a VALID counter, so the next publisher increments it rather
    -- than reseeding. And a generation that does go backwards is DETECTED
    -- rather than silent — BUG-2736's receivers report epoch_regressed and
    -- stop vouching for their buffers, which costs a round of resyncs and
    -- never merges two id spaces.
    --
    -- The stronger repair would be max(seed, current_epoch + 1), since the
    -- epoch key is a second witness to the generation in use. RULED AGAINST
    -- (lead, day-55), and the deciding reason is not that it is partial: a
    -- repair path that reads a NEIGHBOURING shared key to compute its seed
    -- takes a dependency on that neighbour's health in exactly the state
    -- where neighbours are suspect. It would be load-bearing on the thing
    -- that just failed. It also only helps where the epoch is readable, which
    -- excludes the branch that fires BECAUSE the epoch is corrupted.
    --
    -- The residual is accepted as inside the ruling's intent — orderability
    -- without coordination — because it is bounded to one resync round,
    -- detected rather than silent, and never merges two id spaces. Reversible
    -- in one line if that judgement changes.
    redis.call('SET', KEYS[5], ARGV[3])
    return ARGV[3]
  end
  -- READ THE VALUE BACK rather than stringifying the INCR result. Redis
  -- returns an integer reply to Lua as a NUMBER, and Lua 5.1 numbers are
  -- doubles printed with %.14g — so tostring() stops being faithful before
  -- the 18 digits this guard admits. Measured at the boundary: a counter at
  -- 999999999999999998 increments to 999999999999999999, and tostring()
  -- renders it 1000000000000000000 while GET returns it exactly. Publishing
  -- the stringified form would put a generation on the wire that does not
  -- match the one in the key.
  redis.call('INCR', KEYS[5])
  return redis.call('GET', KEYS[5])
end

if redis.call('EXISTS', KEYS[4]) == 1 then
  return 0
end
local id = redis.call('INCR', KEYS[1])
if id == 1 then
  -- The counter is starting from scratch: this installation's first publish
  -- ever, or the seq key was deleted or evicted under us. Both are a NEW id
  -- space, so the epoch is ROTATED unconditionally. Without this, a deleted
  -- seq key restarts the ids inside the SAME epoch and the epoch check reports
  -- nothing -- and the numeric check misses it too whenever a receiver's
  -- high-water mark is low enough that the restarted counter climbs past it
  -- before that receiver sees anything.
  local g = next_gen()
  redis.call('SET', KEYS[3], g)
elseif redis.call('EXISTS', KEYS[3]) == 0 then
  -- No epoch yet for a sequence already in flight: the installation's first
  -- flipped publish, or a phase-1 instance cleared a stale one. Mint the next
  -- generation. Inside the script, so two publishers cannot both mint.
  local g = next_gen()
  redis.call('SET', KEYS[3], g)
end
local epoch = false
if redis.call('TYPE', KEYS[3])['ok'] == 'string' then
  epoch = redis.call('GET', KEYS[3])
end
if not epoch or not string.match(epoch, '^[1-9][0-9]*$') or #epoch > 18 then
  -- The epoch key holds something that is not a positive generation --
  -- corrupted, hand-edited, or written by another installation sharing this
  -- keyspace. Emitting it would make every receiver reject the payload and
  -- drop the event, forever and for every publisher. Rotating instead makes
  -- the state self-healing: one generation change, one round of resyncs, and
  -- the space is identifiable again.
  --
  -- The TYPE CHECK is part of the same guard for the same reason (codex round
  -- 20): a bare GET on a key holding a list or a hash raises WRONGTYPE, which
  -- aborts the script before this branch can run — so the recovery written to
  -- handle a corrupted epoch was unreachable for one of the ways an epoch gets
  -- corrupted. No DEL is needed to clear it: SET replaces a key of any type,
  -- which the mutation matrix established by finding that a DEL here could be
  -- removed without any test noticing.
  --
  -- The LENGTH cap is part of the same guard, not a separate nicety: the
  -- receiver parses this with Go's strconv.ParseInt, so a value that is all
  -- digits but overflows int64 fails there while passing a pattern match here
  -- -- the same total, silent, unrecoverable drop by a different route. Any
  -- 18-digit number fits in an int64, and a generation counts installations'
  -- id-space resets, so 18 digits is not a bound anything real approaches.
  local g = next_gen()
  redis.call('SET', KEYS[3], g)
  epoch = g
end
-- NOTE (BUG-2744): 'id' is a Lua NUMBER here, so this concatenation has the
-- same %.14g precision hazard next_gen avoids by reading its value back.
-- Reachable by corruption rather than by counting — a hand-edited or collided
-- event_seq ARRIVES at that magnitude the same way the generation key does.
--
-- Left for that item rather than fixed here, and the reason is NOT that the
-- return value constrains it: this caller discards the result (.Err()), so a
-- wrong id here reaches the wire with nothing on the Go side to notice.
-- assignScript's id IS consumed, which is why the remedy spans both paths and
-- is that item's design question.
redis.call('PUBLISH', KEYS[2], epoch .. '|' .. id .. '|' .. ARGV[1])
redis.call('SET', KEYS[4], '1', 'EX', ARGV[2])
return id
`)

// RedisBus distributes events across multiple Pad instances via Redis pub/sub.
// Each instance subscribes to Redis channels for its locally-connected SSE clients,
// and publishes events to Redis so all instances see them.
type RedisBus struct {
	// observable carries the optional operational-event seam (BUG-2731).
	observable

	client *redis.Client

	// nowUnix overrides the wall clock behind generationRestartSeed. Nil in
	// every real construction; set only by tests, so the exact value a
	// corrupted generation counter is repaired to can be asserted rather than
	// bounded (BUG-2740).
	nowUnix func() int64

	// keys builds this installation's Redis names (BUG-2724). The zero
	// value is the historical un-namespaced keyspace, so a bus constructed
	// without one behaves exactly as it always did.
	keys redisns.Keys

	// mu guards subscriber membership, the per-workspace subscription
	// bookkeeping, AND the replay buffers TOGETHER.
	//
	// The buffers used to have their own RWMutex, and that separation is
	// what made BUG-2731's case 4 unfixable in isolation. stopRedisSubscription
	// does not JOIN the receive goroutine — sub.cancel() only signals it — so a
	// straggler already inside fanOutLocally could re-create a buffer we had
	// just dropped, with knownFrom set to its own ID, and the resulting
	// one-entry buffer would then vouch for coverage it never had. One lock
	// makes "is this workspace still being received?" and "append to its
	// buffer" a single atomic decision.
	//
	// Holding a write lock through the fan-out cannot STALL anyone, for the
	// reason internal/watchevents records for its own single-mutex design: the
	// sends are already non-blocking (select/default — a full channel is
	// dropped-and-logged, never awaited). It does serialize fan-outs, resumes
	// and map work against each other, which is a real cost — and is why the
	// subscriber map is indexed by workspace: the critical section is then
	// proportional to one workspace's subscribers rather than to all of them.
	mu sync.Mutex
	// subscribers is indexed BY WORKSPACE, not a flat set (codex round 6).
	// Fan-out runs under the same lock as everything else here, so scanning
	// every local subscriber to filter by workspace would make one hot
	// workspace's publish rate the serialization point for every other
	// workspace's fan-out AND for every resume. The index makes the critical
	// section O(subscribers in THIS workspace) with non-blocking sends.
	//
	// MemoryBus keeps its flat map deliberately: it has an RWMutex and a
	// separate replay lock, so its fan-out does not exclude anyone.
	subscribers map[string]map[chan Event]*subscriber
	// workspaceOf resolves a channel back to its workspace on Unsubscribe,
	// which is the one operation that has only the channel to go on.
	workspaceOf map[chan Event]string

	// Track which workspace channels we're subscribed to in Redis,
	// so we subscribe/unsubscribe as local SSE clients come and go.
	wsCounts map[string]int       // workspace → local subscriber count
	wsSubs   map[string]*redisSub // workspace → active Redis subscription
	// pendingSubs holds the in-flight establishment for a workspace that has
	// no live subscription yet, so concurrent first subscribers share one
	// (BUG-2747). An entry here and an entry in wsSubs are mutually exclusive
	// in the steady state, but BOTH exist briefly: establishment installs
	// wsSubs before it waits for the confirmation, and only then clears this.
	pendingSubs map[string]*pendingSub

	// subGen numbers subscriptions so a message can be matched to the one it
	// arrived on. See fanOut for the race it closes.
	subGen int64

	// Per-workspace replay buffers for Last-Event-ID support.
	// Populated from events received via Redis pub/sub. Guarded by mu.
	replayBuffers map[string]*replayBuffer
	replaySize    int

	// confirmTimeout bounds how long Subscribe waits for Redis to acknowledge
	// a new subscription before admitting its callers anyway (BUG-2747).
	//
	// A BOUND, NOT A DEADLINE TO MEET: the acknowledgement is one round trip
	// on a connection that has just been dialled, so on a healthy Redis this
	// expires never. It exists so that an unreachable or stalled Redis
	// degrades to today's behaviour — admitted, no coverage claimed, and told
	// to reconcile when the acknowledgement lands — instead of refusing
	// connections and amplifying the outage.
	confirmTimeout time.Duration

	// heartbeatInterval is T and idleTimeout is 3T: how often this instance
	// publishes a liveness frame per subscribed workspace, and how long a
	// subscription may receive nothing at all before its coverage ends and its
	// connection is replaced (BUG-2738). Tunables with the ruled defaults; see
	// DefaultHeartbeatInterval and cycleIdleSubscriptions.
	heartbeatInterval time.Duration
	idleTimeout       time.Duration

	// heartbeatKick and idleKick wake their loops when the cadence above
	// changes, so a new interval takes effect at once instead of after the old
	// one expires. Buffered depth 1 and written non-blockingly: each is a
	// signal that the values moved, not a queue of changes.
	//
	// ONE PER LOOP because the two run in separate goroutines (see
	// maintenanceLoop); a shared channel would be consumed by whichever was
	// waiting and leave the other on the stale cadence.
	heartbeatKick chan struct{}
	idleKick      chan struct{}

	// maintenanceStopped is closed when maintenanceLoop returns.
	//
	// IT EXISTS BECAUSE THE LOOP'S TEARDOWN IS OTHERWISE UNOBSERVABLE, which
	// makes it untestable and therefore unprotected. Close drains wsSubs, so a
	// loop that ignored b.ctx entirely would find no workspaces and publish
	// nothing — indistinguishable from a loop that stopped, while it went on
	// waking every interval for the life of the process. Same reason
	// Observer.ReceiveLoopExited exists for the receive goroutines.
	maintenanceStopped chan struct{}

	// publishHeartbeat selects whether this instance EMITS liveness frames:
	// PHASE 2 of the heartbeat rollout. Receiving instances recognise and
	// ignore them from the release that introduced this field, so emission is
	// the half that is gated — see config.EventsHeartbeat for why the order is
	// not optional. Constructor parameter with no default, the same shape as
	// publishEpoch, so every call site states which phase it is in.
	publishHeartbeat bool

	// nowFunc overrides the clock behind idle detection. Nil in every real
	// construction; tests set it so a 90s threshold can be crossed without
	// sleeping through one. Distinct from nowUnix, which seams a different
	// clock for a different reason (BUG-2740's generation repair).
	nowFunc func() time.Time

	// afterSubscribeRegister is a TEST SEAM, nil in production. It runs
	// inside SubscribeAndReplaySince's critical section, after the subscriber
	// is registered and before the replay is read — the only point at which
	// that method's guarantee (an event is in the replay OR on the channel,
	// never both) is observable. A hook that publishes must do so from
	// another goroutine: b.mu is held here, so publishing inline would
	// deadlock, which is itself the property under test. Mirrors the seam on
	// MemoryBus and on watchevents.RedisBus.
	afterSubscribeRegister func()

	// afterSubscriptionConfirmed is a TEST SEAM, nil in production. It runs in
	// establishSubscription after Redis has acknowledged the subscription and
	// BEFORE the establishing caller re-acquires b.mu to read its replay —
	// the one moment at which the workspace is receiving events while that
	// caller's subscriber is registered but not yet ADMITTED.
	//
	// That gap is the only place the replay ceiling can be observed doing its
	// job, so it is the only place a test can prove BOTH halves of what the
	// split put at risk: that a resuming caller does not get an in-gap event
	// twice, and that a FRESH caller — which reads no replay at all — still
	// gets it once. b.mu is NOT held here, so a hook may publish inline.
	afterSubscriptionConfirmed func()

	// beforeUnconfirmedMark is a TEST SEAM, nil in production. It runs in
	// markUnconfirmedAdmission BEFORE it takes b.mu, which is the only way to
	// make the timer-versus-acknowledgement race deterministic: a hook that
	// waits for the acknowledgement here reproduces, every time, the interleave
	// where the select chose the timer but confirmSubscription has already run.
	//
	// REPETITION DOES NOT SUBSTITUTE FOR IT, measured rather than assumed.
	// Against the mutation that removes the confirmClosed re-check, 500
	// establishments per run caught it in 0 of 10 runs — a near-zero bound
	// makes the timer win OUTRIGHT far more often than it ties, and winning
	// outright is the ordinary timeout path. With this seam: 10 of 10.
	beforeUnconfirmedMark func()

	// afterRegisterBeforeEstablish is a TEST SEAM, nil in production. It runs
	// in subscribeAndReplay after section 1 has registered the subscriber and
	// (if this caller is the establisher) created the establishment record,
	// and BEFORE the establish loop runs.
	//
	// POSITIONAL, like its siblings (BUG-2749). It exists to place a
	// cancellation in the one window where the caller OWNS an establishment
	// record it has not yet begun — the window in which an early return
	// strands every later subscriber for that workspace. A test that cannot
	// land a cancellation exactly here cannot tell that regression from a
	// correct abandon. Receives the workspace.
	afterRegisterBeforeEstablish func(workspaceID string)

	// beforeInstallSubscription is a TEST SEAM, nil in production. It runs in
	// establishSubscription after the dial and BEFORE the lock that decides
	// whether to install or abandon, so a test can make either abandon reason
	// true at exactly the moment the decision is taken. Receives the workspace.
	beforeInstallSubscription func(workspaceID string)

	// afterInstallSubscription is a TEST SEAM, nil in production. It runs in
	// establishSubscription once the subscription is INSTALLED and its receive
	// loop started, and BEFORE the acknowledgement wait begins.
	//
	// ITS VALUE IS ENTIRELY POSITIONAL (BUG-2749), the same way
	// afterSubscribeRegister's is. The two cancellation cases this unit has to
	// separate — before the install and during the wait — differ only in where
	// the caller's context dies relative to this exact point, and a test that
	// cannot place a cancellation on the far side of it is testing whichever
	// case the scheduler happened to produce. If the install and the wait are
	// ever restructured, this call moves with that boundary or the tests named
	// for the after-install case silently stop exercising it. Receives the
	// workspace.
	afterInstallSubscription func(workspaceID string)

	// publishEpoch selects the wire form this instance EMITS: the phase-2
	// "<epoch>|<id>|<json>" prefix when true, the historical bare JSON body
	// when false. Receiving accepts both regardless — see decodePayload and
	// config.EventsPublishEpoch for why emission is the half that is gated.
	publishEpoch bool

	// epoch is the GENERATION of the ID space this instance has adopted,
	// learned from arriving messages rather than read at startup. Zero until a
	// prefixed message arrives, which on a phase-1 deployment is never.
	// Guarded by mu, and MONOTONIC — it only ever moves up; see the adoption
	// rule in fanOut and redisEpochGenSuffix for what an unordered token could
	// not do.
	//
	// A TRAVELLING GENERATION, NOT A NUMERIC ID BASE, and the asymmetry with
	// MemoryBus is deliberate rather than drift — stated once in
	// internal/idspace's package comment. Its cost, which belongs here: a
	// CURSOR still carries no space of its own, so an old and a new ID of the
	// same value remain indistinguishable to a resume even though this bus's
	// buffers can no longer mix them.
	//
	// LEARNED, NOT FETCHED, deliberately: reading the key at construction
	// would make an instance believe it belongs to a space whose events it has
	// not received, which is precisely the claim BUG-2731 spent a unit
	// removing. The epoch matters only in relation to buffered events, so it
	// arrives with them.
	epoch int64

	// hadReset and discardedHighWater record that this bus has thrown a
	// sequence away, and how high the discarded buffers had climbed. Every
	// buffer built afterwards refuses cursors at or below that mark — see
	// newBuffer and dropAllBuffers, where the two reset reasons set them
	// differently on purpose. Guarded by mu.
	hadReset           bool
	discardedHighWater int64

	// epochAdoptedAt is when this instance last moved to a new generation.
	// It bounds how long a LOWER generation is read as an in-flight straggler
	// rather than as the world moving backwards — see the regression branch in
	// fanOut. Zero before any generation is adopted. Guarded by mu.
	epochAdoptedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// redisSub tracks an active Redis subscription for a workspace.
type redisSub struct {
	pubsub *redis.PubSub
	cancel context.CancelFunc
	// gen identifies this subscription among all subscriptions this bus has
	// opened for the workspace, so a message carrying a stale gen can be
	// recognised as belonging to a subscription that has already ended.
	gen int64

	// lastSeen is when this subscription last received ANYTHING from Redis:
	// an event, a heartbeat, or a subscription confirmation. Guarded by b.mu.
	//
	// WHAT IS BEING MEASURED IS WHETHER THE SOCKET CARRIES TRAFFIC, not
	// whether the workspace is busy — which is why every inbound frame stamps
	// it rather than only the ones that turn into events, and why it is
	// stamped at INSTALL time too. A zero value would read as 1970 and cycle a
	// subscription that has simply not been given the chance to receive
	// anything yet; see cycleIdleSubscriptions' rule 2.
	lastSeen time.Time

	// confirmed is closed by receiveMessages when Redis acknowledges the
	// SUBSCRIBE for this subscription (BUG-2747). Subscribe waits on it — up to
	// confirmTimeout, after which it admits anyway and says so, see
	// markUnconfirmedAdmission — and that wait is what makes the window between
	// the SUBSCRIBE being written and it being registered unreachable by a
	// client.
	//
	// SIGNALLED FROM INSIDE THE RECEIVE LOOP rather than by a Receive call
	// placed ahead of it, and that is load-bearing rather than stylistic.
	// receiveMessages treats its FIRST *redis.Subscription as this initial
	// acknowledgement and skips it; every later one is a RESUBSCRIPTION and
	// ends the workspace's coverage (BUG-2739). Consuming the first
	// acknowledgement with a Receive before the loop starts — the shape
	// internal/watchevents uses in its once-per-process constructor — would
	// leave the loop treating the first GENUINE resubscription as the
	// initial one and silently skipping it, un-fixing BUG-2739 from a diff
	// that appears only to have added a wait.
	confirmed chan struct{}
	// confirmClosed guards confirmed against a second close. Guarded by b.mu.
	confirmClosed bool

	// unconfirmedAdmitted records that the confirmation did not arrive within
	// the bus's bound and subscribers were admitted anyway. When the
	// acknowledgement eventually lands, that span becomes a hole those
	// subscribers sat through, so they are told to reconcile. Guarded by b.mu.
	unconfirmedAdmitted bool
}

// pendingSub is the one-establisher-per-workspace record. Concurrent first
// subscribers for the same workspace must not each open their own Redis
// subscription — that is two connections, two receive loops and every event
// delivered twice — so exactly one caller establishes and the rest wait here.
//
// Held in b.pendingSubs for the duration of establishment and removed when it
// finishes, successfully or not.
type pendingSub struct {
	// done is closed when establishment has finished and the subscription is
	// installed (or has definitively failed). Waiters read the replay buffer
	// only after it closes.
	done chan struct{}
}

// NewRedisBus creates a new Redis-backed EventBus.
// The provided redis.Client should already be configured and connected.
func NewRedisBus(client *redis.Client) *RedisBus {
	return NewRedisBusWithKeys(client, redisns.Default, false, false)
}

// NewRedisBusWithKeys is NewRedisBus with an explicit key namespace
// (BUG-2724). cmd/pad/cmd_server.go uses this one, passing the value
// shared with the watch bus and the presence registry so all three
// keyspaces carry the same namespace or none.
//
// publishEpoch selects the wire form this instance EMITS (BUG-2736). It is a
// constructor parameter with no default rather than a setter, so every call
// site states which phase of the rollout it is in and none can flip a bus that
// is already publishing. See config.EventsPublishEpoch for the order the two
// phases must be rolled in.
func NewRedisBusWithKeys(client *redis.Client, keys redisns.Keys, publishEpoch, publishHeartbeat bool) *RedisBus {
	ctx, cancel := context.WithCancel(context.Background())
	b := &RedisBus{
		client:             client,
		keys:               keys,
		publishEpoch:       publishEpoch,
		publishHeartbeat:   publishHeartbeat,
		subscribers:        make(map[string]map[chan Event]*subscriber),
		workspaceOf:        make(map[chan Event]string),
		wsCounts:           make(map[string]int),
		wsSubs:             make(map[string]*redisSub),
		pendingSubs:        make(map[string]*pendingSub),
		replayBuffers:      make(map[string]*replayBuffer),
		replaySize:         DefaultReplayBufferSize,
		confirmTimeout:     defaultSubscribeConfirmTimeout,
		heartbeatInterval:  DefaultHeartbeatInterval,
		idleTimeout:        DefaultIdleTimeout,
		heartbeatKick:      make(chan struct{}, 1),
		idleKick:           make(chan struct{}, 1),
		maintenanceStopped: make(chan struct{}),
		ctx:                ctx,
		cancel:             cancel,
	}
	// NOT STARTED AT ALL ON PHASE 1 (codex round 4, P3). Both halves are gated
	// on publishHeartbeat and would be guaranteed no-ops there, so the loop
	// would be two goroutines and two timers per process waking every 30s for
	// the life of a deployment that has asked for none of it — and the DEFAULT
	// deployment is phase 1. The flag is constructor-only, so this decision can
	// be taken once and cannot go stale.
	//
	// The in-function gates stay regardless: they are the correctness ones
	// (see cycleIdleSubscriptions for what a phase-1 detector does to a quiet
	// workspace), and direct callers — the tests — reach them without a loop.
	if publishHeartbeat {
		go b.maintenanceLoop()
	} else {
		// Nothing will ever run, so the teardown signal is already true; a
		// caller waiting on it must not hang.
		close(b.maintenanceStopped)
	}
	return b
}

// Subscribe registers a local subscriber for the given workspace.
// Starts a Redis subscription for the workspace if this is the first local
// subscriber, and waits for Redis to acknowledge it before returning. The wait
// is bounded twice over: past confirmTimeout it returns anyway, counted and
// logged, with the subscriber told to reconcile when the acknowledgement
// lands; and if ctx ends first it returns SubscribeCancelled having undone its
// registration, leaving the rest of the wait to run for any joiners
// (BUG-2749).
//
// NO PRODUCTION CALLER, verified repo-wide: the SSE handler reaches
// SubscribeIfAllowed or SubscribeAndReplaySince. It is interface surface and
// test surface, routed through the same path as the other two so it cannot
// drift into being the one door with the old semantics.
func (b *RedisBus) Subscribe(ctx context.Context, workspaceID string) (chan Event, <-chan struct{}, SubscribeOutcome) {
	ch, _, gaps, outcome := b.subscribeAndReplay(ctx, workspaceID, 0, 0)
	return ch, gaps, outcome
}

// addSubscriberLocked registers a channel for a workspace and bumps its local
// count. Callers must hold mu.
func (b *RedisBus) addSubscriberLocked(workspaceID string) *subscriber {
	sub := newSubscriber(workspaceID)
	byWorkspace, ok := b.subscribers[workspaceID]
	if !ok {
		byWorkspace = make(map[chan Event]*subscriber)
		b.subscribers[workspaceID] = byWorkspace
	}
	byWorkspace[sub.ch] = sub
	b.workspaceOf[sub.ch] = workspaceID
	b.wsCounts[workspaceID]++
	return sub
}

// SubscribeIfAllowed atomically checks the per-workspace limit and
// subscribes. See the EventBus interface for why there is no global limit
// here (BUG-2726).
//
// NOTE: the limit is enforced against local (per-pod) subscriber counts
// only. In multi-replica deployments the effective cap is multiplied by
// the number of replicas — as is every other streaming limit Pad has.
func (b *RedisBus) SubscribeIfAllowed(ctx context.Context, workspaceID string, maxPerWorkspace int) (chan Event, <-chan struct{}, SubscribeOutcome) {
	ch, _, gaps, outcome := b.subscribeAndReplay(ctx, workspaceID, 0, maxPerWorkspace)
	return ch, gaps, outcome
}

// SubscribeAndReplaySince implements EventBus. See the interface for the
// guarantee.
//
// One lock does it here: this bus keeps subscribers and replay buffers under
// the same b.mu, and fanOut holds it across the buffer append AND the fan-out
// to live channels. So an event is either fully applied before this call's
// critical section (in the buffer, not on the new channel) or entirely after
// it (on the channel, replay already read) — never both, never neither.
func (b *RedisBus) SubscribeAndReplaySince(ctx context.Context, workspaceID string, sinceID int64, maxPerWorkspace int) (chan Event, []Event, <-chan struct{}, SubscribeOutcome) {
	return b.subscribeAndReplay(ctx, workspaceID, sinceID, maxPerWorkspace)
}

// subscribeAndReplay is the single body behind all three Subscribe* entry
// points, so none of them can drift in how it establishes a subscription.
//
// IT SPANS TWO CRITICAL SECTIONS, WITH THE REDIS I/O BETWEEN THEM, and both
// halves of that are the point (BUG-2747, BUG-2748).
//
//	section 1 — admission check, register the subscriber, capture the replay
//	            ceiling, decide whether we establish this workspace's
//	            subscription or wait on someone who already is
//	off-lock  — dial, SUBSCRIBE, await Redis's acknowledgement
//	section 2 — run the test seam, read the replay up to the ceiling
//
// THE CEILING IS WHAT PRESERVES THE REPLAY-XOR-CHANNEL GUARANTEE across the
// split, and it is a generalisation of what the single critical section gave
// for free rather than a new idea. The subscriber is live in the fan-out map
// from the moment it is registered, so it receives everything published after
// that instant on its CHANNEL; the replay's job is therefore everything
// published BEFORE it, which is exactly the buffer's contents at registration.
// Bounding the replay at the newest ID the buffer held then divides the two
// spans at the same point the original code did, whether or not the wait
// separates them.
//
// An earlier version instead SKIPPED fan-out to a not-yet-admitted subscriber
// and let the replay read deliver the gap. It was wrong for the population
// this whole fix is about: a fresh subscriber (sinceID == 0) reads no replay,
// so events arriving in the gap were skipped by fan-out and never replayed —
// dropped outright. Found by codex round 1; keeping the note because the
// mechanism was locally coherent and the failure was one call path over.
func (b *RedisBus) subscribeAndReplay(ctx context.Context, workspaceID string, sinceID int64, maxPerWorkspace int) (chan Event, []Event, <-chan struct{}, SubscribeOutcome) {
	if ctx.Err() != nil {
		// Checked before the deferred resume-gap report is armed: a caller
		// that has already gone is not a resume this instance failed to serve.
		//
		// THIS IS THE ONLY EARLY EXIT, deliberately. An earlier draft paired it
		// with a cancellation break at the top of the establish loop, which
		// the mutation matrix could only detect when BOTH were removed — and
		// codex round 2 then found why: the loop-top one could exit while
		// still owning an unretired establishment record, stranding the next
		// subscriber forever. It is gone. Past this point a cancelled caller
		// is unwound by the code that owns the thing being unwound, never by
		// jumping over it.
		return nil, nil, nil, SubscribeCancelled
	}

	// Reported on the way out with the lock released, and only for a caller
	// that was actually resuming. See the MemoryBus twin.
	var missed []Event
	resuming := sinceID > 0
	defer func() {
		if resuming && missed == nil {
			b.reportResumeGap(workspaceID)
		}
	}()

	b.mu.Lock()
	if maxPerWorkspace > 0 && b.wsCounts[workspaceID] >= maxPerWorkspace {
		// A refused connection is an admission event, not a resume this
		// instance could not serve; clearing this keeps it out of the gap
		// counter's population.
		resuming = false
		b.mu.Unlock()
		return nil, nil, nil, SubscribeWorkspaceLimit
	}

	sub := b.addSubscriberLocked(workspaceID)

	// Where the buffer stood at REGISTRATION, captured here rather than read in
	// section 2.
	//
	// The BUFFER ITSELF is part of the mark, not just its position (codex round
	// 3). An ID-space reset during the wait replaces the buffer wholesale, and
	// a position in the old one describes nothing in the new one — while
	// knownFrom may still accept an adjacent cursor, so the mismatch is not
	// self-announcing. A nil buffer here is the same statement in its strongest
	// form: this instance was not covering the workspace at all.
	mark := registrationMark{buffer: b.replayBuffers[workspaceID]}
	if mark.buffer != nil {
		mark.appends = mark.buffer.appends
	}

	var establish bool
	var pending *pendingSub
	// PENDING IS CHECKED BEFORE WSSUBS, not after (codex round 1, P1). The two
	// overlap on purpose — establishSubscription installs wsSubs and only then
	// waits for the acknowledgement — so a subscriber arriving in that overlap
	// finds a live-looking subscription that Redis has not confirmed. Reading
	// wsSubs first admitted it immediately, into precisely the unconfirmed
	// window this function exists to close.
	if p, inFlight := b.pendingSubs[workspaceID]; inFlight {
		pending = p
	} else if _, live := b.wsSubs[workspaceID]; !live {
		pending = &pendingSub{done: make(chan struct{})}
		b.pendingSubs[workspaceID] = pending
		establish = true
	}
	b.mu.Unlock()

	if b.afterRegisterBeforeEstablish != nil {
		b.afterRegisterBeforeEstablish(workspaceID)
	}

	// A JOINER VERIFIES RATHER THAN ASSUMES, and may take the establishment
	// over once (codex round 2, P1). Waiting on someone else's record proves
	// only that they FINISHED — not that they succeeded.
	//
	// THE LOAD-BEARING FIX IS ELSEWHERE, and this is defence in depth: the
	// abandon path retires its record in the SAME critical section as the
	// decision, which makes the strand unreachable rather than recoverable. A
	// joiner increments wsCounts under b.mu before the establisher's count
	// check reads it, so a registered joiner prevents the abandon; and a joiner
	// arriving after the check cannot find the record, because it is gone in
	// that same section. Both halves are under one lock, so there is no
	// interleave left.
	//
	// It is kept anyway because that is an ARGUMENT, not a measurement, and the
	// failure it guards against is a permanently dead stream that looks alive.
	// A retry costs one extra pass in a case that should never happen; being
	// wrong costs a user their live updates with no signal. Measured
	// accordingly: no mutation of the surrounding code makes a test reach this
	// path, which is consistent with unreachable and is not proof of it — hence
	// the log, so that if the argument is wrong, production says so instead of
	// silently limping.
	//
	// Bounded at two passes: the second is a caller that has now registered, so
	// the emptied-workspace race it lost cannot recur, and any remaining
	// failure is one a third pass would not fix either.
	for attempt := range 2 {
		// NO CANCELLATION CHECK AT THE TOP OF THIS LOOP, and its absence is
		// load-bearing (BUG-2749, codex round 2 P1).
		//
		// An earlier version broke out of the loop here when the caller's
		// context had ended. On attempt 0 that is a STRAND: section 1 may
		// already have created this workspace's establishment record and named
		// us the establisher, and breaking here leaves that record in
		// pendingSubs with nobody behind it — never retired, its done channel
		// never closed. The next subscriber for the workspace joins it and
		// waits forever, and because its own registration keeps wsCounts
		// non-zero, no later caller establishes either. That is exactly the
		// permanent silently-dead stream the establishment record exists to
		// prevent, reintroduced by a guard meant to save a dial.
		//
		// A cancelled establisher therefore goes THROUGH establishSubscription,
		// which is the only code that knows how to put the record down: it
		// deregisters the departed establisher and abandons-and-retires in one
		// critical section, or installs for whatever joiners arrived. The dial
		// it pays for is on the caller's context and aborts at once.
		//
		// Retries are guarded instead where the decision is actually made —
		// see the ctx.Err() term in the re-decide below, which stops a
		// departed caller MINTING a fresh record rather than abandoning one it
		// already owns. That term is an OPTIMISATION, not a correctness
		// guard, and the mutation matrix is what says so: removing it survives
		// every test here, because a departed caller that does mint a second
		// record still establishes, still deregisters itself in the deciding
		// section, and still abandons and retires. It saves a pointless dial
		// on a path that should already be rare, and nothing more.
		if attempt > 0 {
			// Re-decide, and note that the record is created HERE, at the top
			// of an iteration that is going to use it — never as a trailing
			// side-effect of the last check. A record left in the map with
			// nobody establishing behind it is worse than the bug this loop
			// fixes: the next caller joins it and waits forever.
			b.mu.Lock()
			_, live := b.wsSubs[workspaceID]
			establish, pending = false, nil
			if !live && b.ctx.Err() == nil && ctx.Err() == nil {
				if p, inFlight := b.pendingSubs[workspaceID]; inFlight {
					pending = p
				} else {
					pending = &pendingSub{done: make(chan struct{})}
					b.pendingSubs[workspaceID] = pending
					establish = true
				}
			}
			b.mu.Unlock()
			if !establish && pending == nil {
				break
			}
			slog.Warn("events: finished waiting on a subscription establishment that left none behind; re-establishing",
				"workspace", workspaceID)
		}

		switch {
		case establish:
			b.establishSubscription(ctx, workspaceID, sub, pending)
		case pending != nil:
			// Someone else is establishing the same workspace. Wait for them
			// rather than opening a second connection and a second receive
			// loop.
			select {
			case <-pending.done:
			case <-b.ctx.Done():
			case <-ctx.Done():
			}
		default:
			// Already live when we registered: nothing to establish and
			// nothing to wait for.
		}
	}

	// CANCELLATION IS DEREGISTRATION, and everything else falls out of that
	// (BUG-2749). wsCounts already answers "is anyone still here"; a caller
	// whose context ended is someone who is not, so the fix is to stop being
	// counted rather than to add machinery that hands establishment over.
	//
	// Unsubscribe is safe to call even when establishSubscription already did
	// it under its own deciding lock — it returns early on a channel it no
	// longer holds — so neither path has to know what the other did. And when
	// this IS the removal that takes the workspace to zero, its existing
	// count-to-zero branch stops the Redis subscription, so an establishment
	// completed for a caller who left does not outlive them.
	if ctx.Err() != nil {
		b.Unsubscribe(sub.ch)
		resuming = false
		return nil, nil, nil, SubscribeCancelled
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.afterSubscribeRegister != nil {
		b.afterSubscribeRegister()
	}
	if resuming {
		missed = b.eventsSinceMarkLocked(workspaceID, sinceID, mark)
	}
	return sub.ch, missed, sub.gaps, SubscribeOK
}

// registrationMark records where a workspace's replay buffer stood when a
// caller registered: which buffer, and how many appends it had taken.
type registrationMark struct {
	buffer  *replayBuffer
	appends int64
}

// eventsSinceMarkLocked is eventsSinceLocked bounded ABOVE by where the buffer
// stood when the caller registered, so its replay stops exactly where its own
// channel starts. Callers must hold b.mu.
//
// BOUNDED BY POSITION, NOT BY ID. This bus's ids come from a counter shared
// across workspaces, and a phase-1 publish assigns and publishes in two calls,
// so arrival order and numeric order genuinely disagree. Cutting on id value
// would both replay a straggler the caller already received and silently drop a
// pre-registration event that happens to carry a higher id.
//
// THE BOUND IS APPLIED BEFORE THE CURSOR FILTER, not after (codex round 5). An
// earlier version asked since() for the events above the cursor and then
// dropped the last (appends - mark) of them — two different spaces. A
// post-registration straggler whose id falls at or below the cursor is absent
// from that filtered slice but still counted in the drop, so the count ate a
// legitimate pre-registration event instead. Pre-mark [5 30 20], post-mark
// [6 40], cursor 10: the caller was handed [30] and 20 vanished.
func (b *RedisBus) eventsSinceMarkLocked(workspaceID string, sinceID int64, mark registrationMark) []Event {
	rb, ok := b.replayBuffers[workspaceID]
	if !ok || mark.buffer == nil || rb != mark.buffer {
		// No coverage at registration, or a different buffer now: either way
		// this instance cannot vouch for the caller's span.
		return nil
	}
	// The buffer holds its last `count` appends, so the entries that were
	// already there when the caller registered are the oldest
	// mark.appends - (appends - count) of them. Negative means the whole of
	// what it held then has since been evicted, which sinceBounded refuses.
	keep := mark.appends - (rb.appends - int64(rb.count))
	return rb.sinceBounded(sinceID, int(keep))
}

// Unsubscribe removes a local subscriber and closes its channel.
// Cancels the Redis subscription if this was the last local subscriber for the workspace.
func (b *RedisBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsubscribeLocked(ch)
}

// unsubscribeLocked is Unsubscribe's body, split out so a caller that is
// already inside a critical section can deregister a subscriber without
// releasing the lock (BUG-2749: establishSubscription must apply a departed
// establisher's removal in the SAME section that reads wsCounts to decide
// whether to abandon).
//
// IT IS IDEMPOTENT BY THE workspaceOf LOOKUP, which is what lets the
// cancellation path call Unsubscribe unconditionally afterwards without
// either side tracking what the other did — a second call finds no entry and
// returns before it can double-close the channel.
//
// Callers must hold b.mu.
func (b *RedisBus) unsubscribeLocked(ch chan Event) {
	wsID, ok := b.workspaceOf[ch]
	if !ok {
		return
	}
	delete(b.workspaceOf, ch)
	if byWorkspace, ok := b.subscribers[wsID]; ok {
		delete(byWorkspace, ch)
		if len(byWorkspace) == 0 {
			delete(b.subscribers, wsID)
		}
	}
	close(ch)

	b.wsCounts[wsID]--
	if b.wsCounts[wsID] <= 0 {
		delete(b.wsCounts, wsID)
		b.stopRedisSubscription(wsID)
	}
}

// Publish sends an event to Redis, which distributes it to all instances.
// Events are assigned a globally unique sequence ID via Redis INCR so that
// Last-Event-ID is valid across any instance on reconnect.
func (b *RedisBus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	channel := b.keys.Name(redisChannelSuffix) + event.WorkspaceID
	if b.publishEpoch {
		b.publishWithEpoch(channel, event)
		return
	}

	// PHASE 1: the historical two-call shape — assign, then publish — so the
	// bare wire form still carries the ID INSIDE the JSON, which is what an
	// older instance knows how to read. That is why the atomic script and the
	// prefix arrive together in phase 2 and not here: the JSON cannot be
	// marshalled until the ID is known.
	//
	// It therefore keeps the pre-existing interleave window (two round-trips,
	// so two instances can publish out of ID order) and the pre-existing
	// retry-duplication window. Phase 2 closes both.
	//
	// THE ASSIGNMENT ITSELF IS A SCRIPT rather than a bare INCR, and only so
	// that a restart can clear a stale epoch ATOMICALLY with the INCR that
	// detects it (codex round 3). As two commands there is a window in which a
	// concurrent flipped publisher mints an epoch between our INCR and our
	// DEL, and we delete a LIVE one.
	//
	// The shape the clear closes (codex round 2): phase 2 mints an epoch and
	// the sequence reaches 500; the deployment rolls back to phase 1; the seq
	// key is then evicted or deleted; phase-1 publishers climb from 1 again;
	// phase 2 is re-enabled and finds the old epoch still there. A receiver
	// that had adopted it sees no change, and if its high-water mark is below
	// the new sequence — a replica that just started, or one whose buffers
	// were empty — the numeric check does not see the reset either. Two ID
	// spaces merge in one buffer silently, which is the outcome this whole
	// unit exists to prevent. Phase 2's own rotation cannot cover it: that
	// rotation fires when the SCRIPT's INCR returns 1, and by then the counter
	// has climbed past 1 under this path.
	//
	// Deleting rather than rotating: this path publishes no epoch and has none
	// to propose, and an absent key is exactly what phase 2 mints into.
	id, err := assignScript.Run(b.ctx, b.client,
		[]string{b.keys.Name(redisSeqSuffix), b.keys.Name(redisEpochSuffix)}).Int64()
	if err != nil {
		// NO LOCAL-COUNTER FALLBACK, and its removal is a fix rather than a
		// regression (BUG-2731). The previous version answered a failed INCR
		// by minting an id from a process-local counter starting at zero and
		// publishing it anyway — an id from a different space, which every
		// receiving instance reads as the counter having been reset. It also
		// bought nothing: this bus has no local fan-out path, so an event
		// that does not reach Redis reaches no subscriber on this instance
		// either, fallback id or not. Failing loudly is the honest outcome.
		slog.Error("failed to assign an event ID from Redis; dropping the publish", "error", err)
		return
	}
	event.ID = id

	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal event for Redis", "error", err)
		return
	}

	if err := b.client.Publish(b.ctx, channel, data).Err(); err != nil {
		slog.Error("failed to publish event to Redis; the event may or may not have reached subscribers, and is not retried here",
			"channel", channel, "phase", 1, "error", err)
	}
}

// publishWithEpoch is the PHASE 2 path: one atomic script assigns the ID,
// maintains the epoch, and publishes "<epoch>|<id>|<json>".
//
// The event is marshalled with ID still zero, because the ID travels in the
// prefix and decodePayload writes it back onto the decoded Event. A receiver
// running phase 1 or phase 2 reads the same value either way; a receiver
// running a PRE-phase-1 binary cannot parse this at all, which is the reason
// the flip is a second roll rather than a config change (see
// config.EventsPublishEpoch).
func (b *RedisBus) publishWithEpoch(channel string, event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal event for Redis", "error", err)
		return
	}

	// A fresh token per logical publish — NOT per attempt, which is the point:
	// go-redis reuses the same arguments on its own retries, so the second run
	// of the script sees the same token and declines.
	dedupeKey := b.keys.Name(redisDedupeSuffix) + uuid.NewString()
	if err := publishScript.Run(b.ctx, b.client,
		[]string{
			b.keys.Name(redisSeqSuffix), channel, b.keys.Name(redisEpochSuffix),
			dedupeKey, b.keys.Name(redisEpochGenSuffix),
		},
		string(data), redisDedupeTTLSeconds, b.generationRestartSeed()).Err(); err != nil {
		// NO LOCAL-COUNTER FALLBACK, for the same reason the phase-1 path has
		// none (BUG-2731): an ID minted locally belongs to a different space,
		// which every receiving instance reads as a counter reset, and this
		// bus has no local fan-out path so the event reaches nobody here
		// either way.
		//
		// Note what this error does and does not mean: the script is atomic,
		// so it never half-executes — but go-redis retries a command whose
		// REPLY was lost, so an error here can accompany a publish that
		// actually happened. That is what the dedupe token is for, and why
		// this logs rather than re-publishing.
		slog.Error("failed to publish event to Redis; the script is atomic so it did not half-execute, but a lost REPLY means it may have published anyway — do not re-publish by hand",
			"channel", channel, "phase", 2, "error", err)
	}
}

// EventsSince returns buffered events for a workspace with IDs greater than
// sinceID. Returns nil when this instance cannot vouch for the requested span
// — see the EventBus interface and replayBuffer.since.
func (b *RedisBus) EventsSince(workspaceID string, sinceID int64) []Event {
	// Reported once, on the way out, and with the lock released — see the
	// MemoryBus twin for why both no-buffer and refused-span must reach the
	// same counter.
	var missed []Event
	defer func() {
		if missed == nil {
			b.reportResumeGap(workspaceID)
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()

	missed = b.eventsSinceLocked(workspaceID, sinceID)
	return missed
}

// eventsSinceLocked is EventsSince without the observer report. Callers must
// hold b.mu. Shared with SubscribeAndReplaySince so the two cannot drift in
// what "cannot vouch" means; the report stays with the caller because a
// SubscribeAndReplaySince may not be a resume at all.
func (b *RedisBus) eventsSinceLocked(workspaceID string, sinceID int64) []Event {
	rb, ok := b.replayBuffers[workspaceID]
	if !ok {
		// No buffer means this instance is not currently covering the
		// workspace, or has only just started to. For a fresh client
		// (sinceID == 0) that is honestly "nothing to replay"; for a
		// resuming one it is the strongest form of "cannot vouch" there
		// is, and answering []Event{} told it that it was caught up
		// (BUG-2731).
		//
		// This is the NORMAL state for the first connection to a given
		// workspace on a given replica, because buffers are built lazily
		// in fanOutLocally — so a restart, a scale-up, or a namespace
		// cutover all land here, not just an exotic failure.
		if sinceID > 0 {
			return nil
		}
		return []Event{}
	}
	return rb.since(sinceID)
}

// Close shuts down all Redis subscriptions and closes local subscriber channels.
//
// IT DOES NOT JOIN THE MAINTENANCE GOROUTINES (BUG-2738, codex round 3), and
// that is a choice rather than an omission. Their publish half makes
// synchronous Redis calls bounded by go-redis's own Dial/Read/WriteTimeout —
// exactly the calls that stall on the wedged route this whole feature exists
// to detect — so joining them would let a dead network hold shutdown open for
// as long as those timeouts take. maintenanceStopped is available for a caller
// that genuinely wants to wait; nothing in production does.
//
// What holds instead is that a cycle already past its own ctx check cannot
// leave anything behind: establishSubscription re-checks b.ctx under its
// deciding lock and abandons there, closing the PubSub and retiring the record
// in the same critical section, and the dial dies with b.ctx through
// mergeCancellation (except under TLS, where DialTimeout bounds it — see that
// function). Pinned by TestClosingTheBusDuringACycleInstallsNothing.
func (b *RedisBus) Close() {
	b.cancel() // signal all subscription goroutines to stop

	b.mu.Lock()
	defer b.mu.Unlock()

	for wsID, sub := range b.wsSubs {
		sub.cancel()
		sub.pubsub.Close()
		delete(b.wsSubs, wsID)
	}

	for wsID, byWorkspace := range b.subscribers {
		for ch := range byWorkspace {
			delete(b.workspaceOf, ch)
			close(ch)
		}
		delete(b.subscribers, wsID)
	}
	// CLEARED ALONGSIDE THE SUBSCRIBERS IT COUNTS (codex round 2). Left
	// populated, it is a count of channels this loop has just closed, and an
	// establishment still in flight reads it as a reason to install a
	// subscription for them. The ctx check there is the primary guard; this
	// keeps the two structures from disagreeing in the first place.
	clear(b.wsCounts)
}

// SubscriberCount returns the number of active local subscribers.
func (b *RedisBus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.workspaceOf)
}

// WorkspaceSubscriberCount returns the number of active local subscribers for a workspace.
func (b *RedisBus) WorkspaceSubscriberCount(workspaceID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wsCounts[workspaceID]
}

// establishSubscription opens a workspace's Redis subscription and does not
// return until Redis has acknowledged it, the bound expires, or the CALLER's
// context ends — in which case the remainder of the wait is handed to a
// goroutine and this returns at once (BUG-2749).
//
// MUST BE CALLED WITHOUT b.mu HELD. That is the whole of BUG-2748: this
// function's first statement dials a fresh TCP connection, runs go-redis's
// initConn handshake on it and writes the SUBSCRIBE, and every byte of that
// used to happen inside the bus's single global mutex — per workspace, since
// each client.Subscribe mints a PubSub whose connection starts nil. A Redis
// whose route blackholes packets stalled that dial for the client's
// DialTimeout (5s by default; since BUG-2749 the CALLER's context can cut that
// short on a plaintext connection, but not under TLS — see
// defaultSubscribeConfirmTimeout), and for that whole time no other workspace
// could subscribe, unsubscribe, close, or receive a fanned-out event.
//
// Exactly one caller per workspace reaches here; the rest wait on pending.
func (b *RedisBus) establishSubscription(ctx context.Context, workspaceID string, establisher *subscriber, pending *pendingSub) {
	channel := b.keys.Name(redisChannelSuffix) + workspaceID
	// DIALLED ON THE CALLER'S CONTEXT *AND* THE BUS'S, so a client that leaves
	// mid-dial stops paying for it (BUG-2749) without taking away Close()'s
	// ability to interrupt the same dial (codex round 2 P2).
	//
	// Passing only the caller's context regressed shutdown: b.ctx used to be
	// what cut a stalled dial short when the bus closed, and a caller who
	// stays connected through a shutdown would have left the dial running to
	// DialTimeout with nothing able to stop it. Either ending is a reason to
	// abandon, so the dial waits on both.
	dialCtx, cancelDial := mergeCancellation(ctx, b.ctx)
	defer cancelDial()
	//
	// WHAT THIS DOES AND DOES NOT BOUND, checked in go-redis v9.22.0 rather
	// than inferred from its doc comment — which says Subscribe "does not wait
	// on a response from Redis" and so reads as though no dial happens here at
	// all. It does: Client.Subscribe -> PubSub.Subscribe -> subscribe ->
	// conn(ctx) dials when there is no connection yet, then writes the
	// SUBSCRIBE command. Only the REPLY is unawaited.
	//
	//   - Plaintext: dialConn derives its attempt context from the one passed
	//     in (internal/pool/pool.go, "Apply DialTimeout per attempt, but never
	//     extend an existing earlier deadline") and the default dialer is
	//     net.Dialer.DialContext, which honours it. Cancellation aborts the
	//     dial.
	//   - TLS: the same dialer returns tls.DialWithDialer(netDialer, ...)
	//     (options.go), which takes NO context. Under TLS the dial is bounded
	//     by DialTimeout alone and cancellation cannot shorten it.
	//
	// So on a TLS deployment this shrinks the held slot from (dial + confirm
	// bound) to (dial), not to zero. That residual is real and is why the
	// cancellation check below is repeated after the dial rather than assumed
	// to have fired during it.
	pubsub := b.client.Subscribe(dialCtx, channel)
	subCtx, subCancel := context.WithCancel(b.ctx)

	if b.beforeInstallSubscription != nil {
		b.beforeInstallSubscription(workspaceID)
	}

	b.mu.Lock()
	// TWO REASONS TO ABANDON, and both must retire the establishment record in
	// THIS critical section (codex round 2, both P1s).
	//
	// Nobody left: everyone who wanted this workspace disconnected while we
	// were dialling. Installing now would leave a receive loop and a Redis
	// connection alive with nobody to deliver to, and nothing would ever tear
	// them down — Unsubscribe only stops a subscription when it takes the count
	// from one to zero, and the count is already zero.
	//
	// Bus closed: Close() cancels the context BEFORE it takes the lock and
	// drains wsSubs, so an establishment that locks afterwards would install
	// into a map Close has already emptied. The receive loop would exit on the
	// cancelled context and neither subCancel nor pubsub.Close would ever run,
	// leaking the PubSub and its health-check goroutine.
	//
	// RETIRING THE RECORD UNDER THIS SAME LOCK is what stops a joiner being
	// stranded. Released separately, there is an interval in which we have
	// decided to abandon and the record still says an establishment is coming:
	// a subscriber arriving there registers, waits on a promise nobody will
	// keep, and returns with a channel wired to nothing — permanently, because
	// its own registration makes wsCounts non-zero so no later caller
	// establishes either.
	// THE ESTABLISHER'S OWN DEPARTURE IS APPLIED BEFORE THE COUNT IS READ, in
	// this same section (BUG-2749). The count below is the arbiter for "is
	// anyone still here", and while we were dialling the answer may have
	// become no — but only if the caller that opened this establishment stops
	// being counted first. Removing it after the read would install a
	// subscription and a receive loop for nobody; removing it in a separate
	// section would let a joiner arrive in between and be counted against a
	// decision already taken.
	//
	// Note what this deliberately does NOT do: it does not abandon because the
	// establisher left. If joiners registered while we dialled, wsCounts is
	// still non-zero and the subscription is installed for them, which is the
	// hand-off the filing asked about — expressed as a count rather than as a
	// transfer of ownership.
	// A NIL ESTABLISHER IS THE BUS ESTABLISHING FOR ITSELF (BUG-2738, rule 4).
	// The idle detector re-establishes on b.ctx with no subscriber
	// registration of its own, so there is nothing to deregister — and b.ctx
	// is never cancelled until Close, at which point the count/closed check
	// below is what abandons. Guarding the nil here rather than handing the
	// detector a synthetic subscriber keeps wsCounts meaning "clients", which
	// is what every arbitration in this file reads it as.
	if establisher != nil && ctx.Err() != nil {
		b.unsubscribeLocked(establisher.ch)
	}
	if b.wsCounts[workspaceID] == 0 || b.ctx.Err() != nil {
		b.retirePendingLocked(workspaceID, pending)
		b.mu.Unlock()
		subCancel()
		_ = pubsub.Close()
		close(pending.done)
		return
	}
	b.subGen++
	gen := b.subGen
	sub := &redisSub{
		pubsub: pubsub,
		cancel: subCancel,
		gen:    gen,
		// STAMPED AT INSTALL, not left at the zero value (BUG-2738, rule 2 of
		// cycleIdleSubscriptions). A zero time reads as 1970, so a subscription
		// that has simply not received anything yet would be older than any
		// threshold and the idle detector would cycle it on its next tick —
		// hardest in exactly the case BUG-2747 exists for, an unconfirmed
		// admission where no acknowledgement ever arrives to stamp it. The
		// clock starts when the socket does.
		lastSeen:  b.now(),
		confirmed: make(chan struct{}),
	}
	b.wsSubs[workspaceID] = sub
	b.mu.Unlock()

	// INSTALLED BEFORE THE LOOP STARTS, not after. fanOut drops any message
	// for a workspace with no installed subscription of a matching gen, so
	// starting the receive loop first would reopen a smaller version of the
	// very window this function exists to close.
	go b.receiveMessages(subCtx, pubsub, workspaceID, gen)

	if b.afterInstallSubscription != nil {
		b.afterInstallSubscription(workspaceID)
	}

	timer := time.NewTimer(b.confirmTimeout)
	select {
	case <-sub.confirmed:
	case <-b.ctx.Done():
	case <-ctx.Done():
		// A CANCELLED ESTABLISHER OWES ITS JOINERS THE REST OF THE WAIT, and
		// this is the one thing it genuinely does owe them (BUG-2749).
		//
		// Not the connection: that is installed above with its receive loop
		// running, and wsCounts already decides its fate. What cannot simply
		// be dropped is the WAIT, because finishPending is what releases the
		// joiners and the acknowledgement is what makes their stream
		// honest. Returning here and closing pending.done inline would admit
		// every joiner into a subscription Redis has not acknowledged, telling
		// them nothing — which is precisely the defect BUG-2747 exists to
		// close, re-created for the joiner population at the seam between the
		// two designs.
		//
		// So the REMAINDER of the wait moves to a goroutine and finishes
		// exactly as this caller would have: same three arms, same
		// markUnconfirmedAdmission on the bound, same finishPending. Only the
		// caller's context is dropped, because the caller is who left.
		//
		// It is bounded by confirmTimeout and needs no reaper: if the departing
		// establisher was the last subscriber, its Unsubscribe stops the
		// subscription, and this waiter then falls through to the bound and
		// finds no wsSubs entry — markUnconfirmedAdmission's gen/liveness guard
		// makes that a no-op rather than a spurious count.
		go func() {
			defer timer.Stop()
			select {
			case <-sub.confirmed:
			case <-b.ctx.Done():
			case <-timer.C:
				b.markUnconfirmedAdmission(workspaceID, gen)
			}
			if b.afterSubscriptionConfirmed != nil {
				b.afterSubscriptionConfirmed()
			}
			b.finishPending(workspaceID, pending)
		}()
		return
	case <-timer.C:
		b.markUnconfirmedAdmission(workspaceID, gen)
	}
	timer.Stop()

	if b.afterSubscriptionConfirmed != nil {
		b.afterSubscriptionConfirmed()
	}

	b.finishPending(workspaceID, pending)
}

// mergeCancellation returns a context that ends when EITHER input does.
//
// It exists because the dial has two legitimate reasons to be abandoned that
// live in different places: the caller going away (BUG-2749) and the bus
// closing (BUG-2748's teardown). Deriving from one and ignoring the other
// silently drops half the shutdown story, which is how the second one was lost
// in this unit's first draft.
//
// The returned cancel must be called to release the AfterFunc registration on
// the second context; not calling it keeps that registration alive for as long
// as the second context does.
func mergeCancellation(primary, also context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	stop := context.AfterFunc(also, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// finishPending retires a workspace's establishment record and releases every
// caller waiting on it.
func (b *RedisBus) finishPending(workspaceID string, pending *pendingSub) {
	b.mu.Lock()
	b.retirePendingLocked(workspaceID, pending)
	b.mu.Unlock()
	close(pending.done)
}

// retirePendingLocked removes an establishment record if it is still the
// current one. Callers must hold b.mu.
//
// The identity check matters: a record we abandoned may already have been
// replaced by a later caller's, and deleting that one would let a third caller
// start a second subscription for the same workspace.
func (b *RedisBus) retirePendingLocked(workspaceID string, pending *pendingSub) {
	if b.pendingSubs[workspaceID] == pending {
		delete(b.pendingSubs, workspaceID)
	}
}

// markUnconfirmedAdmission records that Redis did not acknowledge a
// subscription within the bound, so its callers are being admitted without
// this instance being able to say what the stream covers.
//
// ADMIT RATHER THAN REFUSE, deliberately. Before BUG-2747 every subscriber was
// admitted into an unconfirmed subscription and told nothing, so admitting on
// the failure path is not a regression — it is today's behaviour, now counted,
// logged, and reconciled to the client when the acknowledgement lands. Refusing
// instead would turn a Redis blip into failed SSE connections, which amplifies
// an outage for no gain in honesty: the honesty is owed to the CLIENT, and
// confirmSubscription is what pays it.
func (b *RedisBus) markUnconfirmedAdmission(workspaceID string, gen int64) {
	if b.beforeUnconfirmedMark != nil {
		b.beforeUnconfirmedMark()
	}
	b.mu.Lock()
	sub, ok := b.wsSubs[workspaceID]
	// confirmClosed is checked under the same lock that closes it (codex round
	// 1, P2). The timer and the acknowledgement can become ready together; if
	// the timer won the select after confirmSubscription had already cleared
	// the flag, this would set it again with nothing left to come and clear
	// it — a subscriber counted as unconfirmed, never told to reconcile, and
	// a workspace whose flag stays set until its NEXT resubscription.
	if ok && sub.gen == gen && !sub.confirmClosed {
		sub.unconfirmedAdmitted = true
	} else {
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return
	}

	slog.Warn("events: Redis did not acknowledge the subscription in time; admitting subscribers without a coverage claim, they will be told to reconcile when it lands",
		"workspace", workspaceID, "timeout", b.confirmTimeout)
	b.reportSubscriptionUnconfirmed()
}

// confirmSubscription runs when Redis acknowledges a subscription, releasing
// the callers waiting on it.
//
// If they were already admitted without it — the bound expired first — then
// the span between is a hole those subscribers sat through, and they are told
// so through the same channel BUG-2730 built for a mid-stream gap. The
// acknowledgement is what makes the span's END knowable; before it arrives
// there is nothing to report the size of.
func (b *RedisBus) confirmSubscription(workspaceID string, gen int64) {
	b.mu.Lock()
	sub, ok := b.wsSubs[workspaceID]
	if !ok || sub.gen != gen {
		b.mu.Unlock()
		return
	}
	late := sub.unconfirmedAdmitted
	sub.unconfirmedAdmitted = false
	if !sub.confirmClosed {
		sub.confirmClosed = true
		close(sub.confirmed)
	}
	b.mu.Unlock()

	if late {
		// Takes b.mu itself, hence outside the section above.
		b.dropWorkspaceCoverage(workspaceID, ResetReasonSubscriptionUnconfirmed, gen)
	}
}

// stopRedisSubscription cancels and cleans up the Redis subscription for a workspace.
// Must be called with b.mu held.
func (b *RedisBus) stopRedisSubscription(workspaceID string) {
	sub, ok := b.wsSubs[workspaceID]
	if !ok {
		return
	}
	sub.cancel()
	sub.pubsub.Close()
	delete(b.wsSubs, workspaceID)

	// WHEN WE STOP RECEIVING, THE HONEST STATE IS NO BUFFER, NOT A STALE
	// CONTIGUOUS ONE (BUG-2731). This is the invariant a future optimization
	// will be tempted to violate — keeping the buffer "in case they come
	// back" looks like a free win and is the bug.
	//
	// From here until the workspace is subscribed again, events published on
	// other instances never enter this buffer, while the buffer itself goes
	// on LOOKING complete: same IDs, no eviction, nothing to distinguish it
	// from a live one. A later client resuming with a cursor at or below the
	// stale newest ID would be replayed the stale tail and told nothing about
	// the hole. Dropping it makes the next resume answer nil, which is true.
	delete(b.replayBuffers, workspaceID)
}

// receiveMessages reads from a Redis pub/sub channel and fans out to local subscribers.
// receiveMessages consumes one workspace's Redis channel until the context is
// cancelled.
//
// IT USES ChannelWithSubscriptions, AND BOTH HALVES OF THAT CHOICE MATTER.
//
// Against Channel: a plain message channel hides RESUBSCRIPTIONS. go-redis
// reconnects transparently on a dropped connection — a Redis failover, a
// network blip, a server restart — and hands back the messages that arrive
// AFTER it recovers, saying nothing about the ones published while it was
// down. The replay buffer then carries a hole it has no idea about, and a
// resume across it is answered "caught up". A resubscription is the one form
// of mid-stream loss this process can actually OBSERVE (contrast BUG-2735,
// which stays open precisely because a message lost in transit leaves no local
// trace on a bus whose per-workspace IDs are non-consecutive by construction),
// so every subscription confirmation after the first ends that workspace's
// coverage, exactly like a stopped subscription.
//
// Against a bare Receive loop, which is what this was first written as: only
// the Channel* constructors start go-redis's health check (newChannel →
// initHealthCheck), and go-redis owns the redial and backoff, so a hand-rolled
// retry loop here is machinery this package does not need to maintain.
//
// WHAT NEITHER FORM DETECTS: a HALF-OPEN connection — no FIN, no RST, just a
// route that stopped working. Check it in the library rather than believing
// this comment: PubSub.Ping calls writeCmd and returns, never reading a reply
// (go-redis v9.22.0, pubsub.go), so the health check's error is nil for as
// long as the socket accepts writes — which a half-open socket does until its
// send buffer fills. The channel path sets no read deadline either
// (Receive → ReceiveTimeout(ctx, 0)).
//
// So an instance behind a wedged route sits there receiving nothing while its
// buffer keeps looking valid. THAT IS NOW COVERED, but NOT by anything in this
// function's choice of channel constructor: BUG-2738 added application-level
// idle tracking on top. Every inbound frame stamps sub.lastSeen below, and
// cycleIdleSubscriptions ends coverage and replaces the connection when the
// stamp goes stale. Do not assume the health check covers it; it still does
// not, and a future change that drops the stamping silently un-fixes BUG-2738
// while leaving this loop looking untouched.
func (b *RedisBus) receiveMessages(ctx context.Context, pubsub *redis.PubSub, workspaceID string, gen int64) {
	defer b.reportReceiveLoopExited()

	ch := pubsub.ChannelWithSubscriptions()
	var subscribed bool
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-ch:
			if !ok {
				return
			}
			// STAMPED FOR EVERY FRAME, ahead of the type switch and ahead of
			// any decode (BUG-2738). What idle detection measures is whether
			// the SOCKET carries traffic, so a frame that turns out to be
			// undecodable, or to name another workspace, or to be a
			// resubscription notice, is still proof the route works — and each
			// of those paths `continue`s, so stamping inside the switch would
			// miss them. A message we could not read means coverage is broken,
			// which dropWorkspaceCoverage handles; it does NOT mean the
			// connection is dead, and cycling it would be the wrong remedy.
			b.stampLastSeen(workspaceID, gen)

			switch msg := raw.(type) {
			case *redis.Subscription:
				if msg.Kind != "subscribe" && msg.Kind != "psubscribe" {
					continue
				}
				if !subscribed {
					// THE INITIAL ACKNOWLEDGEMENT. Whoever is inside
					// establishSubscription is waiting on this, and nothing is
					// admitted until it lands (BUG-2747). Signalled here rather
					// than by a Receive placed before this loop, because such a
					// Receive would eat this message and leave the flag below
					// to swallow the first genuine RESUBSCRIPTION instead —
					// see redisSub.confirmed.
					subscribed = true
					b.confirmSubscription(workspaceID, gen)
					continue
				}
				// A RESUBSCRIPTION: the connection dropped and came back, and
				// whatever was published in between never reached us.
				slog.Warn("events: pub/sub resubscribed; dropping this workspace's replay buffer, resumes across the gap will report sync_required",
					"workspace", workspaceID, "channel", msg.Channel)
				b.dropWorkspaceCoverage(workspaceID, ResetReasonSubscriptionResumed, gen)

			case *redis.Message:
				kind, epoch, event, err := decodePayload(msg.Payload)
				if kind == payloadHeartbeat {
					// PHASE 1 IS EXACTLY THIS: recognise and ignore. The frame
					// has already done its whole job by arriving — the stamp
					// above is the entire effect. It consumes no id, drops no
					// buffer, reaches no subscriber and moves no counter, so an
					// instance that publishes none is still a correct receiver
					// for one that does. That is what makes the two-phase roll
					// zero-loss.
					continue
				}
				if err != nil {
					// A MESSAGE WE CANNOT READ IS A HOLE IN THIS WORKSPACE'S
					// COVERAGE (codex round 11). Dropping it and carrying on
					// left the buffer claiming a span it no longer had: the
					// event is gone, the ids either side of it look
					// contiguous, and a later resume across it is answered
					// "caught up". Silent loss, from a payload we know we
					// failed to read.
					//
					// The workspace comes from the CHANNEL rather than from
					// the body, which is what makes this possible at all when
					// the body is the thing that would not parse.
					slog.Error("failed to decode Redis event; ending this workspace's replay coverage, resumes across it will report sync_required",
						"channel", msg.Channel, "error", err)
					b.dropWorkspaceCoverage(workspaceID, ResetReasonUndecodableMessage, gen)
					continue
				}
				if event.WorkspaceID != workspaceID {
					// THE CHANNEL IS THE AUTHORITY ON WHOSE EVENT THIS IS, not
					// the body (codex round 15). Two things arrive here that
					// decode without error and are not a usable event:
					//
					//   - a payload that is valid JSON and empty — "null" or
					//     "{}" both unmarshal into a zero Event, whose
					//     workspace is "". Fan-out then finds no subscription
					//     for "" and returned early WITHOUT ending coverage,
					//     so the buffer went on looking continuous across an
					//     event it had skipped.
					//
					//   - a body naming a DIFFERENT workspace from the channel
					//     it arrived on. Fan-out indexes by the body's
					//     workspace, so such a message would have been
					//     appended to that OTHER workspace's buffer, with an
					//     id from a stream that workspace's subscribers are
					//     not reading.
					//
					// Both are the same failure as an unparseable payload —
					// something reached this channel that this installation
					// did not publish — so they take the same route.
					slog.Error("Redis event names a different workspace than the channel it arrived on; ending this workspace's replay coverage",
						"channel", msg.Channel, "channel_workspace", workspaceID, "event_workspace", event.WorkspaceID, "id", event.ID)
					b.dropWorkspaceCoverage(workspaceID, ResetReasonUndecodableMessage, gen)
					continue
				}
				b.fanOutFromRedis(gen, epoch, event)
			}
		}
	}
}

// dropWorkspaceCoverage ends this instance's coverage of one workspace,
// because something happened that its buffer cannot account for. The next
// resume for that workspace answers nil until coverage is re-established.
//
// Scoped to the ONE workspace rather than all of them, unlike an ID-space
// reset: a dropped subscription says nothing about the shared counter or about
// any other workspace's channel, and dropping the rest would be a resync
// charged to clients whose stream never broke.
func (b *RedisBus) dropWorkspaceCoverage(workspaceID, reason string, gen int64) {
	var report string
	defer func() {
		if report != "" {
			b.reportReset(report)
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()

	// THE GENERATION CHECK BELONGS HERE TOO, not only in fan-out (codex round
	// 7). A receive loop can notice its connection died LONG after the
	// workspace was unsubscribed and resubscribed under it: last viewer
	// leaves, the buffer is dropped and the subscription torn down, a viewer
	// returns and a new generation starts buffering — and only then does the
	// old goroutine reach this line and delete the REPLACEMENT buffer. The
	// returning client is then told sync_required for an outage that ended
	// before its subscription began, and the reset counter names an incident
	// that did not happen to it.
	if sub, ok := b.wsSubs[workspaceID]; !ok || sub.gen != gen {
		return
	}

	if _, ok := b.replayBuffers[workspaceID]; !ok {
		// Nothing buffered: there is no coverage to END, so no buffer to drop
		// and nothing to report. Reporting here would give the reset counter
		// a baseline on every reconnect of an idle workspace.
		//
		// THE LIVE SUBSCRIBERS STILL GET TOLD, and that is not a contradiction
		// (codex round 4). "No buffer" says nothing was RECEIVED for this
		// workspace on this instance; it does not say nothing was PUBLISHED.
		// A subscriber that connected and then sat through a pub/sub outage
		// has exactly the hole this signal exists for, and it is the case
		// where the instance has the least idea what it missed. Answering
		// only when a buffer happens to exist would make the honesty
		// conditional on having already received something, which is
		// backwards.
		//
		// The asymmetry is deliberate: the metric measures COVERAGE ENDINGS
		// (there was none) and the signal measures CLIENTS WHO MAY HAVE
		// MISSED SOMETHING (there are some).
		b.signalWorkspaceLocked(workspaceID)
		return
	}
	delete(b.replayBuffers, workspaceID)
	// TELL THE SUBSCRIBERS THAT ARE STILL HOLDING THE STREAM OPEN (BUG-2730).
	// Ending coverage makes the next RESUME honest; it does nothing for a
	// client that never reconnects, and the reasons that reach here — a
	// subscription that dropped and resumed, a message we could not decode —
	// are exactly the ones where events went missing while those clients sat
	// there believing they were current.
	//
	// Scoped to this workspace's subscribers, matching the buffer drop
	// directly above: no other workspace's channel is implicated.
	b.signalWorkspaceLocked(workspaceID)
	report = reason
}

// signalWorkspaceLocked raises the gap flag for every live subscriber of one
// workspace. Callers must hold b.mu; every send is non-blocking, so holding it
// costs nothing and cannot deadlock.
func (b *RedisBus) signalWorkspaceLocked(workspaceID string) {
	for _, sub := range b.subscribers[workspaceID] {
		sub.signalGap()
	}
}

// signalAllLocked raises the gap flag for every live subscriber on this
// instance, for the resets that invalidate every workspace at once rather than
// one channel. Callers must hold b.mu.
func (b *RedisBus) signalAllLocked() {
	for _, byWorkspace := range b.subscribers {
		for _, sub := range byWorkspace {
			sub.signalGap()
		}
	}
}

// stampLastSeen records that this workspace's subscription just received
// something from Redis.
//
// GENERATION-CHECKED like every other bookkeeping write keyed by workspace: a
// receive loop can outlive its subscription (stopRedisSubscription only
// signals it, never joins it), and a straggler frame from a dead generation
// must not refresh the liveness of the one that replaced it. Without this
// check a wedged old loop's final buffered frames could keep a NEW subscription
// looking alive.
func (b *RedisBus) stampLastSeen(workspaceID string, gen int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.wsSubs[workspaceID]; ok && sub.gen == gen {
		sub.lastSeen = b.now()
	}
}

// currentSubGen reports the generation of the workspace's live subscription,
// or 0 if it has none. Test seam: a real message carries its generation down
// from receiveMessages, and a test driving the fan-out directly needs a way to
// name the same value rather than guessing it.
func (b *RedisBus) currentSubGen(workspaceID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.wsSubs[workspaceID]; ok {
		return sub.gen
	}
	return 0
}

// buffersHoldEvents reports whether any replay buffer has been written to.
// Callers must hold mu.
func (b *RedisBus) buffersHoldEvents() bool {
	for _, rb := range b.replayBuffers {
		if rb.count > 0 {
			return true
		}
	}
	return false
}

// newBuffer builds a replay buffer of the right flavour for this bus's
// history: once a sequence has been discarded under us, every buffer we build
// afterwards must refuse the cursor immediately below its first event, because
// that cursor may belong to the sequence we threw away. Callers must hold mu.
func (b *RedisBus) newBuffer() *replayBuffer {
	if b.hadReset {
		return newReplayBufferAfterReset(b.replaySize, b.discardedHighWater)
	}
	return newReplayBuffer(b.replaySize)
}

// dropAllBuffers throws away every replay buffer because this instance can no
// longer vouch for the sequence they describe — either the ID space itself
// changed, or an ID arrived at or below the high-water mark. Callers must hold
// mu.
//
// floor says what the replacements should do with the standing floor — refuse
// every cursor at or below what the discarded buffers held, clear it, or leave
// it alone — and the reset reasons answer it differently ON PURPOSE. Getting this backwards produces a
// RESYNC LOOP, which is worse than the bug the floor exists to fix.
//
//   - COUNTER BACKWARDS, no epoch change: we CANNOT TELL which of two things
//     happened, and that is the point. Either the arriving ID is in the same
//     numeric space and simply arrived out of order (a phase-1 publisher's
//     non-atomic INCR/PUBLISH, during any roll), or the counter genuinely
//     reset and no epoch was there to say so — which is every phase-1
//     deployment, since phase 1 publishes no epoch at all. Either way we have
//     just discarded events a cursor can legitimately ask about, so the floor
//     is raised.
//
//     THE COST OF RAISING IT ON A REAL PHASE-1 RESET is the resync loop the
//     epoch branch below avoids: the new sequence restarts low, so cursors are
//     refused until it climbs past the dead high-water mark, and each refusal
//     hands the client a fresh low cursor. It is bounded by that climb, and it
//     is LOUD. The alternative — not raising it — is a silent skip, and this
//     family chooses loud every time. Phase 2 removes the case entirely: a
//     genuine restart there rotates the generation and takes the branch below.
//
//   - EPOCH CHANGE: a genuinely NEW space, typically restarting from 1 while
//     the dead space had climbed high. Raising the floor there would refuse
//     every cursor until the new counter passed the old high-water mark — and
//     since each refusal hands the client a FRESH low cursor that is refused
//     again, that is a loop, not one resync. The ambiguity between an old and
//     a new ID of the same value is accepted instead, exactly as
//     internal/watchevents accepts it; see BUG-2736's trail for the numeric
//     base that would close it and why it is not this unit.
//
// An epoch change also CLEARS any standing floor, which is not the same as
// declining to raise one. The floor is a same-space device: it names IDs whose
// successors we discarded FROM THE SPACE WE WERE TRACKING. Once the space
// itself is gone those numbers mean nothing, and leaving the floor standing
// produces the very loop the paragraph above rules out — just reached from a
// bus that took a counter-backwards reset earlier in its life.
func (b *RedisBus) dropAllBuffers(floor floorAction) {
	switch floor {
	case floorRaise:
		for _, rb := range b.replayBuffers {
			if rb.lastAppendedID > b.discardedHighWater {
				b.discardedHighWater = rb.lastAppendedID
			}
		}
	case floorClear:
		b.discardedHighWater = 0
	case floorKeep:
		// Deliberately nothing.
	}
	b.hadReset = true
	b.replayBuffers = make(map[string]*replayBuffer)
	// Every workspace's coverage just ended, so every live subscriber has a
	// hole it does not know about (BUG-2730). Announced here rather than at
	// each caller so no future reset path can drop the buffers and forget to
	// say so.
	b.signalAllLocked()
}

// floorAction says what a buffer drop should do with the standing
// counter-backwards floor. Three intents rather than a boolean, because a
// boolean gave the third one no way to spell itself and it silently took the
// wrong branch (codex round 20).
type floorAction int

const (
	// floorClear: the ID space itself changed, so the numbers the floor names
	// belong to a sequence that no longer exists. Leaving it standing refuses
	// every cursor until the new counter climbs past a dead high-water mark,
	// and each refusal hands the client a fresh low cursor — a resync loop.
	floorClear floorAction = iota

	// floorRaise: same numeric space, and we just discarded events a cursor
	// can legitimately ask about, so every cursor at or below them is refused.
	floorRaise

	// floorKeep: WE DO NOT KNOW WHICH SPACE THIS IS. A lower generation inside
	// the straggler window is not proof of a new space — that is precisely the
	// question the window exists to defer — so it must not clear a floor an
	// earlier counter-backwards reset raised. Clearing it there let bare
	// traffic repopulate the buffers below the mark and serve a cursor above
	// them as though coverage were continuous.
	floorKeep
)

// payloadKind distinguishes the frames that arrive on a workspace's event
// channel. A heartbeat is BUS-INTERNAL: never an event, never buffered, never
// replayed, never fanned out, and never counted. It exists only so that
// silence on a socket becomes diagnostic (BUG-2738).
type payloadKind int

const (
	payloadEvent payloadKind = iota
	payloadHeartbeat
)

// decodePayload parses the "<epoch>|<id>|<json>" wire form publishScript emits,
// and ALSO accepts a bare JSON body with no prefix.
//
// THE BARE FORM IS NOT LEGACY-ONLY. It is what every phase-1 instance
// publishes — which, until an operator flips config.EventsPublishEpoch, is
// every instance — as well as what a pre-BUG-2736 binary publishes. Accepting
// it is what makes both rolls zero-loss in the new-receiving-old direction. It
// returns a ZERO epoch, which the receive path reads as "no ID-space
// information" and leaves the epoch bookkeeping untouched rather than treating
// it as a change.
//
// The reverse direction is not recoverable from this side: an instance running
// a PRE-phase-1 binary fails to unmarshal a prefixed payload and drops the
// event for its own clients, loudly. That asymmetry is the entire reason the
// flip is a second roll. See docs/deployment.md.
//
// Splitting on the FIRST two separators keeps a '|' inside the JSON body
// harmless: the epoch and the ID are both digits, so neither can contain one.
// The leading '{' check is what stops a JSON body that happens to contain two
// '|' characters from being mistaken for a prefixed payload — an epoch is
// never a JSON object.
func decodePayload(payload string) (payloadKind, int64, Event, error) {
	// CLASSIFIED BEFORE ANYTHING IS SPLIT OR UNMARSHALLED, and the ORDER is
	// what makes the frame safe (BUG-2738). "hb|1" has one separator and would
	// otherwise fall through to the bare-JSON branch and fail to unmarshal;
	// a future two-field frame would split into three and fail to parse an
	// epoch. Either way it would reach the caller as an error, and an error
	// here ends the workspace's coverage — so a liveness probe would
	// manufacture the resync it exists to prevent.
	//
	// THE KIND IS RETURNED RATHER THAN HANDLED AT THE CALL SITE so that no
	// future caller of this decoder can reintroduce that. The wire format is
	// this function's to know.
	if isHeartbeat(payload) {
		return payloadHeartbeat, 0, Event{}, nil
	}

	if parts := strings.SplitN(payload, "|", 3); len(parts) == 3 && !strings.HasPrefix(parts[0], "{") {
		epochPart, idPart, body := parts[0], parts[1], parts[2]
		epoch, err := strconv.ParseInt(epochPart, 10, 64)
		if err != nil {
			return payloadEvent, 0, Event{}, fmt.Errorf("payload epoch prefix %q is not an integer: %w", epochPart, err)
		}
		if epoch <= 0 {
			// Zero is this package's sentinel for "no ID-space information",
			// so a message may not carry it as a real generation — otherwise a
			// malformed publisher could make every receiver stop reconciling
			// while looking perfectly healthy.
			return payloadEvent, 0, Event{}, fmt.Errorf("payload epoch prefix %d is not a positive generation", epoch)
		}
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			return payloadEvent, 0, Event{}, fmt.Errorf("payload id prefix %q is not an integer: %w", idPart, err)
		}
		var event Event
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			return payloadEvent, 0, Event{}, fmt.Errorf("payload body is not an Event: %w", err)
		}
		event.ID = id
		if err := requirePositiveID(event.ID); err != nil {
			return payloadEvent, 0, Event{}, err
		}
		return payloadEvent, epoch, event, nil
	}

	var event Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return payloadEvent, 0, Event{}, fmt.Errorf("payload is neither <epoch>|<id>|<json> nor a bare Event: %w", err)
	}
	if err := requirePositiveID(event.ID); err != nil {
		return payloadEvent, 0, Event{}, err
	}
	return payloadEvent, 0, event, nil
}

// requirePositiveID is applied to BOTH wire forms, and being applied to both
// is the point (codex round 16). It lived inside the prefixed branch first, so
// a bare payload carrying id 0 or a negative was accepted — delivered with no
// SSE cursor for the client to advance to, and, once an epoch has been
// adopted, read as the sequence going backwards and used to discard every
// replay buffer.
//
// The sequence counts from 1, so a non-positive id is not something this
// installation published in either form. Returning an error routes it where an
// unreadable payload goes: this workspace's coverage ends and the next resume
// says so.
func requirePositiveID(id int64) error {
	if id <= 0 {
		return fmt.Errorf("payload id %d is not a positive sequence id", id)
	}
	return nil
}

// fanOutFromRedis is the receive path: a message that arrived on the
// subscription identified by gen, carrying the ID space it belongs to.
//
// A zero epoch means the payload carried no ID-space information (a phase-1 or
// pre-BUG-2736 publisher, or a direct test call) and the bookkeeping is left
// alone — silence is not evidence of a change.
//
// THAT LEAVES ONE NARROW HOLE DURING THE PHASE-2 ROLL, named here because it
// is a property of the design rather than an oversight (codex round 17). Once
// an epoch has been adopted, a bare message is TREATED AS BELONGING TO THE
// CURRENT SPACE, and it usually does: an un-flipped publisher INCRs the same
// counter. It does not if the counter reset between that publisher's
// assignment and its publish — an id from the dead space then lands in a
// buffer describing the new one.
//
// The alternative rules are worse. Refusing bare messages once an epoch is
// adopted would end coverage on EVERY un-flipped publish for the length of the
// roll, which is a resync storm; delivering them without buffering would put
// holes in the buffer that nothing records. And there is no discriminator: an
// id from the dead space and an id from an un-flipped publisher are both
// "above what we hold" and are otherwise identical.
//
// WHAT BOUNDS IT, stated with the limit that took a second look to see (codex
// rounds 17 and 21). The counter-backwards branch closes the window when this
// WORKSPACE next receives an id below the straggler's — which is the common
// case, because the straggler's id comes from a dead space that had climbed
// higher than the new one has.
//
// It is not guaranteed. The counter is global and the check is per workspace,
// so if OTHER workspaces consume ids past the straggler's value before this
// one publishes again, this workspace's next id is higher and nothing fires.
// The dead-space id then sits in the buffer, and a client resuming from just
// below it is served it as though it followed.
//
// A global high-water mark would close that, and would cost a storm: the check
// is armed during the phase-2 roll, when un-flipped publishers interleave
// routinely, and a global comparison fires on interleaves across ANY pair of
// workspaces rather than within one. That trade is the same one round 9
// settled when it armed this check on an adopted epoch at all. The residual
// belongs with the others that the client cursor's missing epoch would close —
// see BUG-2736.
func (b *RedisBus) fanOutFromRedis(gen, epoch int64, event Event) {
	b.fanOut(gen, epoch, event)
}

// fanOutLocally distributes an event to all local subscribers for the event's
// workspace and stores it in the replay buffer, with no id-space information.
func (b *RedisBus) fanOutLocally(event Event) {
	b.fanOut(anySubscription, 0, event)
}

// stragglerWindow bounds how long after adopting a generation a LOWER one is
// read as a message that was in flight at the rotation rather than as the
// generation counter having genuinely regressed. Generous by two orders of
// magnitude against pub/sub delivery latency, because the cost of being too
// long is a bounded delay before a loud recovery, while being too short costs
// an extra buffer drop — both loud, neither silent.
const stragglerWindow = 30 * time.Second

// defaultSubscribeConfirmTimeout bounds the wait for Redis to acknowledge a new
// subscription (BUG-2747).
//
// IT BOUNDS THE ACKNOWLEDGEMENT, NOT THE WHOLE OF ESTABLISHMENT (codex round
// 7). The dial and go-redis's HELLO/AUTH handshake happen inside
// client.Subscribe, before this timer starts, and they are bounded by the
// CLIENT's DialTimeout — 5s by default. So a stalled dial followed by this
// wait composes to roughly DialTimeout + this, and anyone reasoning about
// worst-case connect latency needs both numbers.
//
// AMENDED BY BUG-2749 on the context half, which this comment used to state
// flatly as "not by any context we pass". Since that unit the dial runs on the
// CALLER's context, and go-redis derives its per-attempt dial deadline from it
// — so on plaintext a caller that goes away no longer waits out DialTimeout.
// Under TLS it still does: the default dialer hands a TLS connection to
// tls.DialWithDialer, which takes no context at all. The composed worst case
// above is therefore unchanged for a client that STAYS, and unchanged for a
// TLS deployment whose client leaves; it shrinks only for a departing client
// on a plaintext connection. See BUG-2754 for the TLS half.
//
// SHORT BECAUSE THE DISTRIBUTION HAS NO MIDDLE, not as a guess at how fast
// Redis is. Establishment either completes in single-digit milliseconds or does
// not complete at all, so past the top of the fast mode, waiting longer buys
// nothing — it only holds an SSE admission slot, global and per-workspace, for a
// client that may already be gone (BUG-2749).
//
// Measured on a containerised Redis over loopback, 300 establishments, timing
// the whole of Subscribe (dial, HELLO/AUTH, SUBSCRIBE and the acknowledgement):
// p50 388µs, p99 679µs, max 1.73ms idle; p50 693µs, p90 5.1ms, p99 12.1ms, max
// 18.5ms with 24 busy loops on 8 cores. One second is ~54x the loaded maximum.
// A real deployment adds network RTT to every sample, which moves the fast mode
// by milliseconds and does not put anything in the middle.
//
// The cost of being too short is an admission this instance cannot describe the
// coverage of — which since this change is counted, logged, and reconciled to
// the client when the acknowledgement lands. That is the safe-and-noisy
// direction; waiting is the silent one.
const defaultSubscribeConfirmTimeout = time.Second

// anySubscription opts out of the generation check for callers that are not a
// receive loop — tests driving the fan-out directly. A real message always
// carries the generation of the subscription it arrived on.
const anySubscription int64 = 0

func (b *RedisBus) fanOut(gen, epoch int64, event Event) {
	// Registered FIRST so it runs LAST — after the Unlock below, so an
	// observer may call back into the bus without deadlocking the receive
	// loop.
	var reset string
	dropped := 0
	defer func() {
		if reset != "" {
			b.reportReset(reset)
		}
		for range dropped {
			b.reportDropped(DropReasonSlowSubscriber)
		}
	}()

	// Events received via Redis pub/sub already carry a global ID assigned by
	// the publishing instance via Redis INCR. We use that ID directly so all
	// instances share the same ID space for Last-Event-ID replay.
	b.mu.Lock()
	defer b.mu.Unlock()

	// FAN-OUT REFUSES TO APPEND FOR A WORKSPACE WITH NO LIVE SUBSCRIPTION
	// (BUG-2731). sub.cancel() in stopRedisSubscription signals the receive
	// goroutine; it does not join it, so a message already in flight can
	// arrive here after the workspace was dropped. Appending it would
	// re-create the buffer with knownFrom set to that straggler's ID — a
	// one-entry buffer vouching for coverage that ended when the
	// subscription did, which is precisely the state the drop exists to
	// prevent.
	//
	// Checked under the same lock that guards wsSubs, so the answer cannot
	// go stale between the check and the append.
	sub, receiving := b.wsSubs[event.WorkspaceID]
	if !receiving {
		return
	}
	// ...AND THAT THIS MESSAGE BELONGS TO THE SUBSCRIPTION THAT IS LIVE NOW
	// (codex round 1 F2). Checking only that SOME subscription exists leaves
	// the same hole one step further along: last subscriber leaves, buffer
	// dropped, a NEW subscriber arrives and registers a new subscription, and
	// only THEN does the old receive goroutine's in-flight message land. It
	// would be appended, setting knownFrom to a pre-outage ID, so a resume
	// from that ID is answered "caught up" while the outage's events are gone.
	if gen != anySubscription && sub.gen != gen {
		return
	}

	// ID-SPACE RECONCILIATION (BUG-2736). Both checks answer the same question
	// — do the events already buffered belong to the same sequence as this
	// one? — and both are needed, because neither sees the other's case. The
	// epoch catches a reset that has already climbed past our high-water mark,
	// which is numerically invisible; the high-water check catches a reset on
	// an instance that never learned the previous epoch, including one
	// publishing from a phase-1 or pre-BUG-2736 binary.
	//
	// Every buffer is dropped, not just this workspace's: the counter is
	// global, so a reset invalidates all of them at once.
	if epoch != 0 && epoch < b.epoch && time.Since(b.epochAdoptedAt) < stragglerWindow {
		// A STRAGGLER FROM A SPACE WE HAVE LEFT (codex round 3). Each
		// workspace has its own subscription and its own receive goroutine,
		// and Redis orders messages within a channel but not across them — so
		// a message published before a rotation, on workspace A's channel, can
		// arrive after the rotation was already learned from workspace B's.
		//
		// It is DISCARDED, not merely ignored for bookkeeping. Its ID belongs
		// to the dead sequence, so appending it would put two spaces in one
		// buffer, which is exactly what the epoch exists to prevent; and its
		// subscribers have already been told to resync across the change, so
		// delivering it now would replay a fragment of a space they have
		// abandoned.
		//
		// With an unordered epoch this branch was unreachable and the message
		// instead flipped the bus BACK to the dead generation, dropping every
		// buffer a second time and making the "one drop per instance per roll"
		// property false.
		// THE MESSAGE IS DISCARDED AND SO IS OUR COVERAGE (codex round 19).
		// Discarding alone left the buffers claiming a span they could still
		// answer, so a client reconnecting during the window was told it was
		// caught up — while, if this is a REGRESSION rather than a straggler,
		// the messages being discarded are the live stream. Thirty seconds of
		// real events missed, silently, by a bus that had already decided it
		// could not classify what it was seeing.
		//
		// Ending coverage is the honest reading of "we cannot tell which space
		// this belongs to". The classification still waits out the window —
		// the epoch is NOT adopted here — so a true straggler does not drag
		// the bus into the dead space. Its cost is one extra drop next to a
		// rotation that had already dropped the buffers, which is nearly free
		// and loud either way.
		slog.Warn("events: discarding a message from an abandoned ID space and ending replay coverage",
			"message_epoch", epoch, "current_epoch", b.epoch, "id", event.ID, "workspace", event.WorkspaceID)
		if b.buffersHoldEvents() {
			b.dropAllBuffers(floorKeep)
			reset = ResetReasonEpochRegressed
		}
		return
	}
	if epoch != 0 && epoch < b.epoch {
		// THE GENERATION WENT BACKWARDS AND STAYED THERE, so this is not a
		// straggler — it is the world moving backwards. The realistic cause is
		// a Redis failover to a replica whose copy of the generation counter
		// predates the rotation, after which every publisher mints from the
		// lower number.
		//
		// WITHOUT THIS BRANCH the discard above would run forever: every
		// post-failover message dropped, no events delivered, no buffers
		// filled, and the only trace a log line per message. Silent and
		// unbounded is the one outcome this family refuses — so a persistent
		// regression is ACCEPTED as a new space, which drops every buffer and
		// makes the next resume answer sync_required. Loud and recoverable.
		//
		// THE WINDOW IS A PHYSICAL QUANTITY, not a guess about intent: a
		// straggler is a message that was in flight at the instant of the
		// rotation, so it is bounded by pub/sub delivery latency. Anything
		// arriving a long time after the adoption cannot be one. Both ways of
		// being wrong are loud — too short costs an extra buffer drop, too
		// long costs a few seconds of discards before recovery.
		slog.Warn("events: the ID space generation went backwards and stayed there; treating it as a new space and dropping replay buffers",
			"previous_epoch", b.epoch, "new_epoch", epoch, "id", event.ID, "workspace", event.WorkspaceID)
		b.dropAllBuffers(floorClear)
		b.epoch = epoch
		b.epochAdoptedAt = time.Now()
		reset = ResetReasonEpochRegressed
	} else if epoch > b.epoch {
		// ADOPTING AN EPOCH ONTO A NON-EMPTY BUFFER IS ALSO A RESET. Learning
		// an epoch for the first time normally means the first message of this
		// bus's life, and dropping empty buffers would be pointless. But
		// during the phase-2 roll the buffers can already hold events from a
		// phase-1 publisher, whose payloads carry no epoch at all — and those
		// events' ID space is exactly what we have no way to compare against
		// the one we are now being told about.
		//
		// The dangerous shape: bare events up to 5, the counter is then
		// deleted, a flipped publisher rotates the epoch and climbs to 6
		// before this instance receives anything. 6 exceeds our high-water
		// mark, so the numeric check sees an ordinary successor, and without
		// this branch the two spaces merge in one buffer.
		//
		// Costs at most ONE drop per instance per roll: once adopted, later
		// bare messages leave the epoch alone and later prefixed ones match.
		//
		// WHAT THIS DELIBERATELY DOES NOT COVER: a bus whose buffers are
		// EMPTY adopts without dropping, so its first buffer starts at exactly
		// the first ID it sees. A client holding the ID one below that — from
		// a space this process never saw, because it started through a
		// cutover — is then served. Closing it locally means every bus
		// refusing the adjacent cursor forever, which trades a RARE silent
		// skip for a COMMON extra resync: on a multi-instance deployment a
		// client legitimately holds ID 149 from replica A and reconnects to
		// replica B whose first ID for that workspace is 150, and consecutive
		// global IDs land in the same busy workspace routinely. BUG-2736's
		// trail names the numeric-base design that closes it with neither
		// cost, and why it is a follow-on rather than this unit.
		if b.epoch == 0 && !b.buffersHoldEvents() {
			// ADOPTING COLD. No buffers to invalidate, so no reset is
			// reported — deliberately, since otherwise every replica would
			// count one at startup and the reset metric would grow a
			// per-deploy baseline instead of meaning something.
			//
			// It is logged rather than counted (codex round 9) because it is
			// the moment the documented residual becomes possible on this
			// replica: from here its first buffer starts at the first ID it
			// sees, so a client holding the ID one below that — from a space
			// this process never saw — is served. An operator correlating
			// "clients reported missing events" against "which replica joined
			// a space cold, and when" has nothing else to go on.
			slog.Info("events: adopting an ID space with no buffered history; resumes adjacent to this instance's first id cannot be checked against the previous space",
				"epoch", epoch, "id", event.ID, "workspace", event.WorkspaceID)
		}
		if b.epoch != 0 || b.buffersHoldEvents() {
			slog.Warn("event ID space changed; dropping replay buffers, resumes spanning the change will report sync_required",
				"previous_epoch", b.epoch, "new_epoch", epoch, "id", event.ID)
			b.dropAllBuffers(floorClear)
			reset = ResetReasonEpochChange
		}
		b.epoch = epoch
		b.epochAdoptedAt = time.Now()
	}

	// Store in replay buffer for reconnect replay.
	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = b.newBuffer()
		b.replayBuffers[event.WorkspaceID] = rb
	}
	if b.epoch != 0 && rb.lastAppendedID != 0 && event.ID <= rb.lastAppendedID {
		// IT ONLY RUNS ONCE AN EPOCH HAS BEEN ADOPTED, and that gate is not a
		// detail (codex round 9). Phase 1 publishes with a two-call
		// INCR-then-PUBLISH, so on any multi-instance deployment two
		// publishers interleave routinely and a lower ID arrives after a
		// higher one as ORDINARY TRAFFIC. Without the gate this branch would
		// fire on that, dropping every replay buffer and resyncing every
		// client — in the DEFAULT configuration, which is where every
		// deployment sits until an operator flips phase 2. That is a
		// regression this diff would have introduced, not a defect it finds.
		//
		// What the gate costs: a genuine counter reset on a
		// never-flipped deployment goes undetected. That is exactly the
		// behaviour before this change, so nothing regresses — and it is
		// precisely the case phase 2 exists to fix, by making resets announce
		// themselves as a generation change instead.
		//
		// THIS WHOLE MECHANISM IS TRANSITIONAL. READ THIS BEFORE TUNING IT.
		//
		// Once every publisher is flipped to phase 2, publish order equals ID
		// order globally, and a genuine counter restart rotates the epoch (see
		// publishScript's id == 1 branch), which is a different code path
		// entirely. So a backwards ID in steady state is an ANOMALY, not a
		// case needing clever classification. Its real job is the ROLL WINDOW,
		// where a phase-1 publisher's non-atomic INCR/PUBLISH can deliver a
		// lower ID after a higher one.
		//
		// IT DOES NOT GO AWAY WHEN THE FORMAT FLIP COMPLETES, which the
		// original scoping hoped it would. The trigger is mixed-VERSION
		// ORDERING — older binaries assign and publish in two calls — not
		// mixed-format payloads, so publish-old-until-flip removes the
		// mixed-FORMAT window only. The floor lives for as long as any
		// deployment can run two publisher versions at once, which is every
		// rolling upgrade.
		//
		// It has already been tuned four times, each round finding a defect
		// inside the previous round's cleverness. So the floor is raised
		// UNCONDITIONALLY: every cursor at or below what the discarded buffers
		// held is refused. The alternative was a discriminator on "a restart
		// always begins at 1", which an out-of-order ID 1 during a roll
		// defeats — silently. Refusing too much is loud: it shows up in
		// pad_event_resume_gaps_total and in the warning below. Refusing too
		// little loses events with nothing to show for it.
		//
		// THE COST, stated so nobody rediscovers it as a bug: if a phase-1
		// publisher restarts the counter mid-roll, cursors are refused until
		// the sequence climbs past the dead high-water mark, so affected
		// clients resync repeatedly. Bounded by the roll — once every
		// publisher is flipped, restarts rotate the epoch, and an epoch change
		// CLEARS the floor (see dropAllBuffers).
		// WHY A PER-WORKSPACE CHECK IS ENOUGH FOR A GLOBAL COUNTER, since
		// two separate reviews have now asked (BUG-2740 rounds 5 and 8) and
		// the answer is not obvious from here.
		//
		// The sequence is shared across workspaces, so a restarted counter
		// reissues ids that other workspaces may consume — and this compares
		// only against THIS workspace's high-water mark. That is sufficient
		// because the unit of coherence is the per-workspace buffer and the
		// per-workspace cursor: an id can only CORRUPT a buffer by colliding
		// with one already in it, and an id at or below this workspace's
		// high-water mark is exactly what this arm catches. A reissued id
		// that lands in a DIFFERENT workspace collides with nothing here.
		//
		// Measured rather than argued, both shapes: a counter DELETED
		// restarts at 1, which takes publishScript's id == 1 branch and
		// rotates the epoch unconditionally, so every buffer is dropped
		// through epoch_change before this arm is reached. A counter SET
		// backwards above 1 skips that rotation — and then a workspace whose
		// own ids are reissued sees them as backward and lands here, while a
		// workspace that never held them keeps a strictly increasing stream
		// and needs no reset.
		slog.Warn("event sequence went backwards; dropping replay buffers, resumes below the discarded high-water mark will report sync_required",
			"high_water_mark", rb.lastAppendedID, "id", event.ID, "workspace", event.WorkspaceID)
		b.dropAllBuffers(floorRaise)
		rb = b.newBuffer()
		b.replayBuffers[event.WorkspaceID] = rb
		reset = ResetReasonCounterBackward
	}
	rb.append(event)

	for _, sub := range b.subscribers[event.WorkspaceID] {
		select {
		case sub.ch <- event:
		default:
			// A DROP HERE WAS SILENT TO THE SUBSCRIBER until BUG-2730, and
			// the consequence is why it mattered: a later delivered event
			// advances that client's Last-Event-ID PAST the dropped IDs,
			// after which no replica will replay them, because every replica
			// agrees the cursor is current.
			//
			// Everything BUG-2731 made honest is per-WORKSPACE state
			// evaluated when a client ASKS (cold start, stopped subscription,
			// reconnect, ID-space reset). This is per-SUBSCRIBER state
			// discovered mid-fan-out about a connection that is still open,
			// so telling it needed a channel from the bus to one live
			// consumer that neither bus had. It has one now.
			slog.Warn("dropping event for slow subscriber", "type", event.Type, "workspace", event.WorkspaceID)
			sub.signalGap()
			dropped++
		}
	}
}
