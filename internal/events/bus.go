package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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

	// Comment events
	CommentCreated = "comment_created"
	CommentDeleted = "comment_deleted"

	// Reaction events
	ReactionAdded   = "reaction_added"
	ReactionRemoved = "reaction_removed"

	// Composite events
	ItemUpdatedWithComment = "item_updated_with_comment"
)

// Default replay buffer settings.
const (
	DefaultReplayBufferSize = 1024           // max events to retain per workspace
	DefaultReplayMaxAge     = 5 * time.Minute // discard events older than this
)

// Event represents a real-time event published when state changes occur.
type Event struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	WorkspaceID string `json:"workspace_id"`
	DocumentID  string `json:"document_id,omitempty"`
	ItemID      string `json:"item_id,omitempty"`
	Collection  string `json:"collection,omitempty"`
	Title       string `json:"title,omitempty"`
	DocType     string `json:"doc_type,omitempty"`
	Actor       string `json:"actor,omitempty"`
	ActorName   string `json:"actor_name,omitempty"`
	Source      string `json:"source,omitempty"`
	Timestamp   int64  `json:"timestamp"`
}

// EventBus is the interface for pub/sub event distribution.
// Implementations include MemoryBus (in-process) and RedisBus (cross-instance).
type EventBus interface {
	// Subscribe registers a new subscriber for the given workspace.
	// Returns a buffered channel that will receive events for that workspace.
	Subscribe(workspaceID string) chan Event

	// SubscribeIfAllowed atomically checks the global and per-workspace
	// subscriber limits and, only if both are satisfied, subscribes in the
	// same critical section.  Returns (ch, true) on success or (nil, false)
	// when a limit would be exceeded.  Pass 0 for either limit to disable it.
	SubscribeIfAllowed(workspaceID string, maxGlobal, maxPerWorkspace int) (chan Event, bool)

	// Unsubscribe removes a subscriber and closes its channel.
	Unsubscribe(ch chan Event)

	// Publish sends an event to all subscribers for the event's workspace.
	Publish(event Event)

	// EventsSince returns events for a workspace with IDs greater than sinceID.
	// Used to replay missed events on SSE reconnect (Last-Event-ID).
	// Returns nil if sinceID is too old and has been evicted from the buffer.
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
}

// since returns all buffered events with ID > sinceID, in chronological order.
// Returns nil if sinceID is older than the oldest buffered event AND the buffer
// is full (i.e. events have been evicted), meaning we can't guarantee completeness.
// Returns an empty (non-nil) slice if sinceID is current (no missed events).
// A sinceID of 0 means "give me everything in the buffer".
func (rb *replayBuffer) since(sinceID int64) []Event {
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
}

// MemoryBus is an in-process pub/sub event bus that fans out events
// to all subscribers for a given workspace. Suitable for single-instance deployments.
type MemoryBus struct {
	mu          sync.RWMutex
	subscribers map[chan Event]*subscriber

	// Monotonic sequence counter for event IDs.
	seq atomic.Int64

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
	}
}

// Subscribe registers a new subscriber for the given workspace.
// Returns a buffered channel that will receive events for that workspace.
func (b *MemoryBus) Subscribe(workspaceID string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, 64)
	b.subscribers[ch] = &subscriber{
		ch:          ch,
		workspaceID: workspaceID,
	}
	return ch
}

// SubscribeIfAllowed atomically checks limits and subscribes.
func (b *MemoryBus) SubscribeIfAllowed(workspaceID string, maxGlobal, maxPerWorkspace int) (chan Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if maxGlobal > 0 && len(b.subscribers) >= maxGlobal {
		return nil, false
	}
	if maxPerWorkspace > 0 {
		count := 0
		for _, sub := range b.subscribers {
			if sub.workspaceID == workspaceID {
				count++
			}
		}
		if count >= maxPerWorkspace {
			return nil, false
		}
	}

	ch := make(chan Event, 64)
	b.subscribers[ch] = &subscriber{
		ch:          ch,
		workspaceID: workspaceID,
	}
	return ch, true
}

// Unsubscribe removes a subscriber and closes its channel.
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

	// Assign a monotonic sequence ID.
	event.ID = b.seq.Add(1)

	// Store in replay buffer for reconnect replay.
	b.replayMu.Lock()
	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = newReplayBuffer(b.replaySize)
		b.replayBuffers[event.WorkspaceID] = rb
	}
	rb.append(event)
	b.replayMu.Unlock()

	// Fan out to live subscribers.
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		if sub.workspaceID != event.WorkspaceID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			slog.Warn("dropping event for slow subscriber", "type", event.Type, "workspace", event.WorkspaceID)
		}
	}
}

// EventsSince returns buffered events for a workspace with IDs greater than sinceID.
// Returns nil if sinceID has been evicted from the buffer (gap too large).
// Returns an empty slice if the caller is fully caught up.
func (b *MemoryBus) EventsSince(workspaceID string, sinceID int64) []Event {
	b.replayMu.RLock()
	defer b.replayMu.RUnlock()

	rb, ok := b.replayBuffers[workspaceID]
	if !ok {
		// No events ever published for this workspace.
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
