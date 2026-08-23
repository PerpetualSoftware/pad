package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PerpetualSoftware/pad/internal/idspace"
)

// Event types
const (
	DocumentCreated  = "document_created"
	DocumentUpdated  = "document_updated"
	DocumentArchived = "document_archived"
	DocumentRestored = "document_restored"
	WorkspaceUpdated = "workspace_updated"

	// Item events (v2)
	ItemCreated  = "item_created"
	ItemUpdated  = "item_updated"
	ItemArchived = "item_archived"
	ItemRestored = "item_restored"

	// Collection events. Emitted when a collection's own row changes
	// (settings/schema/name/icon, e.g. a quick-action added). Routed by
	// Collection (slug) through the SSE visibility filter so sibling
	// ItemDetails / collection pages refresh their independent Collection
	// snapshot proactively — shrinking the optimistic-concurrency (409)
	// window (BUG-2265).
	CollectionUpdated = "collection_updated"

	// Comment events
	CommentCreated = "comment_created"
	CommentUpdated = "comment_updated"
	CommentDeleted = "comment_deleted"

	// Reaction events
	ReactionAdded   = "reaction_added"
	ReactionRemoved = "reaction_removed"

	// Star events
	ItemStarred   = "item_starred"
	ItemUnstarred = "item_unstarred"

	// Composite events: NONE. item_updated_with_comment was declared here
	// and never published by anything — its only producer was a hand-called
	// webhook dispatch using the dot-form name, retired under SPEC-3 v1.2
	// (Dave's ruling, day-48) because it folds into item.updated +
	// comment.created. The constant went with it rather than being left as
	// a name a future publisher could reach for.

	// Batch events. Emitted once for a whole bulk mutation (TASK-1668)
	// instead of one ItemUpdated/ItemArchived per row — the lane-header
	// bulk actions (archive/move/tag/untag/set-priority/assign all) can
	// touch a whole filtered lane, so per-item fan-out would flood both
	// SSE subscribers and webhooks.
	ItemsBulkUpdated = "items_bulk_updated"
)

// Default replay buffer settings.
const (
	DefaultReplayBufferSize = 1024            // max events to retain per workspace
	DefaultReplayMaxAge     = 5 * time.Minute // discard events older than this
)

// Event represents a real-time event published when state changes occur.
type Event struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	WorkspaceID string `json:"workspace_id"`
	DocumentID  string `json:"document_id,omitempty"`
	ItemID      string `json:"item_id,omitempty"`
	// CollectionID is the STABLE collection identity on a collection_updated
	// event (BUG-2265). Slugs are mutable and reusable and events replay, so a
	// stale rename event's OLD slug could be re-owned by a different collection;
	// clients therefore match these events by CollectionID, not the slug (which
	// stays only for rename-navigation URLs). Empty on non-collection events.
	CollectionID string `json:"collection_id,omitempty"`
	Collection   string `json:"collection,omitempty"`
	// NewSlug carries a collection's NEW slug on a collection_updated event
	// that is a rename (BUG-2265). The event is routed by Collection (the OLD
	// slug, which the sibling tabs still address) so old-slug watchers receive
	// it and can re-target to NewSlug. Empty for non-rename updates.
	NewSlug string `json:"new_slug,omitempty"`
	// ItemsChanged is set on a collection_updated event when a field MIGRATION
	// WAS REQUESTED (a schema change carrying migrations), independent of how
	// many rows actually changed (BUG-2265 Codex round 7). It's a SANITIZED
	// reconcile signal — a bare bool carrying NO per-item data and revealing
	// nothing about hidden item values — so it can be delivered to item-grant
	// subscribers; their client triggers a /items-changes deltaSync
	// (server-filtered to their grants) to pick up the migrated field JSON.
	ItemsChanged bool   `json:"items_changed,omitempty"`
	Title        string `json:"title,omitempty"`
	DocType      string `json:"doc_type,omitempty"`
	Actor        string `json:"actor,omitempty"`
	ActorName    string `json:"actor_name,omitempty"`
	Source       string `json:"source,omitempty"`
	UserID       string `json:"user_id,omitempty"` // For user-scoped events (e.g. star/unstar)
	Timestamp    int64  `json:"timestamp"`
	// Seq is the workspace-scoped monotonic mutation cursor of the
	// item the event references (PLAN-1343 / TASK-1352). Populated
	// for item lifecycle events (created / updated / archived /
	// restored) so the local-first read model (TASK-1358) can apply
	// the change in-place when the seq is contiguous with the
	// client's cursor, or trigger a /items-changes backfill when
	// there's a gap. Zero for non-item events (workspace_updated,
	// comment_*, reaction_*) and for legacy publishers that
	// haven't been upgraded.
	Seq int64 `json:"seq,omitempty"`
	// Op / Count describe a batch event (ItemsBulkUpdated, TASK-1668).
	// Op is the verb applied (archive/move/tag/untag/set-priority/
	// assign); Count is the number of items affected in this event's
	// Collection. Zero/empty for single-item events.
	//
	// A batch event is scoped to ONE Collection (the bulk endpoint emits
	// one per affected collection) so the SSE visibility filter routes
	// it like any collection-scoped event. It deliberately carries NO
	// per-item IDs: a batch can't be item-grant-filtered for guests on a
	// broadcast bus, so IDs would leak. Recipients react by running a
	// /items-changes delta, which IS visibility-filtered server-side;
	// Seq holds the max seq across the batch as the reconcile cursor.
	Op    string `json:"op,omitempty"`
	Count int    `json:"count,omitempty"`
}

