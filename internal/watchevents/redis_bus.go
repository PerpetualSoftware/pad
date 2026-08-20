package watchevents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// redisWatchChannel is the single Redis pub/sub channel every instance
	// publishes to and subscribes to.
	//
	// SINGLE, not per-workspace, unlike internal/events' pad:events:<wsID>.
	// That is not a simplification of the template — it is this package's
	// contract: there is exactly one logical watch stream and ALL per-caller
	// filtering happens in the consumer (DOC-2479 DR-2, "no firehose, no
	// wildcard subscriptions"). Subscribe() takes no workspace precisely
	// because a subscriber is not scoped to one, so there is nothing to key a
	// channel by.
	redisWatchChannel = "pad:watchevents"

	// redisWatchSeqKey is the shared counter behind Notification.ID.
	//
	// Deliberately distinct from internal/events' pad:event_seq: the two
	// buses carry different streams with independent Last-Event-ID spaces,
	// and sharing a counter would make each one's ids jump unpredictably
	// whenever the other published — harmless for ordering, but it would
	// turn every replay-gap diagnosis into a question about the other bus.
	redisWatchSeqKey = "pad:watchevents_seq"

	// DEPLOYMENT SCOPING, or rather the lack of it (codex round 3). Both
	// names above are fixed, so two Pad installations pointed at the SAME
	// Redis endpoint cross-feed each other's notifications and share one id
	// counter — and selecting different logical DBs does not help, because
	// Redis pub/sub is not namespaced by DB at all.
	//
	// Left unscoped deliberately: internal/events has used flat `pad:events:`
	// / `pad:event_seq` names since it shipped, and giving one of the two
	// buses a prefix the other lacks would make the operational rule harder
	// to state, not easier. The rule for now is the simple one — one Redis
	// endpoint per Pad installation — and if that ever needs relaxing it
	// should be relaxed for both buses at once, from shared config, rather
	// than growing here first.
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
// The id is prepended to the payload as "<id>|<json>" rather than being
// injected into the JSON, because string-editing JSON inside Lua is a fragile
// way to save one split. The id is digits and the separator is the FIRST '|',
// so a '|' anywhere in the payload is unambiguous.
var publishScript = redis.NewScript(`
local id = redis.call('INCR', KEYS[1])
redis.call('PUBLISH', KEYS[2], id .. '|' .. ARGV[1])
return id
`)

// RedisBus is the multi-instance implementation of Bus (BUG-2651). Every
// instance publishes to one shared Redis channel and receives from it, so a
// Notification produced on instance A reaches a stream held open on instance B
// — the gap MemoryBus's package doc has flagged since this package existed.
//
// THREE THINGS DIFFER FROM internal/events.RedisBus, which is otherwise the
// template. Each is deliberate, and each would look like a mistake to someone
// diffing the two:
//
//  1. One channel and one replay buffer (see redisWatchChannel).
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
	client *redis.Client

	// mu guards subscribers AND replay together — see the type comment (3)
	// and SubscribeAndReplaySince.
	mu          sync.Mutex
	subscribers map[chan Notification]struct{}
	replay      *replayBuffer
	closed      bool

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
	ctx, cancel := context.WithCancel(context.Background())
	b := &RedisBus{
		client:      client,
		subscribers: make(map[chan Notification]struct{}),
		replay:      newReplayBuffer(size),
		ctx:         ctx,
		cancel:      cancel,
	}
	// Eager subscription — see the type comment (2).
	b.pubsub = client.Subscribe(ctx, redisWatchChannel)
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
func (b *RedisBus) Publish(n Notification) {
	if n.Timestamp == 0 {
		n.Timestamp = time.Now().UnixMilli()
	}

	// n.ID is assigned by the script, so it is marshalled zero and filled in
	// by the receiver from the "<id>|" prefix. Serializing before the call is
	// what lets the id assignment and the publish be one atomic step.
	data, err := json.Marshal(n)
	if err != nil {
		slog.Error("watchevents: failed to marshal notification for Redis", "error", err, "kind", n.Kind)
		return
	}

	if err := publishScript.Run(b.ctx, b.client,
		[]string{redisWatchSeqKey, redisWatchChannel}, string(data)).Err(); err != nil {
		slog.Error("watchevents: dropping notification — Redis publish failed, so no globally ordered ID was assigned",
			"error", err, "kind", n.Kind, "item_ref", n.ItemRef)
	}
}

