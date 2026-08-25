package watchevents

import (
	"context"
	"encoding/json"
	"errors"
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
	// redisWatchChannelSuffix is the single Redis pub/sub channel every instance
	// publishes to and subscribes to.
	//
	// SINGLE, not per-workspace, unlike internal/events' pad:events:<wsID>.
	// That is not a simplification of the template — it is this package's
	// contract: there is exactly one logical watch stream and ALL per-caller
	// filtering happens in the consumer (DOC-2479 DR-2, "no firehose, no
	// wildcard subscriptions"). Subscribe() takes no workspace precisely
	// because a subscriber is not scoped to one, so there is nothing to key a
	// channel by.
	redisWatchChannelSuffix = "watchevents"

	// redisWatchSeqSuffix is the shared counter behind Notification.ID.
	//
	// Deliberately distinct from internal/events' pad:event_seq: the two
	// buses carry different streams with independent Last-Event-ID spaces,
	// and sharing a counter would make each one's ids jump unpredictably
	// whenever the other published — harmless for ordering, but it would
	// turn every replay-gap diagnosis into a question about the other bus.
	redisWatchSeqSuffix = "watchevents_seq"

	// redisWatchEpochSuffix identifies the CURRENT id space, and exists because
	// numeric detection alone cannot see a reset that has caught back up
	// (codex round 13).
	//
	// The counter resetting is detectable when an id arrives BELOW our
	// high-water mark. It is invisible when the new space has already climbed
	// past it: hold 100, lose the connection, the counter resets and ids 1-101
	// are published, and the only one that reaches us is 101 — which looks
	// exactly like the contiguous successor of 100. The buffer then mixes two
	// id spaces and a client resuming from OLD 100 is handed NEW 101, having
	// silently missed the new space's 1-100.
	//
	// An epoch is the only thing that distinguishes them, because the
	// question is not "is this number bigger" but "is this the same
	// sequence". Minted once per id space by the publish script and carried
	// on every message.
	//
	// AN OPAQUE TOKEN HERE, A NUMERIC BASE ON MemoryBus (BUG-2736), and the
	// two are NOT interchangeable spellings of one idea. The reason is stated
	// once, in internal/idspace's package comment.
	redisWatchEpochSuffix = "watchevents_epoch"

	// DEPLOYMENT SCOPING (BUG-2724). These names carry the installation's
	// PAD_REDIS_NAMESPACE when one is set — see internal/redisns — so two
	// Pad installations can share a Redis endpoint without cross-feeding
	// notifications or sharing an id counter. With none configured they
	// are byte-identical to the historical flat names, so an existing
	// deployment upgrades without losing its counter, epoch or replay
	// position.
	//
	// The rule: scoping belongs to EVERY keyspace at once, from shared
	// config, never to one file growing a prefix the others lack — an
	// operator rule covering two of three keyspaces is harder to state
	// than the flat one it replaces.
	//
	// STILL UNSCOPED, deliberately: Redis CLUSTER. No hash tags, and
	// publishScript below spans FOUR keys in one EVAL — seq, channel,
	// dedupe, epoch — which hash to different slots and fail CROSSSLOT.
	// Tagging this keyspace alone buys nothing with no cluster client to
	// exercise it against; deferred as one unit on BUG-2724's trail.
	//
	// NOTE FOR A NAMESPACE CUTOVER: the seq and epoch keys carry
	// Last-Event-ID meaning, so renaming them mid-flight makes connected
	// clients resync — honest rather than silent, since the epoch
	// mechanism detects it, but plan it. Presence keys are transient and
	// free to rename.
)

// publishScript assigns the next id and publishes, ATOMICALLY.
//
// Doing those as two client calls is not merely unlocked, it is actively
// order-breaking, and the failure is not theoretical (Codex round 1 P1):
// instance A runs INCR and gets 1, is descheduled; instance B runs INCR, gets
// 2, and publishes; A then publishes 1. Every subscriber receives 2 before 1.
// The replay buffer appends in ARRIVAL order, so it now holds a non-monotonic
// sequence, and replayBuffer.since() reasons on monotonicity: a resume from 2
// takes the `sinceID > newestID` branch and answers "gap too large", turning a
// perfectly healthy reconnect into a spurious sync_required, while a resume
// from 1 silently skips the notification that arrived late.
//
// Redis executes a script atomically on its single thread, so INCR and PUBLISH
// for one instance both complete before another instance's script begins:
// publish order equals id order, globally, with no coordination on our side.
//
// The epoch and id are PREPENDED as "<epoch>|<id>|<json>" rather than injected
// into the JSON, because string-editing JSON inside Lua is a fragile way to
// save a split. The epoch is a uuid and the id is digits, so neither can
// contain a '|' and splitting on the first two is unambiguous however the body
// is punctuated.
//
// The epoch is not decoration: it is what distinguishes a reset id space from a
// continuing one when the new space has already climbed past an instance's
// high-water mark. See redisWatchEpochSuffix before considering it removable.
// The dedupe key makes the script IDEMPOTENT UNDER RETRY, which it needs to be
// because go-redis retries a command when the reply is lost to a network error
// (codex round 5). Without it, a script that ran and whose reply never came
// back would run AGAIN on retry: the same notification published twice under
// two different ids. Both copies look perfectly valid — correct ordering,
// distinct ids — so nothing downstream could tell them apart, and on the push
// path a duplicate is a duplicate DISPATCH into an agent harness, not just a
// repeated line in a feed.
//
// SET NX on a caller-generated token turns the retry into a no-op: the retry
// carries the same KEYS[3], the SET fails, and the script returns 0 without
// publishing. The TTL only has to outlive a retry burst, not a session.
var publishScript = redis.NewScript(`
if redis.call('SET', KEYS[3], '1', 'NX', 'EX', ARGV[2]) == false then
  return 0
end
redis.call('SET', KEYS[4], ARGV[3], 'NX')
local epoch = redis.call('GET', KEYS[4])
local id = redis.call('INCR', KEYS[1])
redis.call('PUBLISH', KEYS[2], epoch .. '|' .. id .. '|' .. ARGV[1])
return id
`)

const (
	// redisWatchDedupeSuffix namespaces the per-publish idempotency tokens.
	redisWatchDedupeSuffix = "watchevents:pub:"

	// redisWatchDedupeTTLSeconds bounds how long a token is remembered. It
	// only has to cover a client-side retry burst — go-redis gives up after
	// MaxRetries with backoff measured in milliseconds — so a minute is
	// generous, and the keys are small and expire on their own.
	redisWatchDedupeTTLSeconds = 60
)