// EventBus is the interface for pub/sub event distribution.
// Implementations include MemoryBus (in-process) and RedisBus (cross-instance).
type EventBus interface {
	// Subscribe registers a new subscriber for the given workspace.
	// Returns a buffered channel that will receive events for that workspace.
	Subscribe(workspaceID string) chan Event

	// SubscribeIfAllowed atomically checks the per-workspace subscriber
	// limit and, only if it is satisfied, subscribes in the same critical
	// section.  Returns (ch, true) on success or (nil, false) when the
	// limit would be exceeded.  Pass 0 to disable it.
	//
	// There is deliberately NO global limit here any more (BUG-2726). Pad
	// serves two SSE endpoints backed by two different buses, and a held
	// connection costs the same process resources whichever opened it, so
	// a global bound is a property of the PROCESS and cannot be enforced
	// by either bus alone — one bus counting its own subscribers would let
	// a caller exhaust the machine through the other while every
	// configured limit still read as satisfied. That bound now lives in
	// internal/server's streamAdmission, which both endpoints acquire from
	// before subscribing. The per-workspace bound stays here because it is
	// genuinely workspace-scoped and the other endpoint has no workspace
	// to count against.
	//
	// The second return is this subscriber's GAP SIGNAL (BUG-2730): a
	// capacity-1, coalescing channel that is raised whenever the bus fails to
	// deliver an event TO THIS SUBSCRIBER — today, a full channel at fan-out
	// time. Reading it is how a held-open stream learns it has a hole; the
	// SSE handler answers by emitting sync_required mid-stream. It is never
	// closed, so a consumer may select on it for the subscription's whole
	// life without the select spinning once the subscription ends; the event
	// channel closing is the end-of-life signal.
	SubscribeIfAllowed(workspaceID string, maxPerWorkspace int) (chan Event, <-chan struct{}, bool)

	// SubscribeAndReplaySince atomically checks the per-workspace limit,
	// registers the subscriber, and captures the buffered events above
	// sinceID — all in ONE critical section (BUG-2730).
	//
	// Subscribing and reading the replay buffer as two calls, which is what
	// the SSE handler did until this existed, leaves a window where an event
	// published in between lands in BOTH the replay set and the live channel,
	// so the client processes it twice. This closes that window structurally
	// rather than asking the consumer to dedupe by ID — the same guarantee
	// internal/watchevents' method of the same name provides, for the same
	// reason.
	//
	// missed is nil when this instance CANNOT VOUCH for the span (the
	// EventsSince contract, unchanged), which the handler turns into
	// sync_required. Callers must only interpret a nil missed that way when
	// they actually passed a resuming cursor: sinceID <= 0 means "not
	// resuming" and always yields a nil missed with nothing wrong.
	SubscribeAndReplaySince(workspaceID string, sinceID int64, maxPerWorkspace int) (ch chan Event, missed []Event, gaps <-chan struct{}, ok bool)

	// Unsubscribe removes a subscriber and closes its channel.
	Unsubscribe(ch chan Event)

	// Publish sends an event to all subscribers for the event's workspace.
	Publish(event Event)

	// EventsSince returns events for a workspace with IDs greater than sinceID.
	// Used to replay missed events on SSE reconnect (Last-Event-ID).
	//
	// Returns nil whenever this instance CANNOT VOUCH for the span the caller
	// is asking about, which the SSE handler turns into sync_required. That
	// covers eviction (the historical meaning), and since BUG-2731 also a
	// buffer whose coverage starts above the caller's cursor — including the
	// buffer not existing at all, which is a cold start, a restart, a
	// scale-up, or the first connection to a workspace on this instance.
	// Returns an empty (non-nil) slice only when the caller is genuinely
	// caught up.
	EventsSince(workspaceID string, sinceID int64) []Event

	// Close shuts down the event bus and cleans up resources.
	Close()

	// SubscriberCount returns the number of active local subscribers.
	SubscriberCount() int

	// WorkspaceSubscriberCount returns the number of active subscribers
	// for a specific workspace.
	WorkspaceSubscriberCount(workspaceID string) int
}