// Subscribe returns a channel receiving every future Notification, with no
// replay — same contract as MemoryBus.Subscribe.
func (b *RedisBus) Subscribe() chan Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Notification, 64)
	if b.closed {
		// A subscriber registered after Close would never be closed by
		// anyone. Hand back an already-closed channel so the consumer's
		// range/select terminates instead of blocking forever.
		close(ch)
		return ch
	}
	b.subscribers[ch] = struct{}{}
	return ch
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
// Returns (ch, nil) when sinceID has been evicted; the subscription is valid,
// only the replay is unavailable, and the caller should treat it like the SSE
// handler's sync_required signal.
func (b *RedisBus) SubscribeAndReplaySince(sinceID int64) (chan Notification, []Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Notification, 64)
	if b.closed {
		close(ch)
		return ch, nil
	}
	b.subscribers[ch] = struct{}{}
	return ch, b.replaySince(sinceID)
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
	if sinceID > 0 && b.knownFrom > 0 && sinceID+1 < b.knownFrom {
		return nil
	}
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
				return
			}
			n, err := decodePayload(msg.Payload)
			if err != nil {
				slog.Error("watchevents: failed to decode notification from Redis",
					"error", err, "channel", msg.Channel)
				continue
			}
			b.fanOutLocally(n)
		}
	}
}

// decodePayload parses the "<id>|<json>" wire form publishScript emits.
//
// The id lives outside the JSON because it is assigned inside the Lua script,
// atomically with the publish (see publishScript). Whatever ID the publisher
// had in the struct is overwritten by the authoritative one — the publisher
// never knows it, since the script assigns it after the marshal.
func decodePayload(payload string) (Notification, error) {
	sep := strings.IndexByte(payload, '|')
	if sep < 0 {
		return Notification{}, fmt.Errorf("payload has no id prefix")
	}
	id, err := strconv.ParseInt(payload[:sep], 10, 64)
	if err != nil {
		return Notification{}, fmt.Errorf("payload id prefix %q is not an integer: %w", payload[:sep], err)
	}
	var n Notification
	if err := json.Unmarshal([]byte(payload[sep+1:]), &n); err != nil {
		return Notification{}, fmt.Errorf("payload body is not a Notification: %w", err)
	}
	n.ID = id
	return n, nil
}

// fanOutLocally appends to the replay buffer and sends to every live local
// subscriber, under the single mutex — see SubscribeAndReplaySince for why
// those two steps may not be separated.
//
// The notification already carries its globally assigned ID from the
// publishing instance; nothing is renumbered here, which is what keeps
// Last-Event-ID meaningful across instances.
func (b *RedisBus) fanOutLocally(n Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Coverage bookkeeping before the append — see knownFrom's comment.
	switch {
	case b.lastAppendedID == 0:
		// Cold start: this instance knows nothing before this id.
		b.knownFrom = n.ID
	case n.ID != b.lastAppendedID+1:
		slog.Warn("watchevents: gap in the received notification sequence; resumes across it will report sync_required",
			"expected", b.lastAppendedID+1, "got", n.ID)
		b.knownFrom = n.ID
	}
	if n.ID > b.lastAppendedID {
		b.lastAppendedID = n.ID
	}

	b.replay.append(n)

	for ch := range b.subscribers {
		select {
		case ch <- n:
		default:
			slog.Warn("watchevents: dropping notification for slow subscriber",
				"kind", n.Kind, "item_ref", n.ItemRef)
		}
	}
}
