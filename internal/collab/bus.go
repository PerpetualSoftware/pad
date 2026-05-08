// Package collab provides the server-side substrate for real-time
// collaborative editing of item content via Yjs (PLAN-1248).
//
// The package centers on a "dumb relay" model: the server never
// understands the Y.Doc structure or applies CRDT operations on its
// own — it only persists raw binary updates (in the op-log added by
// TASK-1252) and broadcasts them to peers connected to the same item.
// All CRDT logic lives in the browser via @tiptap/y-tiptap.
//
// Wiring:
//
//	WebSocket handler (TASK-1254)  ─┐
//	                                │  Publish(OpEvent)
//	                                ▼
//	                            ┌──────┐
//	                            │ OpBus│ ← in-process MemoryOpBus
//	                            └──────┘   (RedisOpBus is a future
//	                                ▲       drop-in for multi-replica
//	                                │       deploys, deferred IDEA)
//	                  Subscribe(itemID)
//	                                │
//	WebSocket handler (other peer)  ┘
//
// OpBus only handles the broadcast leg. Persistence (op-log append,
// snapshot rebuild) lives in the room manager (TASK-1255) so the bus
// stays focused on fan-out.
package collab

// OpEvent is a single message broadcast across an item's collab room.
//
// Fields:
//   - ItemID    target item; the bus filters subscribers by this.
//   - ClientID  Yjs client id of the originating peer. The dumb relay
//     does not interpret it, but the designated-applier
//     election (TASK-1257) uses it to break ties when
//     multiple peers could supply a markdown snapshot.
//   - Type      coarse classifier — "sync" carries Y.Doc updates that
//     must be persisted to the op-log; "awareness" carries
//     cursor/presence ephemera that must NOT be persisted.
//   - Data      raw y-protocol message; opaque to the server.
//   - Timestamp UnixMilli when the message entered the bus. Set
//     automatically on Publish if zero.
type OpEvent struct {
	ItemID    string
	ClientID  uint64
	Type      string
	Data      []byte
	Timestamp int64
}

// OpEvent.Type values. Kept narrow on purpose — the server should not
// need to know about more than the handful of categories that change
// its own behavior (persist vs broadcast-only). New y-protocol message
// types should map to one of these unless the server actively needs to
// distinguish them.
const (
	// OpTypeSync carries a Y.Doc binary update. Persisted to the
	// op-log (TASK-1252) on broadcast so reconnecting peers can
	// replay since their last cursor.
	OpTypeSync = "sync"

	// OpTypeAwareness carries cursor / selection / presence info
	// (CollaborationCursor extension, TASK-1264). Broadcast only —
	// NEVER persisted, since presence is meaningless after the
	// originating client disconnects.
	OpTypeAwareness = "awareness"
)

// OpBus is the cross-instance pub/sub interface for collab broadcasts.
// MemoryOpBus is the production implementation for single-instance
// deployments (every shipping target today: self-hosted single Go
// binary, pad-cloud single replica). A future RedisOpBus would
// implement the same interface for multi-replica fanout — that's
// scoped as a separate IDEA filed at PLAN-1248 close, since the
// dumb-relay design intentionally keeps Redis off the self-host
// dependency surface.
//
// Channels returned by Subscribe MUST be drained promptly. Slow
// subscribers — peers whose channel buffer fills before the consumer
// reads — have new events dropped (with a warning log) rather than
// blocking the publisher. This mirrors internal/events.MemoryBus and
// keeps the broadcast loop responsive even when one socket is
// momentarily backed up by the OS write buffer.
type OpBus interface {
	// Subscribe registers a subscriber for the given item and returns a
	// buffered channel of OpEvents. Caller is responsible for calling
	// Unsubscribe (typically in a defer) so the channel is closed and
	// the slot reclaimed.
	Subscribe(itemID string) chan OpEvent

	// Unsubscribe removes a subscriber and closes its channel. Safe to
	// call with an already-removed channel — no-op in that case.
	Unsubscribe(ch chan OpEvent)

	// Publish broadcasts an event to every subscriber whose itemID
	// matches event.ItemID. Non-blocking: full subscriber channels
	// drop the event with a warning. event.Timestamp is set to
	// time.Now().UnixMilli() if zero.
	Publish(event OpEvent)

	// SubscriberCount returns the number of active subscribers for the
	// given item. Useful for room-manager TTL accounting (TASK-1255 —
	// when the count hits 0, the room enters its 60s grace window).
	SubscriberCount(itemID string) int

	// Close shuts down the bus and closes every active subscriber
	// channel. After Close, Subscribe / Publish are undefined; callers
	// must not invoke them.
	Close()
}
