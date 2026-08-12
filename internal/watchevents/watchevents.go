// Package watchevents is the internal notification pipeline behind the
// padd watch / nudge feature (TASK-2533, per DOC-2479's event-model
// design). Producers (item update, item move, comment creation, ...)
// publish a Notification whenever something watch-worthy happens;
// GET /api/v1/events/stream is the single consumer, filtering the raw
// stream down to each caller's own watches plus the addressed-to-you
// predicate before anything reaches a client (DR-2: no firehose, no
// wildcard subscriptions).
//
// SINGLE-PROCESS LIMITATION. Bus here is an in-process pub/sub, exactly
// like internal/events.MemoryBus — and it has the same blind spot: in a
// multi-process padd deployment, a Notification published on one
// process is never seen by a stream connection held open on a
// different process, and is silently dropped for that connection. This
// is precisely why internal/events also ships a RedisBus for
// multi-instance deployments (Pad Cloud runs that shape). Bus is
// defined as an interface specifically so a Redis-backed implementation
// can slot in later without changing any producer or the stream
// handler; only MemoryBus is implemented in Phase 1. Do not point this
// package at a multi-process deployment without that follow-up — the
// local plugin monitor talking to a local, single-process padd is the
// only supported Phase 1 consumer.
package watchevents

import (
	"log/slog"
	"sync"
	"time"
)

// Notification kinds, matching DOC-2479's event payload contract
// (kind ∈ {status-change, assignment, comment, ask}) exactly.
const (
	KindStatusChange = "status-change"
	KindAssignment   = "assignment"
	KindComment      = "comment"
	// KindAsk is contract-reserved: DOC-2479 specs it as part of the kind
	// enum, but no Phase 1 producer emits it. Grounding "human-gate-shaped
	// collection" mechanically requires either a collection `kind` field
	// or a user→active-role binding, and this codebase has neither today
	// (TASK-2533 plan review, confirmed with the dispatcher). Left in the
	// enum so the wire contract doesn't have to change when a producer
	// eventually exists.
	KindAsk = "ask"
)

// Notification is one watch-worthy fact: an item's status changed, it was
// (re)assigned, or a comment landed on it. Consumed only by the
// GET /api/v1/events/stream handler, which filters and reshapes it per
// connected caller before anything is serialized to an HTTP response —
// this type itself is never sent to a client as-is.
type Notification struct {
	// ID is a monotonic sequence number assigned by the Bus on Publish,
	// used for Last-Event-ID resume. Zero until Publish assigns it.
	ID          int64
	WorkspaceID string
	ItemID      string
	// ItemRef is the human-facing issue ID ("TASK-214"), pre-resolved by
	// the producer so the stream handler and the CLI's --for-session
	// formatter never need a second lookup.
	ItemRef   string
	Kind      string
	Actor     string
	ActorName string
	Summary   string
	// AssignedUserID is populated on Kind == KindAssignment with the
	// item's NEW assignee (empty string = unassigned). The
	// addressed-to-you check compares this directly against the
	// connected caller's user ID — see server.sseWatchVisible.
	AssignedUserID string
	// StatusFieldKey / ToStatus are populated on Kind == KindStatusChange,
	// mirroring models.ItemMutationSignal, so the `--until field=value`
	// watch predicate can be evaluated against a Notification directly
	// without a second lookup back into the item.
	StatusFieldKey string
	ToStatus       string
	Timestamp      int64
}

// Bus is the pub/sub surface the watch pipeline's producers and the
// GET /api/v1/events/stream consumer share. See the package doc comment
// for why only an in-process MemoryBus exists in Phase 1.
type Bus interface {
	// Publish assigns the Notification a monotonic ID, stores it in the
	// replay buffer, and fans it out to every live Subscribe channel.
	Publish(n Notification)
	// Subscribe returns a channel that receives every future
	// Notification. There is exactly one logical stream (unlike
	// internal/events.EventBus, which is workspace-scoped) — per-caller
	// filtering happens in the consumer, not the bus, per DR-2.
	Subscribe() chan Notification
	// Unsubscribe removes a subscriber and closes its channel.
	Unsubscribe(ch chan Notification)
	// EventsSince returns buffered notifications with ID > sinceID, for
	// Last-Event-ID resume. Returns nil if sinceID has been evicted from
	// the buffer (gap too large — caller should treat this like the SSE
	// handler's sync_required signal).
	EventsSince(sinceID int64) []Notification
	// Close shuts the bus down, closing every subscriber channel.
	Close()
}

