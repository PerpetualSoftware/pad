package collab

import (
	"log/slog"
	"sync"
	"time"
)

// subscriber wraps a delivery channel with the item filter it cares
// about. We map *chan to *subscriber (rather than chan→itemID) so
// MemoryOpBus.Unsubscribe is O(1) and so future enhancements (per-sub
// metrics, last-activity timestamps) have a place to live.
type memSubscriber struct {
	ch     chan OpEvent
	itemID string
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

// Subscribe registers a subscriber for itemID and returns a buffered
// channel. Buffer size matches internal/events.MemoryBus (64) — tuned
// against an SSE workload but suitable for collab too: even a chatty
// editor produces ops at human-keystroke pace, so 64 events of
// headroom comfortably covers any momentary write-stall on the
// receiving WebSocket without pushing memory pressure into the bus.
func (b *MemoryOpBus) Subscribe(itemID string) chan OpEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan OpEvent, 64)
	b.subscribers[ch] = &memSubscriber{
		ch:     ch,
		itemID: itemID,
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
// The bus deliberately does NOT take corrective action itself: it
// has no concept of WHICH peer owns which channel and no way to
// signal a force-close. That's the room manager's domain.
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
		if sub.itemID != event.ItemID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			slog.Warn(
				"collab: dropping op for slow subscriber",
				"type", event.Type,
				"item_id", event.ItemID,
				"client_id", event.ClientID,
			)
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