// replayBuffer is a bounded ring buffer of recent events for a single workspace.
// It supports efficient append and replay-since-ID queries.
type replayBuffer struct {
	events []Event
	size   int // max capacity
	head   int // next write position
	count  int // current number of events

	// knownFrom is the lowest event ID from which this buffer's coverage of
	// its workspace can be VOUCHED FOR: the first ID appended since this
	// instance began continuously receiving the workspace. Zero means
	// nothing has been appended, which is strictly less knowledge than any
	// non-zero value and must produce at least as strong a signal.
	//
	// CONTINUITY HERE MEANS RECEIVING-CONTINUITY, NEVER ID-CONTIGUITY, and
	// that is the one thing to understand before changing this (BUG-2731).
	// internal/watchevents has a field of the same name that ALSO detects
	// holes, by noticing a non-consecutive ID — and porting that here would
	// be a serious regression, because this bus has a GLOBAL counter and
	// PER-WORKSPACE buffers. Four publishes alternating across two
	// workspaces give W=[1 4] and X=[2 3]: a workspace's buffer holds
	// non-consecutive IDs BY CONSTRUCTION, so an id-contiguity check would
	// fire on nearly every append, reset knownFrom every time, and turn
	// every resume into sync_required — the false-positive inversion of the
	// bug this field exists to fix. The watch bus can do it because it has
	// one logical stream whose IDs are consecutive by construction; we
	// cannot, and no cleverer local check recovers it — see BUG-2735, which
	// carries the two real options (a per-workspace counter, or an authority
	// read against the shared counter) and the load arithmetic that ruled
	// the second one out.
	//
	// THE ID SPACE'S IDENTITY IS STILL NOT HERE, and after BUG-2736 that is a
	// division of labour rather than a gap. This buffer can say where its
	// coverage started; it cannot say which INCARNATION of the counter that ID
	// belongs to. The question is answered ABOVE this type, by whichever bus
	// owns the ID space: MemoryBus compares the cursor against its own
	// incarnation base (see internal/idspace), which it can do because it is
	// the sole publisher into that space.
	//
	// RedisBus cannot compute one — its counter is shared across processes —
	// so its ID space is identified by an epoch GENERATION travelling with
	// each message, reconciled in RedisBus.fanOut before anything reaches this
	// buffer. Same question, two answers, for reasons redis_bus.go records.
	//
	// Invalidated only by lifecycle facts this instance can actually
	// observe — today, losing the workspace's subscription (it stopping, or
	// a pub/sub reconnect) — never by a gap between IDs.
	//
	// IT BOUNDS THE START OF COVERAGE, NOT ITS CONTINUITY, and that limit is
	// structural rather than an omission: a message lost mid-subscription
	// (hold 100, miss 101, receive 102) leaves NO local trace here, because
	// per-workspace IDs are non-consecutive by construction and 101 may
	// simply have belonged to another workspace. BUG-2735 carries that case,
	// the two non-local options for detecting it, and why one of them was
	// ruled out on load. Do not try to recover it with a cleverer local
	// check; there isn't one.
	knownFrom int64

	// lastAppendedID is this buffer's high-water mark. IDs reaching a given
	// workspace's buffer ascend once every publisher is on the atomic script
	// — the shared counter only climbs, and the script makes publish order
	// equal ID order — but NOT during a mixed-version or mixed-FORMAT rollout,
	// where an older build assigns and publishes as two separate calls and can
	// deliver a LOWER ID after a higher one.
	//
	// So an arriving ID at or below this one means the sequence went
	// BACKWARDS: the counter was reset, or an out-of-order delivery landed.
	// RedisBus acts on either; MemoryBus assigns its own IDs and can never see
	// one, which is why this field is written there and never read.
	lastAppendedID int64

	// minKnownFrom is the lowest coverage start this buffer is ALLOWED to
	// claim, for a buffer replacing one whose sequence was discarded. Zero on
	// an ordinary buffer. See append and newReplayBufferAfterReset.
	minKnownFrom int64
}