// RedisBus is the multi-instance implementation of Bus (BUG-2651). Every
// instance publishes to one shared Redis channel and receives from it, so a
// Notification produced on instance A reaches a stream held open on instance B
// — the gap MemoryBus's package doc has flagged since this package existed.
//
// THREE THINGS DIFFER FROM internal/events.RedisBus, which is otherwise the
// template. Each is deliberate, and each would look like a mistake to someone
// diffing the two:
//
//  1. One channel and one replay buffer (see redisWatchChannelSuffix).
//
//  2. The Redis subscription is opened EAGERLY in NewRedisBus and lives for
//     the bus's lifetime, rather than being started on the first local
//     subscriber and torn down after the last. The replay buffer is filled by
//     the RECEIVE path, so a lazily-torn-down subscription stops filling it at
//     exactly the moment before a Last-Event-ID resume is attempted — for the
//     common shape here (one harness monitor holding one stream, reconnecting)
//     that would make resume structurally useless. events.RedisBus can afford
//     lazy because per-workspace means N idle subscriptions; here it is one per
//     instance, forever, which is the whole trade.
//
//  3. ONE mutex guards subscriber membership and the replay buffer together,
//     and it is held across the entire local fan-out. events.RedisBus uses two
//     (mu + replayMu) and exposes only separate Subscribe + EventsSince — a
//     structure that cannot provide SubscribeAndReplaySince's guarantee. See
//     that method's comment; reproducing the template's locking here would
//     hand back a double-delivery window this package's interface exists to
//     close.
//
// A publisher's OWN subscribers are served through the Redis round trip like
// everyone else's — Publish never fans out locally. That costs a round trip of
// latency and buys NO-DOUBLE-DELIVERY: a local send plus the echo back from
// Redis would deliver twice to the publishing instance's own streams.
//
// Not "exactly-once", which an earlier draft of this comment claimed and which
// is not on offer at any layer here (Codex round 1). Redis pub/sub is
// at-most-once — a subscriber that is disconnected when a message is published
// never sees it — and the local send is deliberately non-blocking, so a slow
// subscriber's notification is dropped and logged rather than awaited. The
// replay buffer plus Last-Event-ID is the recovery mechanism for both, and it
// is bounded: DefaultReplayBufferSize entries, after which a resume answers
// "gap too large" and the consumer resyncs.
type RedisBus struct {
	// observable carries the optional operational-event seam (BUG-2727).
	// Embedded first so SetObserver is part of the type's own surface
	// rather than something a caller reaches through a field.
	observable

	client *redis.Client

	// keys builds this installation's Redis names (BUG-2724). The zero
	// value is the historical un-namespaced keyspace.
	keys redisns.Keys

	// mu guards subscribers AND replay together — see the type comment (3)
	// and SubscribeAndReplaySince.
	mu          sync.Mutex
	subscribers map[chan Notification]*subscriber
	replay      *replayBuffer
	replaySize  int
	closed      bool

	// afterSubscribeRegister is a test-only seam, nil in production and thus
	// zero-cost there. SubscribeAndReplaySince calls it, while still holding
	// mu, at the exact instant AFTER the subscriber is registered and BEFORE
	// the replay buffer is read — the boundary that the single mutex
	// collapses into one atomic step, and the boundary a reintroduced
	// split-lock layout (events.RedisBus's template; see the type comment
	// (3)) would place its unlock/relock around.
	//
	// TestRedisBusSubscribeAndReplayNeverDoubleDelivers uses it to force a
	// concurrent fanOutLocally to attempt the lock right here, on every
	// attempt, instead of racing an unsynchronized goroutine and hoping for a
	// microsecond of overlap (which is what made that test flake — see its
	// comment). Because the hook fires while mu is still held, the forced
	// fan-out is provably blocked until the read+return completes; that is
	// what the test is asserting still holds.
	//
	// The seam's value is tied to its POSITION, not its mere existence: if
	// SubscribeAndReplaySince is ever restructured — e.g. split into two
	// critical sections — this call must move with the register/read
	// boundary, or the test starts exercising a spot that no longer
	// corresponds to where a real regression would open a window, and passes
	// while proving nothing.
	afterSubscribeRegister func()

	// beforeSettleWait is a test-only seam, nil in production. It runs in
	// resumeOutrunsLocalView after the first counter read has DISAGREED and
	// immediately before the settle wait begins.
	//
	// POSITIONAL, like its neighbour. It is the only point at which a test can
	// land a cancellation strictly inside the window rather than racing it with
	// a sleep — and a sleep-timed cancellation that lands late does not fail
	// safe here, it just measures a different path (BUG-2751).
	beforeSettleWait func()

	// knownFrom / lastAppendedID bound what this instance can HONESTLY
	// replay — a failure mode MemoryBus structurally cannot have and this
	// one can (codex rounds 3 and 4).
	//
	// MemoryBus assigns every id itself, so its buffer is contiguous by
	// construction and replayBuffer.since()'s only gap case is eviction —
	// the buffer being full and the caller asking for something older than
	// the oldest entry. Here the ids come from Redis, and this instance's
	// view has TWO ways of not covering what a client asks for:
	//
	//  1. A HOLE. Delivery is at-most-once, so a subscription that blips can
	//     miss 101 and receive 102. The buffer then holds a hole, is nowhere
	//     near full, and since() would answer a resume from 100 with just
	//     [102] — 101 silently lost, no sync_required.
	//  2. A COLD START, which the first version of this missed. A replica
	//     that restarts while Redis is at 101 has an EMPTY buffer and no
	//     lastAppendedID, so its first received message (say 102) recorded no
	//     hole at all. A client reconnecting to it with Last-Event-ID 100
	//     silently skipped 101 by exactly the same route.
	//
	// knownFrom collapses both: it is the lowest id from which this
	// instance's buffer is contiguous. It is SET on the first append (before
	// which we know nothing) and RESET on every hole (before which we no
	// longer know anything usable). A resume from below it is answered as a
	// gap. The atomic publish script is what makes a non-consecutive id
	// readable as MISSED rather than merely reordered.
	knownFrom      int64
	lastAppendedID int64

	// highWaterID is the highest id this instance has EVER appended in the
	// current epoch, and it exists because lastAppendedID does not survive
	// dropCoverage (BUG-2739, codex round 13).
	//
	// Backward-counter detection reads "did an id arrive at or below what we
	// already hold", and dropCoverage answers 0 to that question — so a
	// counter reset that happens DURING the outage that caused the drop
	// arrives afterwards looking like an ordinary cold start, and
	// counter_backward is never reported. That composite is not exotic: a
	// Redis restarted from a stale snapshot drops every connection (the
	// resubscription) and restores watchevents_seq to an older value (the
	// reset), in one event.
	//
	// It changes no coverage decision — both roads reach knownFrom = n.ID
	// over an emptied buffer — so this is purely the OPERATOR's signal, which
	// docs/deployment.md leans on per migration phase. Reset only when the
	// epoch changes, since ids from a new space are not comparable with it.
	highWaterID int64

	// epoch identifies the id space these ids belong to. A change means the
	// counter was reset and the buffer describes a sequence that no longer
	// exists — see redisWatchEpochSuffix and fanOutFromRedis.
	epoch string
	// epochJustChanged makes the next cold start refuse the
	// contiguous-with-our-view cursor, because across an epoch boundary that
	// cursor is ambiguous rather than contiguous. See fanOutLocally.
	epochJustChanged bool

	pubsub *redis.PubSub
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// afterProbePublish is a test-only seam, nil in production. It runs in
	// publishHeartbeats after the publish and BEFORE lastProbeOK is stamped —
	// the window in which a slow publish can have its subscription replaced
	// underneath it, and the only place a test can force that interleave.
	afterProbePublish func()

	// beforeDropHook is a test-only seam, nil in production. It runs in
	// cycleIfIdle after the decision to cycle and BEFORE the re-validation that
	// precedes the drop — the only point at which a test can land a recovery
	// strictly inside that window rather than racing it.
	beforeDropHook func()

	// subGen numbers subscriptions so a frame can be matched to the one it
	// arrived on (BUG-2769, codex round 1).
	//
	// stopping a receive loop only SIGNALS it — cancelling its context and
	// closing its PubSub does not join the goroutine, and go-redis's channel is
	// buffered — so a straggler from a subscription that has already been
	// replaced can still reach the switch. Without a generation it would stamp
	// the REPLACEMENT's liveness, append to the replacement's buffer, or drop
	// the replacement's coverage, all on the strength of a frame that arrived
	// on a dead socket. Guarded by mu.
	subGen int64

	// subCancel ends the CURRENT subscription's receive loop without ending the
	// bus. The idle cycle calls it before closing the old PubSub so the
	// replaced loop exits quietly instead of reporting the instance deaf
	// (BUG-2769). Guarded by mu.
	subCancel context.CancelFunc

	// lastSeen is when this instance last received ANYTHING on its watch
	// subscription — a notification, a subscription confirmation, or a
	// heartbeat. Guarded by mu.
	//
	// ONE FIELD, NOT A MAP, and that is this bus's shape rather than a
	// simplification: there is a single process-wide subscription on a single
	// channel (see the type comment), so liveness is a property of the
	// instance. internal/events needs a per-workspace stamp because it holds a
	// PubSub per workspace; none of that structure exists here.
	lastSeen time.Time

	// lastProbeOK is when this instance last SUCCEEDED in publishing a
	// heartbeat. Guarded by mu.
	//
	// It is the detector's PREMISE. Idle detection reasons "we published a
	// frame and nothing came back, so the receive path is dead", which is only
	// valid if the publish happened: PUBLISH travels on the client's ordinary
	// connection pool while the subscription holds one from the separate
	// pub/sub pool, so a publish-side failure says nothing about whether this
	// subscription can receive. Without it the detector reads its own inability
	// to probe as evidence about the peer.
	lastProbeOK time.Time

	// publishHeartbeat selects whether this instance emits liveness frames AND
	// runs idle detection — one switch, because an instance detects off its own
	// frames. Constructor parameter with no default, so every call site states
	// its phase. See config.WatchHeartbeat.
	publishHeartbeat bool

	// heartbeatInterval is T and idleTimeout is 3T. Guarded by mu.
	heartbeatInterval time.Duration
	idleTimeout       time.Duration

	// heartbeatKick and idleKick wake their loops when the cadence changes.
	//
	// ONE PER LOOP, and this was ported wrong first: a SHARED channel is
	// consumed by whichever goroutine happens to be waiting, leaving the other
	// on the stale cadence. internal/events' mutation matrix caught that there
	// (M11c) and the fix did not come across with the structure — the wiring
	// test here failed for exactly that reason.
	heartbeatKick chan struct{}
	idleKick      chan struct{}

	// nowFunc overrides the clock behind idle detection. Nil in production.
	nowFunc func() time.Time
}

// NewRedisBus creates a Redis-backed Bus with the default replay buffer size
// and opens the shared subscription immediately. The client should already be
// configured; the caller is expected to have pinged it (cmd/pad/cmd_server.go
// does, before either bus is constructed).
func NewRedisBus(client *redis.Client) *RedisBus {
	return NewRedisBusWithReplaySize(client, DefaultReplayBufferSize)
}

