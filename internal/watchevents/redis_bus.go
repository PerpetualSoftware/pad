package watchevents

import (
	"context"
	"encoding/json"
	"log/slog"
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
// latency and buys exactly-once delivery: a local send plus the echo back from
// Redis would deliver twice to the publishing instance's own streams.
type RedisBus struct {
	client *redis.Client

	// mu guards subscribers AND replay together — see the type comment (3)
	// and SubscribeAndReplaySince.
	mu          sync.Mutex
	subscribers map[chan Notification]struct{}
	replay      *replayBuffer
	closed      bool

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

// Publish assigns a globally ordered ID via Redis INCR and publishes to the
// shared channel. It does NOT deliver locally; the receive path does that for
// every instance including this one.
//
// FAIL-CLOSED ON INCR FAILURE, and this is the one place this implementation
// refuses to follow internal/events.RedisBus, which falls back to a local
// counter. Two instances falling back at once mint ids from independent
// counters into a SHARED stream, and replayBuffer.since() reasons on
// monotonicity to detect gaps — so a disordered id does not degrade replay, it
// corrupts it silently for every consumer until the buffer rolls over. Note
// also what the fallback would actually buy: INCR and PUBLISH travel the same
// connection, so an INCR failure almost always precedes a PUBLISH failure, and
// the fallback mostly lets a doomed publish proceed carrying a poisoned id.
// Dropping the notification loses a nudge in a deployment that is already
// degraded; keeping it breaks resume for everyone.
func (b *RedisBus) Publish(n Notification) {
	if n.Timestamp == 0 {
		n.Timestamp = time.Now().UnixMilli()
	}

	id, err := b.client.Incr(b.ctx, redisWatchSeqKey).Result()
	if err != nil {
		slog.Error("watchevents: dropping notification — Redis INCR failed, so no globally ordered ID is available",
			"error", err, "kind", n.Kind, "item_ref", n.ItemRef)
		return
	}
	n.ID = id

	data, err := json.Marshal(n)
	if err != nil {
		slog.Error("watchevents: failed to marshal notification for Redis", "error", err, "kind", n.Kind)
		return
	}
	if err := b.client.Publish(b.ctx, redisWatchChannel, data).Err(); err != nil {
		slog.Error("watchevents: failed to publish notification to Redis",
			"error", err, "channel", redisWatchChannel, "kind", n.Kind, "item_ref", n.ItemRef)
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
	return ch, b.replay.since(sinceID)
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
	return b.replay.since(sinceID)
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
			var n Notification
			if err := json.Unmarshal([]byte(msg.Payload), &n); err != nil {
				slog.Error("watchevents: failed to unmarshal notification from Redis",
					"error", err, "channel", msg.Channel)
				continue
			}
			b.fanOutLocally(n)
		}
	}
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