// newReplayBufferAfterReset builds a buffer replacing one whose sequence was
// discarded. It refuses every cursor BELOW discarded+1 — that is, everything
// at or below the highest ID the discarded buffers held, EXCEPT that a cursor
// exactly equal to `discarded` is still served, since nothing above it was
// buffered for such a client to be missing. Pass 0 when nothing was held.
//
// Use newReplayBuffer for a genuinely new buffer; the two differ in what they
// may vouch for, and that difference is the whole point of having two.
func newReplayBufferAfterReset(size int, discarded int64) *replayBuffer {
	rb := newReplayBuffer(size)
	rb.minKnownFrom = discarded + 1
	return rb
}

func newReplayBuffer(size int) *replayBuffer {
	return &replayBuffer{
		events: make([]Event, size),
		size:   size,
	}
}

// append adds an event to the ring buffer, evicting the oldest if full.
func (rb *replayBuffer) append(e Event) {
	rb.events[rb.head] = e
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
	if rb.knownFrom == 0 {
		// First append since this buffer started (or restarted) covering
		// the workspace. From this ID forward we can answer honestly.
		rb.knownFrom = e.ID

		// ...EXCEPT on a buffer that REPLACED one whose ID space died, where
		// two separate assumptions behind the ordinary rule fail. Both are
		// corrected here because both produce the same silent skip.
		//
		// FIRST: since() serves sinceID+1 == knownFrom on the reasoning that
		// no ID lies strictly between the cursor and our first event — true
		// only when both are in the SAME ID space. Across a reset they are
		// not, so a client holding OLD 149 would be handed NEW 150 as though
		// it followed. Hence e.ID+1: the adjacent cursor is not adjacent here.
		//
		// SECOND: the reset DISCARDED buffered events, and a cursor at or
		// below the highest of them must not be told it is current — its
		// successor is exactly what we threw away. Hence the floor.
		//
		// The higher of the two wins, because each is a lower bound on what
		// this buffer can honestly claim and neither subsumes the other: the
		// floor is based on what we HELD, e.ID+1 on what we now SEE, and a
		// reset can move either one further out.
		if rb.minKnownFrom > 0 {
			rb.knownFrom = e.ID + 1
			if rb.minKnownFrom > rb.knownFrom {
				rb.knownFrom = rb.minKnownFrom
			}
			rb.minKnownFrom = 0
		}
	}
	rb.lastAppendedID = e.ID
}