// NewRedisBusWithReplaySize is NewRedisBus with a custom replay capacity
// (tests use a small one, exactly like NewWithReplaySize).
func NewRedisBusWithReplaySize(client *redis.Client, size int) *RedisBus {
	return NewRedisBusWithKeys(client, size, redisns.Default, false)
}

// NewRedisBusWithKeys is NewRedisBusWithReplaySize with an explicit key
// namespace (BUG-2724). cmd/pad/cmd_server.go uses this one, passing the
// value shared with the event bus and the presence registry.
//
// NOTE for anyone introducing a namespace on a RUNNING deployment: the
// seq and epoch keys carry Last-Event-ID meaning, so renaming them is a
// resume-gap event — connected clients' cursors belong to the old id
// space and will be answered with sync_required. The epoch mechanism
// makes that safe rather than silent, but plan the cutover; it is not a
// free config change. Presence keys, by contrast, are transient (90s TTL)
// and cost nothing to rename.
// publishHeartbeat selects whether this instance emits liveness frames AND runs
// idle detection (BUG-2769) — one switch, because an instance detects off its
// own frames. A constructor parameter with no default, the same shape
// internal/events uses for its two rollout flags, so every call site states
// which phase it is in. See config.WatchHeartbeat for why the order of the two
// rolls is not optional.
func NewRedisBusWithKeys(client *redis.Client, size int, keys redisns.Keys, publishHeartbeat bool) *RedisBus {
	if size <= 0 {
		// newReplayBuffer(0) returns a buffer whose first append panics on a
		// zero-length backing slice. That was reachable here in a way it is
		// not for MemoryBus, because the epoch-reset path REBUILDS the buffer
		// at runtime — a bad size would turn a Redis counter reset into a
		// crash rather than a resync. (MemoryBus's NewWithReplaySize has the
		// same trap for a caller passing 0; left alone as pre-existing and
		// off this bug's path, but it is the same trap.)
		size = DefaultReplayBufferSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &RedisBus{
		client:      client,
		keys:        keys,
		subscribers: make(map[chan Notification]*subscriber),
		replay:      newReplayBuffer(size),
		replaySize:  size,
		ctx:         ctx,
		cancel:      cancel,

		publishHeartbeat:  publishHeartbeat,
		heartbeatInterval: DefaultWatchHeartbeatInterval,
		idleTimeout:       DefaultWatchIdleTimeout,
		heartbeatKick:     make(chan struct{}, 1),
		idleKick:          make(chan struct{}, 1),
	}
	// Eager subscription — see the type comment (2).
	b.pubsub = client.Subscribe(ctx, keys.Name(redisWatchChannelSuffix))

	// WAIT FOR THE SUBSCRIBE TO BE CONFIRMED before returning. go-redis
	// establishes the subscription asynchronously, so without this the
	// constructor hands back a bus that is not yet listening — and Redis
	// pub/sub is at-most-once, so everything published in that window is
	// lost to this instance, silently.
	//
	// Found as a test flake (a second bus constructed and published to
	// immediately received nothing), which is exactly the shape a rolling
	// deploy has: a replica comes up, and traffic reaches it before its
	// subscription is live. The window is small and entirely real.
	//
	// A failure here is logged rather than fatal: the receive loop's
	// ChannelWithSubscriptions() re-subscribes on reconnect, so a bus that
	// missed its first confirmation still recovers — it just cannot promise
	// it was listening from the moment it was constructed.
	//
	// THIS RECEIVE IS LOAD-BEARING FOR receiveMessages, which has no
	// "skip the first confirmation" flag precisely because this consumes the
	// initial one. Removing it makes every bus announce a hole at startup.
	// See that function's comment, and TestNoCoverageIsDroppedAtStartup,
	// which fails if this line goes away.
	subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
	defer subCancel()
	if _, err := b.pubsub.Receive(subCtx); err != nil {
		slog.Warn("watchevents: Redis subscription not confirmed at construction; "+
			"notifications published before it establishes will be missed by this instance",
			"error", err, "channel", keys.Name(redisWatchChannelSuffix))
	}

	subLoopCtx, subLoopCancel := context.WithCancel(ctx)
	b.subCancel = subLoopCancel
	b.lastSeen = b.now()
	b.lastProbeOK = b.now()
	b.wg.Add(1)
	b.subGen++
	go b.receiveMessages(subLoopCtx, b.pubsub, b.subGen)

	// NOT STARTED AT ALL ON PHASE 1 (codex round 2). Both halves are gated on
	// publishHeartbeat and would be guaranteed no-ops there, so the loop would
	// be goroutines and timers per bus for a deployment that asked for none of
	// it — and phase 1 is the DEFAULT. The flag is constructor-only, so the
	// decision is taken once and cannot go stale. The in-function gates stay:
	// those are the correctness ones, and direct callers reach them without a
	// loop.
	//
	// UNTESTED BY DESIGN, and recorded rather than papered over: removing this
	// gate changes no behaviour, because both halves return immediately on
	// phase 1 anyway. What it changes is goroutine and timer count, and the only
	// assertion that separates them is a goroutine census — which is flaky in a
	// package whose other tests start and stop buses concurrently. The
	// mutation matrix says so plainly (starting the loop unconditionally
	// survives), and that survival is the honest reading.
	if publishHeartbeat {
		b.wg.Add(1)
		go b.maintenanceLoop()
	}
	return b
}

// Publish hands the notification to publishScript, which assigns its globally
// ordered ID and publishes it in one atomic step. It does NOT deliver locally;
// the receive path does that for every instance including this one.
//
// FAIL-CLOSED, where internal/events.RedisBus falls back to a local counter
// when its id lookup fails. Two instances falling back at once mint ids from
// independent counters into a SHARED stream, and replayBuffer.since() reasons
// on monotonicity to detect gaps — so a disordered id does not degrade replay,
// it corrupts it silently for every consumer until the buffer rolls over.
// Dropping the notification loses a nudge in a deployment that is already
// degraded; keeping it breaks resume for everyone.
//
// Folding the id assignment INTO the publish also removed the only case where
// that fallback could have applied: there is no longer a window in which an id
// exists but the publish has not happened, so "assign or don't" and "publish
// or don't" are the same decision.
func (b *RedisBus) Publish(n Notification) error {
	if n.Timestamp == 0 {
		n.Timestamp = time.Now().UnixMilli()
	}

	// n.ID is assigned by the script, so it is marshalled zero and filled in
	// by the receiver from the "<epoch>|<id>|" prefix. Serializing before the
	// call is what lets the id assignment and the publish be one atomic step.
	data, err := json.Marshal(n)
	if err != nil {
		slog.Error("watchevents: failed to marshal notification for Redis", "error", err, "kind", n.Kind)
		return fmt.Errorf("watchevents: marshal notification: %w", err)
	}

	// Checked BEFORE the script call, and reported as ErrBusClosed rather
	// than as whatever the cancelled context happens to produce (BUG-2699).
	// Close() cancels b.ctx, so a post-Close publish already failed — but
	// it failed with a context error indistinguishable from a request that
	// raced a real cancellation, and the caller's two outcomes turn on
	// exactly that distinction: closed proves nothing was published, while
	// a transport error does not. Reading b.closed under the lock is what
	// makes the proof available; inferring it from the error text would
	// not.
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrBusClosed
	}

	// A fresh token per logical publish — NOT per attempt, which is the
	// point: go-redis reuses the same arguments on its own retries, so the
	// second run of the script sees the same token and declines.
	dedupeKey := b.keys.Name(redisWatchDedupeSuffix) + uuid.NewString()

	// The candidate epoch is only adopted when none exists (SET NX inside the
	// script), so every publisher can offer one and exactly the first wins.
	if err := publishScript.Run(b.ctx, b.client,
		[]string{b.keys.Name(redisWatchSeqSuffix), b.keys.Name(redisWatchChannelSuffix), dedupeKey, b.keys.Name(redisWatchEpochSuffix)},
		string(data), redisWatchDedupeTTLSeconds, uuid.NewString()).Err(); err != nil {
		// WORDED AS UNCONFIRMED, not as a drop (codex round 3). The earlier
		// text said "dropping notification ... no globally ordered ID was
		// assigned", which contradicts what this error actually means and
		// contradicted the return path four lines down: go-redis retries a
		// command whose reply was lost, so the script may already have run
		// and published. An operator who reads "dropped" and re-sends turns
		// a possible delivery into a duplicate DISPATCH.
		slog.Error("watchevents: publish outcome UNCONFIRMED — the Redis call failed, but a lost reply can mean the notification was published anyway; do not re-send without checking",
			"error", err, "kind", n.Kind, "item_ref", n.ItemRef)
		// Returned as a plain wrapped error, deliberately NOT ErrBusClosed
		// and deliberately not described as a drop to the caller, however
		// the log line above phrases it for an operator. This error means
		// UNCONFIRMED: go-redis retries a command whose reply was lost to a
		// network error, which is the entire reason the script carries a
		// SET-NX dedupe token (codex round 5), so the script may well have
		// run and published while this call still returns non-nil. A caller
		// that re-publishes on this error risks a duplicate DISPATCH, not a
		// repeat of nothing — see Bus.Publish's doc comment.
		return fmt.Errorf("watchevents: redis publish: %w", err)
	}
	return nil
}

