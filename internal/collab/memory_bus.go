package collab

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// subscriber wraps a delivery channel with the item filter it cares
// about. We map *chan to *subscriber (rather than chan→itemID) so
// MemoryOpBus.Unsubscribe is O(1) and so future enhancements (per-sub
// metrics, last-activity timestamps) have a place to live.
type memSubscriber struct {
	ch     chan OpEvent
	itemID string
	// overflowed is set by the first drop of an event this subscriber
	// needs (anything but awareness), TASK-1273. From then on Publish skips
	// it entirely: an event delivered AFTER the gap would carry an op-log
	// cursor past the dropped row, and the peer's reconnect would resume
	// from there and never replay it. So its writeLoop only drains what was
	// queued before the gap, and onOverflow has the room close the socket.
	overflowed atomic.Bool
	onOverflow func()
}

// MemoryOpBus is the in-process OpBus implementation used in every
// shipping target today (single-binary self-hosted, single-replica
// pad-cloud). Mirrors the shape of internal/events.MemoryBus so a
// future RedisOpBus is a drop-in: same Subscribe/Publish/Close
// surface, same slow-subscriber drop semantics, same cleanup order.
type MemoryOpBus struct {
	mu          sync.RWMutex
	subscribers map[chan OpEvent]*memSubscriber
}

// NewMemoryOpBus returns a ready-to-use in-process bus.
func NewMemoryOpBus() *MemoryOpBus {
	return &MemoryOpBus{
		subscribers: make(map[chan OpEvent]*memSubscriber),
	}
}

// subscriberBufSize is the per-subscriber bus channel buffer. Sized
// generously so a slow drain (replay holding the conn's writeMu, OS
// write buffer momentarily backed up, …) doesn't spill over into the
// drop path under normal editor load.
//
// Sizing rationale: a 1k-row op-log replay at ~1ms/row takes ~1
// second; during that window a chatty 5-peer room produces O(50)
// live events, so 256 events covers a 5× safety margin. Larger
// documents (10k+ rows) under sustained write load can still
// overflow — that's the documented "force-close slow peers" path
// in the bus's Publish doc comment, deferred to a follow-up task.
const subscriberBufSize = 256

// Subscribe registers a subscriber for itemID and returns a buffered
// channel of size subscriberBufSize.
func (b *MemoryOpBus) Subscribe(itemID string) chan OpEvent {
	return b.SubscribeWithOverflow(itemID, nil)
}

// SubscribeWithOverflow is Subscribe with the overflow signal (TASK-1273):
// onOverflow runs once, on its own goroutine, the first time an event this
// subscriber needs is dropped because its buffer is full. The subscriber
// receives nothing after that drop.
func (b *MemoryOpBus) SubscribeWithOverflow(itemID string, onOverflow func()) chan OpEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan OpEvent, subscriberBufSize)
	b.subscribers[ch] = &memSubscriber{
		ch:         ch,
		itemID:     itemID,
		onOverflow: onOverflow,
	}
	return ch
}

// Unsubscribe removes a subscriber and closes its channel. No-op when
// the channel is unknown — a double-Unsubscribe (e.g. handler defer
// firing after Close has already cleaned up) is safe.
func (b *MemoryOpBus) Unsubscribe(ch chan OpEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// Publish fans an event out to every subscriber whose itemID matches.
//
// Non-blocking: a slow subscriber whose buffer is full has new events
// dropped (logged at warn level so operators can spot a stuck client)
// rather than back-pressuring the broadcast loop. One stuck consumer
// must NEVER poison every other peer in the same room.
//
// Recovery contract for dropped sync events:
//
// The room manager (TASK-1255) is the bus's only consumer in
// production. It reads from each subscriber channel and writes to the
// owning peer's WebSocket. A full channel means that peer's
// WebSocket write is backed up — slow network, blocked client,
// half-broken socket, etc. The room manager is responsible for
// detecting that condition (e.g. via the channel-len threshold
// exposed by the room health check, TASK-1255 + TASK-1256) and
// force-closing the slow peer's WebSocket. A fresh reconnect then
// replays everything the peer missed by loading op-log rows since
// the peer's last known id (TASK-1252 + Yjs state-vector
// negotiation). Awareness drops are unrecoverable, but presence is
// ephemeral so that's fine — sync drops are the only case that
// matters for correctness, and they're recoverable via the
// op-log + reconnect path.
//
// The bus does not close anything itself: it has no concept of which
// peer owns which channel. Since TASK-1273 it signals the overflow
// (SubscribeWithOverflow) and stops delivering to that subscriber, so no
// event after the gap can advance the peer's cursor past it; the room
// manager closes the peer's WebSocket from the signal.
//
// Mutation contract for OpEvent.Data:
//
// Data is cloned at the publish boundary so subscribers (and any
// other publisher) cannot affect each other through a shared backing
// array. Subscribers MUST treat their received Data as read-only —
// mutating one subscriber's view would still mutate every other
// subscriber's, since they share the same clone. Cloning per
// subscriber would push that cost onto every fan-out; the chosen
// trade-off is "one allocation per Publish, immutability by
// convention on the read side".
func (b *MemoryOpBus) Publish(event OpEvent) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	// Clone Data so a publisher's later buffer reuse cannot mutate
	// what subscribers observe. One allocation per Publish is cheaper
	// than a corrupted Yjs decode somewhere downstream — and the
	// gorilla/websocket ReadMessage caller IS allowed to reuse its
	// read buffer between messages, so we must not assume the input
	// slice is owned by us.
	if event.Data != nil {
		cloned := make([]byte, len(event.Data))
		copy(cloned, event.Data)
		event.Data = cloned
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		if sub.itemID != event.ItemID || sub.overflowed.Load() {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			if event.Type == OpTypeAwareness {
				// Presence is ephemeral: the next awareness update replaces
				// it, so a dropped one costs nothing worth a reconnect.
				slog.Warn(
					"collab: dropping awareness for slow subscriber",
					"item_id", event.ItemID,
					"client_id", event.ClientID,
				)
				continue
			}
			if sub.overflowed.CompareAndSwap(false, true) {
				slog.Warn(
					"collab: slow subscriber dropped an op; closing it so its reconnect replays from the op-log",
					"type", event.Type,
					"item_id", event.ItemID,
					"client_id", event.ClientID,
				)
				if sub.onOverflow != nil {
					go sub.onOverflow()
				}
			}
		}
	}
}

// SubscriberCount returns the number of active subscribers whose
// itemID matches. Used by the room manager to decide when the active
// peer count has dropped to zero (room enters its 60s grace before
// teardown) and when it climbs from zero (room comes back live).
func (b *MemoryOpBus) SubscriberCount(itemID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	n := 0
	for _, sub := range b.subscribers {
		if sub.itemID == itemID {
			n++
		}
	}
	return n
}

// Close shuts down the bus. All subscriber channels are closed under
// the same write-lock that gates Publish/Subscribe so a final inflight
// Publish can't race a Close into delivering on a closed channel.
//
// After Close returns, the bus must not be used. Subscribe will leak
// goroutines blocked on the closed channel; Publish becomes a no-op
// over an empty subscriber map but is otherwise undefined.
func (b *MemoryOpBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// Compile-time assertion that MemoryOpBus satisfies OpBus. Catches a
// drifted interface signature at build time rather than at the first
// caller site.
var _ OpBus = (*MemoryOpBus)(nil)