// DefaultReplayBufferSize and DefaultReplayMaxAge mirror
// internal/events' defaults. The watch stream is lower-volume than the
// per-workspace activity firehose (only status/assignment/comment
// notifications that matter for *someone's* watch or addressed-to-you
// state), so the same sizing is a reasonable starting point without
// needing independent tuning.
const (
	DefaultReplayBufferSize = 1024
	DefaultReplayMaxAge     = 5 * time.Minute
)

// replayEntry pairs a Notification with the time it was published, so
// the ring buffer can also enforce DefaultReplayMaxAge (unused for now —
// eviction is purely capacity-driven, exactly like internal/events —
// but kept alongside the Notification for parity/future use).
type replayBuffer struct {
	items []Notification
	size  int
	head  int
	count int
}

func newReplayBuffer(size int) *replayBuffer {
	return &replayBuffer{items: make([]Notification, size), size: size}
}

func (rb *replayBuffer) append(n Notification) {
	rb.items[rb.head] = n
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
}

// since mirrors internal/events.replayBuffer.since exactly (same gap /
// eviction semantics), scoped to this package's Notification type.
func (rb *replayBuffer) since(sinceID int64) []Notification {
	if rb.count == 0 {
		return []Notification{}
	}
	oldest := (rb.head - rb.count + rb.size) % rb.size
	oldestID := rb.items[oldest].ID
	newest := (rb.head - 1 + rb.size) % rb.size
	newestID := rb.items[newest].ID

	if sinceID > newestID {
		return nil
	}
	if sinceID > 0 && sinceID < oldestID && rb.count == rb.size {
		return nil
	}

	var result []Notification
	for i := 0; i < rb.count; i++ {
		idx := (oldest + i) % rb.size
		if rb.items[idx].ID > sinceID {
			result = append(result, rb.items[idx])
		}
	}
	if result == nil {
		result = []Notification{}
	}
	return result
}

// MemoryBus is an in-process implementation of Bus. See the package doc
// comment for its single-process limitation.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[chan Notification]struct{}
	seq         int64

	replayMu sync.RWMutex
	replay   *replayBuffer
}

// New creates a MemoryBus with the default replay buffer size.
func New() *MemoryBus {
	return NewWithReplaySize(DefaultReplayBufferSize)
}

// NewWithReplaySize creates a MemoryBus with a custom replay buffer
// capacity (tests use a small one).
func NewWithReplaySize(size int) *MemoryBus {
	return &MemoryBus{
		subscribers: make(map[chan Notification]struct{}),
		replay:      newReplayBuffer(size),
	}
}

func (b *MemoryBus) Publish(n Notification) {
	if n.Timestamp == 0 {
		n.Timestamp = time.Now().UnixMilli()
	}

	b.mu.Lock()
	b.seq++
	n.ID = b.seq
	b.mu.Unlock()

	b.replayMu.Lock()
	b.replay.append(n)
	b.replayMu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- n:
		default:
			slog.Warn("watchevents: dropping notification for slow subscriber", "kind", n.Kind, "item_ref", n.ItemRef)
		}
	}
}

func (b *MemoryBus) Subscribe() chan Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Notification, 64)
	b.subscribers[ch] = struct{}{}
	return ch
}

func (b *MemoryBus) Unsubscribe(ch chan Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

func (b *MemoryBus) EventsSince(sinceID int64) []Notification {
	b.replayMu.RLock()
	defer b.replayMu.RUnlock()
	return b.replay.since(sinceID)
}

func (b *MemoryBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}