// since returns all buffered events with ID > sinceID, in chronological order.
// Returns nil when this buffer cannot vouch for completeness across the
// requested span — the caller turns that into sync_required. Returns an empty
// (non-nil) slice if sinceID is current (no missed events). A sinceID of 0
// means "give me everything in the buffer" and is never a coverage question:
// a fresh client is not resuming from a position, so there is no span to span.
func (rb *replayBuffer) since(sinceID int64) []Event {
	// COVERAGE CHECK (BUG-2731). A resume asking about IDs at or below what
	// this buffer started covering cannot be answered honestly, and the
	// pre-BUG-2731 code answered it anyway — with an empty-but-non-nil slice,
	// which the SSE handler reads as "caught up". An empty buffer cannot
	// prove a client is current, and neither can a partial one whose first
	// event is above the client's cursor.
	//
	// sinceID+1 == knownFrom PASSES THIS CHECK: there is no ID strictly
	// between the cursor and our first event, so no workspace event can have
	// been missed in that span. (It may still be refused further down by the
	// eviction or newest-ID checks — this one check is not the whole answer.)
	// sinceID+1 < knownFrom leaves room for a missed event, and because the
	// ID space is shared across workspaces (see knownFrom) we cannot tell
	// whether the IDs in that room were ours. Refusing is the conservative
	// direction and the only honest one.
	if sinceID > 0 && (rb.knownFrom == 0 || sinceID+1 < rb.knownFrom) {
		return nil
	}

	if rb.count == 0 {
		return []Event{}
	}

	// Find the oldest event in the buffer
	oldest := (rb.head - rb.count + rb.size) % rb.size
	oldestID := rb.events[oldest].ID

	// Find the newest event in the buffer.
	newest := (rb.head - 1 + rb.size) % rb.size
	newestID := rb.events[newest].ID

	// If sinceID is beyond the newest event we have, the ID came from a
	// different sequence (e.g., a different instance in a Redis deployment).
	// We can't determine what was missed — signal a gap.
	if sinceID > newestID {
		return nil
	}

	// If the requested ID is older than our oldest AND the buffer has wrapped
	// (events were evicted), we can't guarantee completeness — signal a gap.
	// But if the buffer hasn't filled up yet, all events are still present.
	if sinceID > 0 && sinceID < oldestID && rb.count == rb.size {
		return nil
	}

	// Collect events with ID > sinceID
	var result []Event
	for i := 0; i < rb.count; i++ {
		idx := (oldest + i) % rb.size
		if rb.events[idx].ID > sinceID {
			result = append(result, rb.events[idx])
		}
	}
	if result == nil {
		result = []Event{}
	}
	return result
}

// subscriber wraps a channel with its workspace filter.
type subscriber struct {
	ch          chan Event
	workspaceID string

	// gaps carries the "you personally have a hole" signal to this one
	// subscriber's consumer (BUG-2730). Capacity 1 and written with a
	// non-blocking send, so it COALESCES: many drops between two reads
	// raise it once. That is deliberate and is the load bound — the
	// signal's rate is the consumer's read rate, never the drop rate,
	// which matters because the consumer we are signalling is by
	// definition one that could not keep up.
	//
	// Never closed by the bus's fan-out paths; Unsubscribe/Close own its
	// lifetime alongside ch, so the two are always retired together.
	gaps chan struct{}
}

// signalGap raises this subscriber's gap flag without blocking.
//
// Callers may hold the bus lock: the send can never block (capacity 1,
// default arm), so this adds no ordering hazard to a fan-out that already
// runs under it.
func (s *subscriber) signalGap() {
	select {
	case s.gaps <- struct{}{}:
	default:
	}
}

// newSubscriber builds a subscriber with both of its channels, so no path
// can register one that has an event channel and no gap channel — a nil
// gaps channel would silently discard every signal (a send on nil blocks,
// and the default arm would take it forever).
func newSubscriber(workspaceID string) *subscriber {
	return &subscriber{
		ch:          make(chan Event, 64),
		workspaceID: workspaceID,
		gaps:        make(chan struct{}, 1),
	}
}