// Subscribe returns a channel receiving every future Notification, with no
// replay — same contract as MemoryBus.Subscribe.
func (b *RedisBus) Subscribe() (chan Notification, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub := newSubscriber()
	ch := sub.ch
	if b.closed {
		// A subscriber registered after Close would never be closed by
		// anyone. Hand back an already-closed channel so the consumer's
		// range/select terminates instead of blocking forever.
		close(ch)
		return ch, sub.gaps
	}
	b.subscribers[ch] = sub
	return ch, sub.gaps
}

// SubscribeAndReplaySince atomically registers the subscriber and captures the
// buffered notifications above sinceID, under the SAME lock the local fan-out
// holds.
//
// That shared lock is the whole mechanism, and it is why this type cannot
// borrow events.RedisBus's two-lock layout. For any notification n and any
// call to this method, one of two things is true: n's fan-out completed first,
// so n is in the replay buffer and the channel it would have been sent to did
// not yet exist; or the fan-out begins after, so n arrives on the channel and
// the buffer read has already happened. Never both, never neither — the same
// argument MemoryBus makes, one layer further out, with fan-out standing in
// for Publish because that is where delivery happens here.
//
// The replay is nil whenever this instance cannot vouch for the span —
// eviction, a recorded hole, a cold start, or a resume that outruns the local
// view. The subscription is valid either way; only the replay is unavailable,
// and the caller turns that into sync_required.
//
// The third return is the subscriber's GAP SIGNAL — see Subscribe.
func (b *RedisBus) SubscribeAndReplaySince(ctx context.Context, sinceID int64) (chan Notification, []Notification, <-chan struct{}) {
	// Consulted BEFORE the lock: it sleeps and does network I/O. See its
	// comment for why that ordering is also the only one that preserves the
	// subscribe-and-replay guarantee.
	//
	// ctx IS THE CALLER'S, and that is the whole of BUG-2751. This runs on the
	// request path while the SSE handler holds a global AND a per-user
	// admission slot (BUG-2726), released by defer when the handler returns —
	// so every moment spent here is capacity held for a client that may
	// already be gone.
	// CHECKED BEFORE THE SETTLE PATH IS ENTERED AT ALL (codex round 2). Placed
	// after it, an already-cancelled caller still made the first Redis GET —
	// which fails on the dead context and logs "could not read the sequence
	// counter to validate a resume" at WARN. That line means "Redis is
	// unhealthy" to whoever reads it, and it would have fired on every client
	// that hung up a moment before its resume landed. Ordinary disconnect
	// churn would have looked like Redis trouble.
	if ctx.Err() != nil {
		return b.declineCancelledResume(sinceID)
	}

	forceGap := b.resumeOutrunsLocalView(ctx, sinceID)

	// A CALLER THAT HAS GONE IS NOT REGISTERED (BUG-2751, codex round 1).
	// resumeOutrunsLocalView answers false on cancellation — "no forced gap" —
	// which on its own reads as an ordinary converged resume, so the rest of
	// this function would go on to register a subscriber and build a replay
	// slice for a connection that is unwinding. Ending the wait early and then
	// doing the work anyway is half a fix.
	//
	// The shape returned is the SAME one the closed-bus branch below returns:
	// a closed channel, no replay. That matters at the call site — the handler
	// falls back to plain Subscribe() when it gets a NIL channel, so returning
	// nil here would re-register the very caller this is declining to serve.
	if ctx.Err() != nil {
		return b.declineCancelledResume(sinceID)
	}

	// Registered FIRST so it runs LAST, after the Unlock — reports fire
	// with no bus lock held. See Observer.
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()
	sub := newSubscriber()
	ch := sub.ch
	if b.closed {
		close(ch)
		return ch, nil, sub.gaps
	}
	b.subscribers[ch] = sub
	if b.afterSubscribeRegister != nil {
		b.afterSubscribeRegister()
	}
	if forceGap {
		// Already counted by resumeOutrunsLocalView, which is where that
		// decision is made.
		return ch, nil, sub.gaps
	}

	missed := b.replaySince(sinceID)
	if missed == nil {
		// The LOCAL half of an unservable resume: replaySince answers
		// nil when the cursor falls below what this instance can vouch
		// for — a cold start, or a hole it recorded — and the handler
		// turns that into sync_required exactly as for the
		// shared-counter case. One client resyncing is one unit of the
		// metric's population either way.
		//
		// Counted HERE rather than inside replaySince, which stays a
		// pure read of local state, and on the deferred path so it fires
		// with the lock released.
		pending.resumeGap()
	}
	return ch, missed, sub.gaps
}

// callerIsGone reports whether a Redis error is our own cancellation rather
// than a fault worth telling an operator about.
//
// A FUNCTION OF THE ERROR ALONE, AND THAT IS THE FIX (codex round 4, which
// BLOCKED on the earlier form). The first version asked ctx.Err() instead —
// which cannot distinguish "the caller left" from "Redis failed while the
// caller happened to be leaving", so a genuine failure coinciding with a
// disconnect was downgraded to Debug and disappeared. That is the signal an
// operator most needs, hidden by the change meant to reduce noise.
//
// Written as a predicate rather than inlined so the classification can be
// tested for what it IS: the racing case cannot be staged deterministically
// through a live client — go-redis decides, by timing, whether it returns the
// context error or the server error — so the property is pinned here, where
// timing plays no part.
func callerIsGone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// declineCancelledResume answers a caller that has already gone.
//
// SAME SHAPE AS THE CLOSED-BUS BRANCH — a closed channel, no replay, never nil.
// nil is meaningful at the call site: the SSE handler falls back to plain
// Subscribe() when it gets one, which would re-register the caller this is
// declining.
//
// LOGGED AT DEBUG, AND DELIBERATELY NOT COUNTED (codex round 2). The finding
// was that an operator cannot distinguish disconnect churn from no resume
// activity. True, and the honest fix is not a counter: a client hanging up
// during its own resume is ORDINARY on a mobile network, so a metric for it
// would be a number nobody can act on, sitting next to
// pad_watchevents_resume_gaps_total where it would be read as a fault. The
// condition an operator does act on — capacity held by connections that no
// longer exist — is already visible in the admission counts, and this change is
// what keeps those honest.
func (b *RedisBus) declineCancelledResume(sinceID int64) (chan Notification, []Notification, <-chan struct{}) {
	slog.Debug("watchevents: resume abandoned before it could be served; the caller is gone",
		"since_id", sinceID)
	sub := newSubscriber()
	close(sub.ch)
	return sub.ch, nil, sub.gaps
}

