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
	return NewRedisBusWithKeys(client, size, redisns.Default)
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
func NewRedisBusWithKeys(client *redis.Client, size int, keys redisns.Keys) *RedisBus {
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
	// Channel() re-subscribes on reconnect, so a bus that missed its first
	// confirmation still recovers — it just cannot promise it was listening
	// from the moment it was constructed.
	subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
	defer subCancel()
	if _, err := b.pubsub.Receive(subCtx); err != nil {
		slog.Warn("watchevents: Redis subscription not confirmed at construction; "+
			"notifications published before it establishes will be missed by this instance",
			"error", err, "channel", keys.Name(redisWatchChannelSuffix))
	}

	b.wg.Add(1)
	go b.receiveMessages()
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
func (b *RedisBus) SubscribeAndReplaySince(sinceID int64) (chan Notification, []Notification, <-chan struct{}) {
	// Consulted BEFORE the lock: it sleeps and does network I/O. See its
	// comment for why that ordering is also the only one that preserves the
	// subscribe-and-replay guarantee.
	forceGap := b.resumeOutrunsLocalView(sinceID)

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
func (b *RedisBus) resumeOutrunsLocalView(sinceID int64) bool {
	if sinceID <= 0 || b.client == nil {
		return false
	}

	remote, ok := b.sharedCounter()
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
	select {
	case <-b.ctx.Done():
		return false
	case <-time.After(settleWindow):
	}

	remote, ok = b.sharedCounter()
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
func (b *RedisBus) sharedCounter() (int64, bool) {
	v, err := b.client.Get(b.ctx, b.keys.Name(redisWatchSeqSuffix)).Int64()
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
func (b *RedisBus) receiveMessages() {
	defer b.wg.Done()
	ch := b.pubsub.Channel()
	for {
		select {
		case <-b.ctx.Done():
			return
		case msg, ok := <-ch:
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
				if b.ctx.Err() != nil {
					return
				}
				slog.Error("watchevents: Redis subscription closed; this instance will receive no further notifications " +
					"(publishes still succeed, so nothing else will report this)")
				b.reportReceiveLoopExited()
				return
			}
			epoch, n, err := decodePayload(msg.Payload)
			if err != nil {
				slog.Error("watchevents: failed to decode notification from Redis",
					"error", err, "channel", msg.Channel)
				continue
			}
			b.fanOutFromRedis(epoch, n)
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
func (b *RedisBus) fanOutFromRedis(epoch string, n Notification) {
	// Registered FIRST so it runs LAST — after the Unlock below — because
	// observer callbacks must not run under the bus mutex (codex round 9).
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
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
		b.knownFrom = n.ID
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

// signalAllLocked raises the gap flag for every live subscriber on this
// instance — the per-INSTANCE half of BUG-2730, for the conditions where the
// hole belongs to the bus rather than to one slow reader. Callers must hold
// b.mu; every send is non-blocking, so holding it cannot deadlock.
func (b *RedisBus) signalAllLocked() {
	for _, sub := range b.subscribers {
		sub.signalGap()
	}
}