// MemoryBus is an in-process pub/sub event bus that fans out events
// to all subscribers for a given workspace. Suitable for single-instance deployments.
type MemoryBus struct {
	// observable carries the optional operational-event seam (BUG-2731).
	// Wired here as well as on RedisBus, and deliberately: a single-process
	// deployment restarts too, and the cold-buffer resume gap is exactly as
	// real there. The reset counters stay at zero here by construction —
	// MemoryBus assigns its own IDs and has no shared counter to lose.
	observable

	mu          sync.RWMutex
	subscribers map[chan Event]*subscriber

	// Monotonic sequence counter for event IDs, counting up from base.
	seq atomic.Int64

	// base identifies THIS incarnation of the counter. Set once at
	// construction; see internal/idspace for what it buys and what it costs.
	// A zero base is the pre-BUG-2736 behaviour (IDs from 1) and is
	// deliberately unreachable through any constructor.
	//
	// A NUMERIC BASE HERE, A TRAVELLING EPOCH ON RedisBus. They are not two
	// spellings of one idea and must not be symmetrized into one; the reason,
	// and why a numeric base for Redis is a deferred follow-on rather than an
	// oversight, is stated once in internal/idspace's package comment.
	base int64

	// Per-workspace replay buffers for Last-Event-ID support.
	replayMu      sync.RWMutex
	replayBuffers map[string]*replayBuffer
	replaySize    int
	replayMaxAge  time.Duration
}

// New creates a new in-memory EventBus with default replay buffer settings.
func New() *MemoryBus {
	return NewWithReplay(DefaultReplayBufferSize, DefaultReplayMaxAge)
}

// NewWithReplay creates a new in-memory EventBus with custom replay settings.
func NewWithReplay(bufferSize int, maxAge time.Duration) *MemoryBus {
	return &MemoryBus{
		subscribers:   make(map[chan Event]*subscriber),
		replayBuffers: make(map[string]*replayBuffer),
		replaySize:    bufferSize,
		replayMaxAge:  maxAge,
		base:          idspace.New(),
	}
}

// Subscribe registers a new subscriber for the given workspace.
// Returns a buffered channel that will receive events for that workspace.
func (b *MemoryBus) Subscribe(workspaceID string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	sub := newSubscriber(workspaceID)
	b.subscribers[sub.ch] = sub
	return sub.ch
}

// SubscribeIfAllowed atomically checks limits and subscribes.
func (b *MemoryBus) SubscribeIfAllowed(workspaceID string, maxPerWorkspace int) (chan Event, <-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if maxPerWorkspace > 0 {
		count := 0
		for _, sub := range b.subscribers {
			if sub.workspaceID == workspaceID {
				count++
			}
		}
		if count >= maxPerWorkspace {
			return nil, nil, false
		}
	}

	sub := newSubscriber(workspaceID)
	b.subscribers[sub.ch] = sub
	return sub.ch, sub.gaps, true
}