// whicheverEndsFirst returns a context that ends when EITHER input does, and a
// cancel that must be called to release the registration on the second one.
//
// It exists because the settle wait has two independent reasons to be
// abandoned that live in different places: the client going away (BUG-2751)
// and the bus closing. Deriving from one and ignoring the other silently drops
// half the story — internal/events lost the shutdown half exactly that way in
// its own first draft, which is why this is written as a merge rather than a
// swap.
func whicheverEndsFirst(caller, bus context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(caller)
	stop := context.AfterFunc(bus, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// settleWindow is how long resumeOutrunsLocalView waits before deciding that
// ids it has not seen are MISSED rather than merely in flight.
//
// The counter is incremented inside the publish script, and the message then
// has to reach this instance — so a counter running ahead of our high-water
// mark is the NORMAL state for a moment after every publish. That window is
// bounded by propagation time (a local network hop), while a genuinely missed
// message never arrives at all. So the discriminator is TIME, not magnitude:
// wait out the propagation window and re-read. This is the lead's ruling on
// BUG-2651, and it is what avoids inventing an unprincipled "how many ids
// behind is too many" threshold.
const settleWindow = 250 * time.Millisecond

// resumeOutrunsLocalView reports whether a resume from sinceID is asking about
// ids this instance cannot vouch for, by asking the one authority that knows:
// the shared counter.
//
// WHY THIS EXISTS. The local coverage bookkeeping (knownFrom) detects a hole
// once a LATER id arrives to reveal it. It is blind to the trailing case —
// hold 100, miss 101, client resumes from 100 before 102 lands — where the
// buffer simply has nothing above the cursor and "caught up" is
// indistinguishable from "nothing happened". A silently lost nudge is
// unbounded staleness; a spurious resync costs one redundant fetch. That
// asymmetry is why this is worth a network read on a path that only runs on
// reconnect.
//
// It also catches the counter having gone BACKWARDS — a reset this instance
// has not yet observed — because any DISAGREEMENT between the authority and
// our high-water mark means our replay cannot be trusted for this cursor.
//
// Called WITHOUT the mutex held, and deliberately: it sleeps and does network
// I/O, neither of which may happen inside the lock the fan-out needs. It is
// also called BEFORE subscribing rather than between subscribe and replay,
// which would reopen the double-delivery window SubscribeAndReplaySince
// exists to close. Nothing is lost by waiting first: fanOutLocally appends to
// the replay buffer regardless of who is subscribed, so anything arriving
// during the settle beat is picked up by the replay read afterwards.
//
// A failed read answers false — proceed on local knowledge. Turning a Redis
// hiccup into a resync for every reconnecting client would be a worse failure
// than the one this is guarding against.
//
// CHATTINESS BOUND, stated because it is the cost side of the lead's ruling
// (chatty-but-correct beats quiet-but-lossy): the condition is agreement
// between the two reads, so a resume that happens while publishing is
// CONTINUOUS across the whole settle window can find them disagreeing every
// time and resync. That is bounded by this stream being low-volume by design
// (status changes, comments and pushes — the package's own sizing note calls
// out that it is not the per-workspace firehose) and by resumes only
// happening on reconnect. If a workload ever makes this chatty in practice,
// the fix is a longer settle window, not a magnitude threshold.
func (b *RedisBus) resumeOutrunsLocalView(ctx context.Context, sinceID int64) bool {
	if sinceID <= 0 || b.client == nil {
		return false
	}

	// BOUNDED BY THE CALLER *AND* BY THE BUS, not by either alone (BUG-2751).
	//
	// It used to wait on b.ctx only — the bus's lifetime — so a client that
	// disconnected during the settle window held its admission slots for the
	// remainder of it plus two Redis round trips. The connection was gone; the
	// capacity was not.
	//
	// Swapping to the caller's context alone would trade one leak for another:
	// b.ctx is what lets Close cut a wait short, and dropping it would leave a
	// shutdown blocked behind a client that is still connected. Both endings
	// are reasons to stop, so the wait ends on either.
	ctx, cancel := whicheverEndsFirst(ctx, b.ctx)
	defer cancel()

	remote, ok := b.sharedCounter(ctx)
	if !ok {
		return false
	}
	if remote == b.highestSeen() {
		// Agreed as of this instant, so there is nothing to settle for.
		//
		// WHAT THIS DOES NOT COVER, and cannot (codex round 12): a
		// notification published AFTER this read and missed by this instance
		// is invisible to any check made here — and shrinking the window by
		// settling anyway would not close it, since the same race exists in
		// the instant after this function returns. The check's honest scope
		// is "what was missed BEFORE the resume"; a message missed after it
		// is a property of at-most-once pub/sub with no per-connection ack,
		// and the only real answer to that is a durable stream (Redis
		// Streams with consumer groups) rather than a longer wait here.
		return false
	}

	// Give propagation its beat, then re-read BOTH SIDES.
	//
	// Re-reading only the local side was the first version of this and it was
	// wrong in both directions (codex round 11). The captured counter is a
	// SNAPSHOT: if id 101 lands during the beat while 102 is published and
	// missed, comparing against the stale 101 says "converged" and loses 102;
	// and a GET that raced just before a publish can report a value BELOW
	// what we already hold, which then never matches and resyncs a client
	// that missed nothing.
	//
	// Agreement between the authority and this instance is the condition, and
	// it has to be evaluated on two fresh reads or it is not agreement at all.
	if b.beforeSettleWait != nil {
		b.beforeSettleWait()
	}

	timer := time.NewTimer(settleWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// The caller left, or the bus is closing. Either way there is nobody
		// to answer and no reason to hold the slot: returning false means "no
		// forced gap", and the caller is about to unwind anyway.
		return false
	case <-timer.C:
	}

	remote, ok = b.sharedCounter(ctx)
	if !ok {
		return false
	}
	held := b.highestSeen()
	if remote == held {
		return false
	}

	// Any remaining disagreement is a gap, in either direction: still BEHIND
	// means ids exist that never reached us, still AHEAD means the counter
	// was reset under us and our buffer belongs to a dead id space. Both make
	// this instance's replay untrustworthy for this cursor.
	slog.Warn("watchevents: resume cannot be served from this instance's view; reporting a gap",
		"since_id", sinceID, "highest_seen", held, "shared_counter", remote)
	b.reportResumeGap()
	return true
}

// sharedCounter reads the authoritative sequence value. The bool is false when
// it could not be read, which callers treat as "answer from local knowledge" —
// see resumeOutrunsLocalView for why failing closed here would be worse.
func (b *RedisBus) sharedCounter(ctx context.Context) (int64, bool) {
	v, err := b.client.Get(ctx, b.keys.Name(redisWatchSeqSuffix)).Int64()
	switch {
	case err == nil:
		return v, true
	case errors.Is(err, redis.Nil):
		// ABSENT IS A VALUE, NOT A FAILURE, and reading it as the latter was
		// a bug (codex round 12). "No counter" legitimately means zero — on a
		// fresh deployment nothing has been published, so there is nothing to
		// have missed and 0 == 0 agrees with an instance that has seen
		// nothing. But the key can also DISAPPEAR after this bus has seen
		// ids, via FLUSHDB or eviction, and treating that as unreadable meant
		// falling back to local knowledge and cheerfully replaying an id
		// space the authority no longer has — while the next publish starts
		// again at 1.
		//
		// Returning 0-and-readable makes that case fall out of the ordinary
		// comparison: an instance holding 101 disagrees with an authority at
		// 0, does not converge, and the resume is answered with a gap.
		return 0, true
	case callerIsGone(err):
		// OUR OWN CANCELLATION IS NOT A REDIS FAULT (codex round 3). The
		// caller can disconnect while this GET is IN FLIGHT, and the error
		// that comes back is context.Canceled — indistinguishable, at the WARN
		// below, from Redis being unreachable. That line is read as "the
		// sequence counter is unhealthy", so on a stream where clients hang up
		// mid-resume it would manufacture exactly the alarm an operator would
		// chase. The entry-side decline catches a caller that was already
		// gone; this catches one that left while we were asking.
		//
		// CLASSIFIED ON THE ERROR, NOT ON ctx.Err() (codex round 4, which
		// BLOCKED on this). Asking the context instead asks the wrong
		// question: a GENUINE Redis failure that merely coincides with a
		// cancellation would be downgraded to Debug and vanish — precisely the
		// signal an operator needs, hidden by the fix meant to reduce noise.
		// The error itself is the only thing that says which happened.
		slog.Debug("watchevents: resume abandoned while reading the sequence counter; the caller is gone",
			"error", err)
		return 0, false
	default:
		slog.Warn("watchevents: could not read the sequence counter to validate a resume; "+
			"answering from local knowledge only", "error", err)
		return 0, false
	}
}

func (b *RedisBus) highestSeen() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastAppendedID
}

// replaySince is replayBuffer.since plus the hole check. Callers must hold mu.
//
// Returning nil is the same signal eviction already produces, and the SSE
// handler already treats it as sync_required — so a missed notification
// becomes a resync rather than a silent loss, which is the behaviour a
// consumer of MemoryBus would expect and get.
func (b *RedisBus) replaySince(sinceID int64) []Notification {
	// sinceID == 0 is a fresh subscriber asking for everything buffered; it
	// is not resuming from a position, so there is no position to span.
	if sinceID > 0 {
		// knownFrom == 0 means this instance has received NOTHING yet, which
		// is strictly LESS knowledge than "contiguous from X" and must
		// therefore produce at least as strong a signal (codex round 9). The
		// earlier version skipped the check entirely in that state, so an
		// empty buffer answered any cursor with an empty-but-non-nil slice —
		// which the SSE handler reads as "caught up".
		//
		// The scenario is a restart, not an exotic one: replica B comes up
		// while Redis is at 100, id 101 is published before B's subscription
		// is live, and a client reconnects to B with Last-Event-ID 100 before
		// 102 arrives. B says caught-up, then delivers 102, and 101 is gone
		// with nothing to tell anyone.
		if b.knownFrom == 0 || sinceID+1 < b.knownFrom {
			return nil
		}
	}

	// NOTE: everything above reasons about what this instance HAS received,
	// which cannot see a notification missed at the END of the sequence.
	// That half is handled before the lock is taken — see
	// resumeOutrunsLocalView, which the resume path consults.
	return b.replay.since(sinceID)
}

func (b *RedisBus) Unsubscribe(ch chan Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// EventsSince answers from THIS INSTANCE'S LOCAL VIEW ONLY, and deliberately
// does not run resumeOutrunsLocalView's authority check.
//
// The asymmetry with SubscribeAndReplaySince is intentional rather than an
// oversight. The Bus interface already documents EventsSince as the standalone
// primitive "for tests and any future caller that doesn't need the atomic
// subscribe-and-replay guarantee"; the SSE handler — the one caller whose
// answer a user depends on — uses SubscribeAndReplaySince. Teaching this method
// to sleep for a settle window and make a network call would surprise every one
// of those callers for no benefit they asked for.
//
// If a future caller DOES need a trustworthy resume without subscribing, the
// honest move is to give it the check explicitly, not to make this one
// silently expensive.
func (b *RedisBus) EventsSince(sinceID int64) []Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.replaySince(sinceID)
}

// Close stops the receive loop, closes the Redis subscription, and closes every
// subscriber channel.
//
// The `closed` flag exists because this bus, unlike MemoryBus, has a goroutine
// that can be mid-fan-out when Close runs: it takes the same mutex, so the two
// serialize, but a Subscribe racing in afterwards would otherwise register a
// channel nobody will ever close.
func (b *RedisBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
	b.mu.Unlock()

	b.cancel()
	if b.pubsub != nil {
		_ = b.pubsub.Close()
	}
	b.wg.Wait()
}

// receiveMessages is the single consumer of the shared Redis channel. Every
// delivery on this instance — including for notifications this instance itself
// published — comes through here.
// IT USES ChannelWithSubscriptions, AND THE REASON DIFFERS FROM
// internal/events' (BUG-2739).
//
// Against Channel: a plain message channel hides RESUBSCRIPTIONS. go-redis
// reconnects transparently on a dropped connection — a Redis failover, a
// network blip, a server restart — and hands back the messages that arrive
// AFTER it recovers, saying nothing about the ones published while it was
// down. This bus then learns of the hole only when a LATER notification
// arrives with a non-contiguous id, so a flap that loses the newest
// notification on a stream that then goes quiet leaves every connected client
// silently stale, indefinitely.
//
// NO first-confirmation FLAG HERE, and that is the one place a port of
// internal/events' loop would be wrong. That package's receive loop is handed
// a fresh PubSub nobody has read from, so the INITIAL subscribe confirmation
// arrives on its channel and has to be skipped. Ours does not:
// NewRedisBusWithKeys calls b.pubsub.Receive before this goroutine starts, to
// confirm the subscription is live before the constructor returns, and that
// Receive consumes the initial confirmation. Verified rather than assumed —
// with the constructor's Receive in place, this channel delivers zero
// subscriptions at startup. So skipping "the first" would swallow the first
// GENUINE resubscription, which is precisely the fault this loop exists to
// catch, while looking like the package that got it right.
//
// TestNoCoverageIsDroppedAtStartup is the enforcement, not this comment: it
// fails if the constructor's Receive is ever removed.
//
// The one case this over-reports is that Receive having FAILED (its 5s
// timeout warn path) and go-redis subscribing afterwards. That window really
// was uncovered — the constructor logs exactly that — so announcing a hole
// for it is honest.
//
// AND ONE CASE IT UNDER-REPORTS, checked in the library rather than assumed:
// the subscription confirmation goes through the SAME bounded channel as
// messages (go-redis v9.22.0, initAllChan — `case *Subscription, *Message:`,
// chanSize 100, chanSendTimeout 1 minute), so a confirmation can be DROPPED
// under sustained load exactly like a message. Coverage still ends, by the
// other road IN THE USUAL CASE: a full channel means messages are flowing,
// so if the outage lost anything the next message consumed is non-contiguous
// and the gap arm in fanOutLocally raises it. The operator then sees a
// sequence gap rather than a subscription_resumed reset — a less specific
// label for the same truth.
//
// NOT a guarantee, and the exceptions are worth naming rather than rounding
// off. If nothing was published during the outage there is no hole to find,
// which is fine — nothing was lost. If the drops continue through whatever
// would have exposed the hole and the stream then goes quiet, nothing ever
// does: that is BUG-2727's standing boundary, unchanged in both directions
// by this work. A reconnecting client is covered either way, since
// resumeOutrunsLocalView asks the shared counter rather than local state.
// TAKES ITS SUBSCRIPTION AND ITS OWN CONTEXT, rather than reading b.pubsub
// (BUG-2769). An idle cycle replaces the subscription under a running bus, and
// the loop reading the OLD one has to be able to tell "I was replaced" from "the
// client died", because those take different exits — the second logs an ERROR
// and moves a counter documented to mean the instance has gone deaf. Passing
// both in makes the distinction structural: the cycle cancels this ctx before
// it closes the old PubSub, so the replaced loop leaves by the quiet door.
func (b *RedisBus) receiveMessages(ctx context.Context, pubsub *redis.PubSub, gen int64) {
	defer b.wg.Done()
	ch := pubsub.ChannelWithSubscriptions()
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-ch:
			// STAMPED FOR EVERY FRAME, ahead of the type switch and ahead of
			// any decode (BUG-2769). What idle detection measures is whether
			// the SOCKET carries traffic, so a frame that turns out to be
			// undecodable is still proof the route works — and that path
			// continues, so stamping inside the switch would miss it. An
			// unreadable message means coverage is broken, which dropCoverage
			// handles; it does not mean the connection is dead, and cycling it
			// would be the wrong remedy.
			// THE GENERATION TRAVELS TO EACH MUTATION rather than being
			// checked once here (codex round 2). A check in this frame and a
			// mutation inside stampLastSeen / fanOutFromRedis /
			// dropCoverageForGen are two separate lock acquisitions, and a
			// replacement between them is exactly the interleave the fence
			// exists to stop. Each of the three re-checks under the same lock
			// it mutates under.
			if ok {
				b.stampLastSeen(gen)
			}
			if !ok {
				// The subscription's message channel closed. go-redis
				// (v9.22.0) closes it ONLY on pool.ErrClosed — the client
				// or the PubSub was closed — and retries every other
				// receive error indefinitely while a health-check
				// goroutine reconnects on ping failure. So the ordinary
				// cause is our own shutdown, which cancels b.ctx and is
				// caught by the case above; arriving HERE instead means
				// the client went away underneath a bus that is still
				// running, and from this point the instance publishes
				// fine and receives nothing at all — including its own
				// publishes, which come back through Redis like everyone
				// else's. Silence was the original behaviour and made
				// that state indistinguishable from a quiet workspace.
				//
				// UNLESS WE ARE SHUTTING DOWN. Close cancels b.ctx AND
				// closes the pubsub, so both cases of this select can be
				// ready at once and Go picks between ready cases at
				// RANDOM — an ordinary shutdown could then log an ERROR
				// and bump a counter documented to mean "non-zero
				// outside shutdown".
				//
				// DEFENCE, not a fix for observed behaviour, and NO TEST
				// FAILS IF IT IS DELETED: measured, 200 Close cycles
				// under traffic without it produced zero false exits,
				// with Close's ordering reversed as well. Kept because
				// select's randomness is real and this makes the outcome
				// independent of that ordering.
				// ctx covers BOTH endings now: bus shutdown, which cancels
				// b.ctx and is inherited here, and a deliberate replacement by
				// the idle cycle, which cancels this subscription's own ctx
				// first. Either way the closure is ours and not a fault.
				if ctx.Err() != nil {
					return
				}
				slog.Error("watchevents: Redis subscription closed; this instance will receive no further notifications " +
					"(publishes still succeed, so nothing else will report this)")
				b.reportReceiveLoopExited()
				return
			}
			switch msg := raw.(type) {
			case *redis.Subscription:
				if msg.Kind != "subscribe" && msg.Kind != "psubscribe" {
					// Unsubscribe confirmations. DEFENCE, AND NO TEST FAILS
					// IF IT IS DELETED — measured, not assumed, and the
					// reason is doubled: nothing in this bus produces one.
					// It never calls pubsub.Unsubscribe, and PubSub.Close
					// (go-redis v9.22.0) closes the connection without
					// sending UNSUBSCRIBE at all, so no confirmation is
					// emitted on the way out either. Kept because a Kind
					// switch that names only the case it handles is a
					// switch waiting to mis-handle the others, and because
					// the alternative failure — a graceful stop
					// incrementing an operator's failover counter — is
					// silent.
					continue
				}
				// A RESUBSCRIPTION: the connection dropped and came back, and
				// whatever was published in between never reached us. See the
				// function comment for why there is no "skip the first" here.
				slog.Warn("watchevents: pub/sub resubscribed; dropping the replay buffer — "+
					"resumes across the gap will report sync_required",
					"channel", msg.Channel)
				b.dropCoverageForGen(ResetReasonSubscriptionResumed, gen)

			case *redis.Message:
				if isWatchHeartbeat(msg.Payload) {
					// PHASE 1 IS EXACTLY THIS: recognise and ignore. The frame
					// has already done its whole job by arriving — the stamp
					// above is the entire effect. It consumes no id, drops no
					// buffer, reaches no subscriber and moves no counter, so an
					// instance that publishes none is still a correct receiver
					// for one that does. That is what makes the two-phase roll
					// zero-loss.
					continue
				}
				epoch, n, err := decodePayload(msg.Payload)
				if err != nil {
					// A MESSAGE WE CANNOT READ IS A HOLE IN THIS INSTANCE'S
					// COVERAGE. Discarding it and carrying on was the original
					// behaviour and left the buffer claiming a span it no
					// longer had.
					//
					// WHAT WE KNOW is narrower than "a notification was
					// lost": something arrived on our channel that we could
					// not read. It may have been ours, or it may be foreign
					// — another installation sharing this Redis without
					// PAD_REDIS_NAMESPACE (BUG-2724), a probe, an older
					// publisher. Coverage ends because we cannot TELL, which
					// is a claim about our own evidence rather than about the
					// stream.
					//
					// It is NOT enough that this bus's ids are consecutive by
					// construction — one channel, one counter — so IF the
					// message was ours the next notification's id would expose
					// the miss through the gap arm below. That detection needs
					// a LATER notification to arrive, and an unreadable NEWEST
					// message on a stream that then goes quiet is exactly the
					// case with no later one. (internal/events cannot lean on
					// id arithmetic at all, since its per-workspace ids are
					// non-consecutive by construction; it reaches the same
					// drop by a different road.)
					//
					// knownFrom = 0 is the honest value because there is no id
					// to raise it to — we cannot name what we may have missed,
					// which is the same fact from the other side. Recovery is
					// bounded by the next publish, which re-establishes
					// coverage through the cold-start arm.
					slog.Error("watchevents: failed to decode notification from Redis; "+
						"dropping the replay buffer — resumes across it will report sync_required",
						"error", err, "channel", msg.Channel)
					b.dropCoverageForGen(ResetReasonUndecodableMessage, gen)
					continue
				}
				b.fanOutFromRedis(epoch, n, gen)
			}
		}
	}
}