// SubscribeAndReplaySince implements EventBus. See the interface for the
// guarantee; the mechanism here is the lock order Publish documents — b.mu
// exclusively, then b.replayMu, so no publish can be between its append and
// its fan-out while this runs.
func (b *MemoryBus) SubscribeAndReplaySince(workspaceID string, sinceID int64, maxPerWorkspace int) (chan Event, []Event, <-chan struct{}, bool) {
	// Counted on the way out, with no lock held, and only for a caller that
	// was actually resuming — mirroring EventsSince, whose own report this
	// path bypasses because it reads the buffer directly.
	var missed []Event
	resuming := sinceID > 0
	defer func() {
		if resuming && missed == nil {
			b.reportResumeGap(workspaceID)
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()

	if maxPerWorkspace > 0 {
		count := 0
		for _, sub := range b.subscribers {
			if sub.workspaceID == workspaceID {
				count++
			}
		}
		if count >= maxPerWorkspace {
			// resuming is cleared so the deferred report does not fire for a
			// subscription that never happened: a refused connection is an
			// admission event, not a resume this instance could not serve.
			resuming = false
			return nil, nil, nil, false
		}
	}

	sub := newSubscriber(workspaceID)
	b.subscribers[sub.ch] = sub

	if resuming {
		missed = b.eventsSinceLocked(workspaceID, sinceID)
	}
	return sub.ch, missed, sub.gaps, true
}

// Unsubscribe removes a subscriber and closes its channel.
//
// The subscriber's gap channel is deliberately NOT closed. A closed channel
// is always ready, so a consumer selecting on both would spin at full speed
// between Unsubscribe and its own exit; the event channel closing is the
// end-of-life signal, and it already is one.
func (b *MemoryBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// Publish sends an event to all subscribers for the event's workspace.
// Non-blocking: if a subscriber's channel is full, the event is dropped
// and a warning is logged. Events are assigned a monotonic sequence ID
// and stored in the replay buffer for Last-Event-ID support.
func (b *MemoryBus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	// Registered FIRST so it runs LAST — after every Unlock below. Reports
	// fire with no bus lock held, per Observer's contract, which is what lets
	// an observer call back into the bus. Same discipline as
	// internal/watchevents' pendingReports, without needing the type for a
	// single counter.
	dropped := 0
	defer func() {
		for range dropped {
			b.reportDropped(DropReasonSlowSubscriber)
		}
	}()

	// SUBSCRIBER-SET LOCK FIRST, AND HELD ACROSS BOTH HALVES (BUG-2730).
	// Publish has two effects a resuming client can observe — the append to
	// the replay buffer and the send to live channels — and until this fix
	// they were two separate critical sections with a window between them.
	// A SubscribeAndReplaySince landing in that window sees the event in
	// NEITHER (subscribed after the fan-out snapshot, read the buffer before
	// the append) or in BOTH. Holding b.mu across both makes the pair atomic
	// with respect to that call, which takes b.mu exclusively.
	//
	// LOCK ORDER IS b.mu THEN b.replayMu, everywhere, without exception. The
	// reverse nesting anywhere in this file is a deadlock; EventsSince takes
	// replayMu alone, which is safe precisely because it takes b.mu never.
	//
	// RLock rather than Lock: concurrent publishes still fan out
	// concurrently, exactly as before. The only thing excluded is the
	// exclusive holder — subscribe, unsubscribe, close.
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Store in replay buffer for reconnect replay.
	b.replayMu.Lock()

	// THE ID IS ASSIGNED UNDER THIS LOCK, and that is the whole reason the
	// assignment sits here rather than before it. Assigned outside, two
	// concurrent publishes could take ids N and N+1 and then append in the
	// other order — and replayBuffer.since computes oldest and newest by
	// POSITION, so a buffer holding [N+1, N] reports N as its newest and
	// answers a resume from N+1 with sync_required. Under the lock, buffer
	// order equals id order by construction.
	//
	// It is the same invariant RedisBus buys with an atomic publish script
	// (see publishScript): publish order equals id order, because the
	// alternative is a buffer whose own ordering assumptions are false.
	//
	// The base is what makes a restarted process's ids distinguishable from
	// the dead space's — see internal/idspace.
	event.ID = b.base + b.seq.Add(1)

	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = newReplayBuffer(b.replaySize)
		b.replayBuffers[event.WorkspaceID] = rb
	}
	rb.append(event)
	b.replayMu.Unlock()

	// Fan out to live subscribers, still under the b.mu.RLock taken above.
	for _, sub := range b.subscribers {
		if sub.workspaceID != event.WorkspaceID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// THE DROP IS NOW ANNOUNCED TO THE SUBSCRIBER IT HAPPENED TO
			// (BUG-2730). Before this, the process knew and the one
			// consumer whose correctness depended on it did not: a later
			// delivered event advances that client's Last-Event-ID past
			// the dropped IDs, after which no replica will ever replay
			// them because every replica agrees the cursor is current.
			//
			// Announced to THIS subscriber only. A full channel is a fact
			// about one connection's read rate; every other subscriber
			// received the event.
			slog.Warn("dropping event for slow subscriber", "type", event.Type, "workspace", event.WorkspaceID)
			sub.signalGap()
			dropped++
		}
	}
}

// EventsSince returns buffered events for a workspace with IDs greater than sinceID.
// Returns nil when this instance cannot vouch for the requested span (gap too
// large, or coverage that never reached back that far — see replayBuffer.since).
// Returns an empty slice if the caller is fully caught up.
func (b *MemoryBus) EventsSince(workspaceID string, sinceID int64) []Event {
	// Counted in ONE place, on the way out, so both ways of failing to serve
	// a resume land on the same counter: the workspace having no buffer at
	// all, and since() refusing the span. Counting at each `return nil`
	// instead is how a gap metric ends up measuring one of its two halves —
	// the failure BUG-2699's own metric shipped with, and the reason this is
	// structural rather than remembered.
	var missed []Event
	defer func() {
		if missed == nil {
			b.reportResumeGap(workspaceID)
		}
	}()

	missed = b.eventsSinceLocked(workspaceID, sinceID)
	return missed
}

// eventsSinceLocked is EventsSince without the observer report — the shared
// core, so the two callers cannot drift in what "cannot vouch" means.
//
// It takes b.replayMu itself and takes b.mu NEVER, which is what makes it
// callable both on its own (EventsSince) and with b.mu already held
// exclusively (SubscribeAndReplaySince). See Publish for the lock order that
// makes the second case safe.
//
// The report is the CALLER's job because the two callers count different
// populations: every EventsSince call is a resume, while a
// SubscribeAndReplaySince may be a fresh subscription that never asked.
func (b *MemoryBus) eventsSinceLocked(workspaceID string, sinceID int64) []Event {
	// A CURSOR THIS INCARNATION COULD NOT HAVE ISSUED IS A GAP, and this bus
	// can say so EXACTLY rather than infer it (BUG-2736). Every ID it assigns
	// is above base, so a non-zero cursor at or below base was issued by a
	// previous incarnation of this process, whose events died with it.
	//
	// The coverage check in replayBuffer.since would refuse almost all of
	// these anyway, because a fresh incarnation's knownFrom sits a whole
	// stride above the dead space — but "almost all" is an inference from the
	// stride invariant, and this is a fact about who issued the number. The
	// one case the inference loses is the adjacent cursor: since() serves
	// sinceID+1 == knownFrom on the reasoning that no ID lies strictly
	// between, which is only true WITHIN one ID space. Checking the base
	// makes that impossible to reach instead of improbable.
	//
	// sinceID == 0 is exempt: a fresh client is not resuming from a position.
	if sinceID > 0 && sinceID <= b.base {
		return nil
	}

	b.replayMu.RLock()
	defer b.replayMu.RUnlock()

	rb, ok := b.replayBuffers[workspaceID]
	if !ok {
		// Nothing has been published for this workspace IN THIS PROCESS.
		//
		// For a fresh client (sinceID == 0) that is honestly "nothing to
		// replay". For a RESUMING client it is the strongest form of "cannot
		// vouch" there is (BUG-2731): a non-zero cursor for a workspace we
		// have never published to names events we cannot reconstruct, whoever
		// issued it. Answering []Event{} told that client it was caught up and
		// silently dropped everything it had missed.
		//
		// A cursor from a PREVIOUS INCARNATION of this process is refused
		// earlier, by the base check above — since BUG-2736 this bus counts
		// from an incarnation base rather than from 1, so the two cases are
		// distinguishable and are answered separately.
		if sinceID > 0 {
			return nil
		}
		return []Event{}
	}
	return rb.since(sinceID)
}

// Close shuts down the event bus by closing all subscriber channels.
// SSE handler goroutines will see the channel close and exit cleanly.
func (b *MemoryBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// SubscriberCount returns the number of active subscribers (for testing/debugging).
func (b *MemoryBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// WorkspaceSubscriberCount returns the number of active subscribers for a workspace.
func (b *MemoryBus) WorkspaceSubscriberCount(workspaceID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, sub := range b.subscribers {
		if sub.workspaceID == workspaceID {
			count++
		}
	}
	return count
}