// decodePayload parses the "<epoch>|<id>|<json>" wire form publishScript emits.
//
// Splitting into exactly three parts on the FIRST two separators is what keeps
// a '|' inside the JSON body harmless: the epoch is a uuid and the id is
// digits, so neither can contain one.
//
// The id lives outside the JSON because it is assigned inside the Lua script,
// atomically with the publish (see publishScript). Whatever ID the publisher
// had in the struct is overwritten by the authoritative one — the publisher
// never knows it, since the script assigns it after the marshal.
func decodePayload(payload string) (string, Notification, error) {
	parts := strings.SplitN(payload, "|", 3)
	if len(parts) != 3 {
		return "", Notification{}, fmt.Errorf("payload is not <epoch>|<id>|<json>")
	}
	epoch, idPart, body := parts[0], parts[1], parts[2]
	if epoch == "" {
		return "", Notification{}, fmt.Errorf("payload has an empty epoch prefix")
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return "", Notification{}, fmt.Errorf("payload id prefix %q is not an integer: %w", idPart, err)
	}
	var n Notification
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		return "", Notification{}, fmt.Errorf("payload body is not a Notification: %w", err)
	}
	n.ID = id
	return epoch, n, nil
}

// fanOutLocally appends to the replay buffer and sends to every live local
// subscriber, under the single mutex — see SubscribeAndReplaySince for why
// those two steps may not be separated.
//
// The notification already carries its globally assigned ID from the
// publishing instance; nothing is renumbered here, which is what keeps
// Last-Event-ID meaningful across instances.
// fanOutFromRedis is the receive path's entry point: it applies the EPOCH
// check, then hands off to fanOutLocally for the id-level bookkeeping.
//
// The two are separate because they answer different questions. The epoch asks
// "is this the same id sequence I have been tracking" — a string comparison
// that no arithmetic on ids can substitute for. fanOutLocally then asks "am I
// contiguous within it". Numeric detection alone is blind to a reset that has
// already climbed past our high-water mark (codex round 13), which is exactly
// the case the epoch exists for.
// gen is the subscription this frame arrived on. Checked under the SAME lock
// that mutates, because a check taken in the caller and a mutation taken here
// are two lock acquisitions with a replacement possible in between — which is
// what codex round 2 found wrong with a single fence at the top of the frame
// handler (BUG-2769).
func (b *RedisBus) fanOutFromRedis(epoch string, n Notification, gen int64) {
	// Registered FIRST so it runs LAST — after the Unlock below — because
	// observer callbacks must not run under the bus mutex (codex round 9).
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	if b.subGen != gen {
		// A straggler from a subscription that has already been replaced. It
		// arrived on a socket this instance has stopped believing, so it is not
		// evidence about the id space and must not enter the replacement's
		// buffer.
		b.mu.Unlock()
		return
	}
	if b.epoch == "" {
		b.epoch = epoch
	} else if b.epoch != epoch {
		slog.Warn("watchevents: the notification id space changed; dropping the replay buffer — "+
			"resumes from the previous epoch will report sync_required",
			"previous_epoch", b.epoch, "new_epoch", epoch, "id", n.ID)
		b.epoch = epoch
		b.replay = newReplayBuffer(b.replaySize)
		b.lastAppendedID = 0
		b.knownFrom = 0
		// The new space's ids are not comparable with the old space's high
		// water mark, so it goes with the epoch it belonged to.
		b.highWaterID = 0
		b.epochJustChanged = true
		pending.reset(ResetReasonEpochChange)
		// Every live subscriber's coverage just ended (BUG-2730). Ending it
		// makes the next RESUME honest and does nothing for a client that is
		// still holding the stream open, which is the case this signal
		// exists for.
		b.signalAllLocked()
	}
	b.mu.Unlock()

	b.fanOutLocally(n)
}

func (b *RedisBus) fanOutLocally(n Notification) {
	// Registered FIRST so it runs LAST — after the Unlock below. See
	// Observer: reports fire with no bus lock held, so an observer may
	// call back into the bus without deadlocking it.
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Coverage bookkeeping before the append — see knownFrom's comment.
	switch {
	case b.lastAppendedID == 0 && !b.epochJustChanged && b.highWaterID > 0 && n.ID <= b.highWaterID:
		// A COLD START THAT IS ACTUALLY A COUNTER RESET (codex round 13).
		//
		// lastAppendedID is 0 because dropCoverage zeroed it, not because
		// this instance is new — and the id that arrived is at or below what
		// we held before that drop, which is the counter having gone
		// backwards. Without this arm the reset is indistinguishable from a
		// cold start and counter_backward is never reported, even though the
		// operator is looking at exactly the Redis incident that produces
		// both at once.
		//
		// It reaches the same COVERAGE decision as the arm below —
		// knownFrom = n.ID over an already-empty buffer — so this exists for
		// the report and the signal, not to change what any client is told.
		slog.Warn("watchevents: notification id went backwards across a coverage drop; "+
			"the Redis sequence counter was reset during the outage",
			"high_water", b.highWaterID, "got", n.ID)
		// n.ID + 1, NOT n.ID — see the note on the arm below; the id space
		// restarted, so a cursor at n.ID-1 may belong to the old one.
		b.knownFrom = n.ID + 1
		b.highWaterID = n.ID
		pending.reset(ResetReasonCounterBackward)
		b.signalAllLocked()

	case b.lastAppendedID == 0:
		// Cold start: this instance knows nothing before this id, so a client
		// resuming from the id JUST BELOW it is contiguous with our view and
		// can be served — knownFrom = n.ID admits exactly that cursor.
		//
		// UNLESS we just crossed an epoch, in which case that cursor is
		// ambiguous rather than contiguous: id spaces can overlap, so a
		// client presenting n.ID-1 might be holding the OLD sequence's
		// n.ID-1, which was a different notification entirely. Admitting it
		// would hand them the new epoch's id as though it followed theirs —
		// which is the precise failure the epoch check exists to prevent, so
		// letting it back in one line later would be a poor joke. One higher
		// refuses it while still serving anyone genuinely inside the new
		// space.
		b.knownFrom = n.ID
		if b.epochJustChanged {
			b.knownFrom = n.ID + 1
			b.epochJustChanged = false
		}

	case n.ID <= b.lastAppendedID:
		// THE COUNTER WENT BACKWARDS, which means the id space itself
		// restarted — pad:watchevents_seq evicted under maxmemory, lost to a
		// FLUSHDB, or restored from an older snapshot (codex round 6). Ids
		// are no longer comparable across that boundary, so KEEPING the old
		// entries is what does the damage: the ring would hold 99,100,101
		// alongside a fresh 1,2,3, and a resume from 2 would replay the
		// stale hundreds as though they were newer.
		//
		// Dropping the buffer makes every resume from the old space answer
		// nil instead — replayBuffer.since() returns nil once sinceID
		// exceeds the newest id it holds, which after the reset is a small
		// number — so those clients resync, which is the only honest
		// outcome. Clients in the NEW space keep working immediately.
		//
		// RESIDUAL WINDOW, accepted (codex round 8). This fires when the
		// first post-reset notification ARRIVES, so between the counter
		// resetting and the next publish, this instance still holds and will
		// still replay the old ids. A client reconnecting inside that window
		// with a low Last-Event-ID gets buffered entries it has already
		// seen. Nothing here can detect the reset earlier: the counter lives
		// in Redis and this instance only learns about it by receiving
		// something.
		//
		// CLOSED, as of the trailing-gap work: resumeOutrunsLocalView reads
		// the shared counter on every resume, and a value BELOW our
		// high-water mark is exactly this reset seen from the other side. So
		// a client reconnecting inside this window is told to resync rather
		// than handed stale ids. This arm still matters — it is what repairs
		// the instance's own state when the first post-reset message
		// arrives, and it is the only thing that fixes a bus with no
		// reconnecting clients at all.
		slog.Warn("watchevents: notification id went backwards; the Redis sequence counter was reset. "+
			"Dropping the replay buffer — resumes from the previous id space will report sync_required",
			"previous", b.lastAppendedID, "got", n.ID)
		b.replay = newReplayBuffer(b.replaySize)
		// n.ID + 1, NOT n.ID (BUG-2739, codex round 21). The id space
		// RESTARTED, so the two spaces overlap and a client presenting
		// n.ID-1 may be holding the OLD sequence's n.ID-1 — a different
		// notification entirely. knownFrom = n.ID admitted exactly that
		// cursor and replayed the new space's ids as though they followed
		// it, which is the corruption this arm exists to prevent, reached
		// one line later.
		//
		// This is the SAME reasoning the cold-start arm applies after an
		// epoch change, and for the same reason: an id space changed under
		// us. The epoch arm had it and this one did not, which was a gap
		// rather than a distinction — a counter evicted WITHOUT an epoch
		// rotation is precisely the case the epoch cannot see, so it is the
		// case that needed the guard most.
		//
		// The cost is one refused resume for a client genuinely at n.ID-1 of
		// the NEW space, which it could only hold by having been served by
		// another instance — the conservative direction, same trade the
		// epoch arm already accepts.
		//
		// WHAT THIS DOES NOT CLOSE, because arithmetic on ids cannot
		// (BUG-2743 tracks the complete fix). replaySince serves any cursor
		// at or above knownFrom-1, so this admits n.ID itself — and if the
		// OLD space also reached n.ID, that cursor is still ambiguous. The
		// same is true of every old-space id up to the old high water mark,
		// and of the epoch arm's identical +1. Closing it needs a boundary
		// that remembers the OLD space's extent (refuse everything at or
		// below it until the new space climbs past), not a larger constant
		// here. That is a resume-semantics change touching the epoch path
		// too, so it is its own unit.
		//
		// This arm is mitigation, and the epoch is the actual answer: an
		// opaque token is the only thing that can say "different sequence"
		// when the numbers cannot. See redisWatchEpochSuffix.
		b.knownFrom = n.ID + 1
		// The high water mark REBASES onto the new space here, and must:
		// leaving it at the old space's peak would make every id of the
		// restarted sequence look backward to the cold-start arm after any
		// later dropCoverage, turning one reset into a run of false ones.
		b.highWaterID = n.ID
		pending.reset(ResetReasonCounterBackward)
		b.signalAllLocked()

	case n.ID != b.lastAppendedID+1:
		slog.Warn("watchevents: gap in the received notification sequence; resumes across it will report sync_required",
			"expected", b.lastAppendedID+1, "got", n.ID,
			"missed", n.ID-b.lastAppendedID-1)
		b.knownFrom = n.ID
		pending.gap(n.ID - b.lastAppendedID - 1)
		// THE HOLE IS ANNOUNCED TO EVERY SUBSCRIBER ON THIS INSTANCE
		// (BUG-2730). Raising knownFrom above already made a RECONNECT
		// honest — replaySince refuses a cursor below it and the stream
		// answers sync_required. A client holding the stream OPEN across the
		// same hole was told nothing: it stayed connected, kept receiving
		// everything after the gap, and never saw what went missing, while
		// this instance had logged the exact id range.
		//
		// PER-INSTANCE, and that is the whole scope. The missing ids never
		// arrived HERE; other instances may have received them fine, so
		// announcing wider would charge a resync to clients whose stream is
		// intact. And every subscriber registered at this moment is exactly
		// the set that was connected across the hole — anyone subscribing
		// after this line missed nothing they were promised, and is
		// correctly not in the set.
		b.signalAllLocked()
	}
	b.lastAppendedID = n.ID
	if n.ID > b.highWaterID {
		b.highWaterID = n.ID
	}

	b.replay.append(n)

	for _, sub := range b.subscribers {
		select {
		case sub.ch <- n:
		default:
			slog.Warn("watchevents: dropping notification for slow subscriber",
				"kind", n.Kind, "item_ref", n.ItemRef)
			// This one only: a full channel is a fact about one connection's
			// read rate, and every other subscriber received the
			// notification.
			sub.signalGap()
			pending.drop(DropReasonSlowSubscriber)
		}
	}
}

// dropCoverage ends this instance's coverage of the watch stream, because
// something happened that its replay buffer cannot account for: a pub/sub
// resubscription, or a message it could not read (BUG-2739). The next resume
// answers nil until the following notification re-establishes coverage
// through fanOutLocally's cold-start arm.
//
// WHOLE-BUS, unlike internal/events' per-workspace dropWorkspaceCoverage, and
// not by choice: this package keeps ONE replay buffer because there is exactly
// one logical watch stream (see redisWatchChannelSuffix). There is no narrower
// thing to drop.
//
// ALL THREE FIELDS RESET TOGETHER, and that is the trap. Dropping the buffer
// and clearing knownFrom while leaving lastAppendedID at its pre-outage value
// makes the NEXT notification look contiguous — no arm of fanOutLocally's
// switch fires, so knownFrom is never re-established, stays 0, and
// replaySince refuses EVERY resume on this instance from then on. It reads
// correct and bricks resumes permanently.
// TestCoverageIsReestablishedByTheNextNotification is what holds this, and it
// is why the recovery case was written before the refusal case.
//
// highWaterID deliberately SURVIVES, because backward-counter detection needs
// a mark that a coverage drop does not erase — see its field comment.
//
// epochJustChanged is deliberately NOT set: these conditions are a hole in our
// view of the SAME id space, so the cold-start arm's ordinary knownFrom = n.ID
// is correct. The +1 exists only for the ambiguity between two different id
// spaces, where a client's cursor might belong to the old one.
//
// WHY NOT ASK THE SHARED COUNTER FIRST, which would tell us whether anything
// was actually published during the outage and spare the buffer when nothing
// was? Because the answer is unavailable exactly when it matters. A
// resubscription means the Redis connection just failed; a counter read on
// that path either fails (resumeOutrunsLocalView answers FALSE on a failed
// read, by design, so the caller proceeds on local knowledge) or blocks the
// single receive goroutine on network I/O, stalling delivery for every
// subscriber on the instance. Ending coverage locally is the record that
// survives Redis being unreadable, which is the whole point of keeping one.
//
// The cost of that choice, and the reason we drop at all when the resume path
// and the gap arm each catch a real loss independently, are stated in
// docs/deployment.md under "What a failover now COSTS" — they are operator
// decisions and belong where operators read. The short version: both of those
// other detections need something to HAPPEN (a reachable Redis, or a later
// publish), and the live subscriber on a stream that then goes quiet has
// neither. That client is what this function exists for.
// dropCoverageForGen is dropCoverage for a caller that learned of the problem
// from a specific subscription. A frame from a replaced one says nothing about
// the replacement's coverage, and ending it would resync every client on the
// instance because a dead socket's buffered tail arrived late.
func (b *RedisBus) dropCoverageForGen(reason string, gen int64) {
	b.mu.Lock()
	current := b.subGen == gen
	b.mu.Unlock()
	if !current {
		return
	}
	b.dropCoverage(reason)
}

func (b *RedisBus) dropCoverage(reason string) {
	// Registered FIRST so it runs LAST, after the Unlock — reports fire with
	// no bus lock held. See Observer.
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()

	b.replay = newReplayBuffer(b.replaySize)
	b.lastAppendedID = 0
	// DEFENCE IN DEPTH, AND NO TEST FAILS IF IT IS DELETED — said out loud
	// rather than left for the next person to discover by mutation, which is
	// how it was found here. With the buffer emptied on the line above,
	// replayBuffer.since already answers nil to any sinceID > 0 (its
	// count == 0 guard), and the next notification takes the cold-start arm
	// and overwrites knownFrom anyway. So this line is unobservable today.
	//
	// Kept because without it dropCoverage's correctness would rest on
	// another type's internal guard, in another file, for a reason unrelated
	// to coverage — and because "we cover nothing" is the statement this
	// function exists to make, in the three fields that say it.
	b.knownFrom = 0
	pending.reset(reason)
	// Every live subscriber's coverage just ended (BUG-2730). Ending it makes
	// the next RESUME honest and does nothing for a client still holding the
	// stream open — which, for a flap that loses the newest notification on a
	// stream that then goes quiet, is the only client there is.
	b.signalAllLocked()
}

// signalAllLocked raises the gap flag for every live subscriber on this
// instance — the per-INSTANCE half of BUG-2730, for the conditions where the
// hole belongs to the bus rather than to one slow reader. Callers must hold
// b.mu; every send is non-blocking, so holding it cannot deadlock.
func (b *RedisBus) signalAllLocked() {
	for _, sub := range b.subscribers {
		sub.signalGap()
	}
}
