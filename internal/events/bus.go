package events

import (
	"log/slog"
	"sync"
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

// Event represents a real-time event published when state changes occur.
type Event struct {
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

// subscriber wraps a channel with its workspace filter.
type subscriber struct {
	ch          chan Event
	workspaceID string
}

// Bus is an in-process pub/sub event bus that fans out events
// to all subscribers for a given workspace.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[chan Event]*subscriber
}

// New creates a new EventBus.
func New() *Bus {
	return &Bus{
		subscribers: make(map[chan Event]*subscriber),
	}
}

// Subscribe registers a new subscriber for the given workspace.
// Returns a buffered channel that will receive events for that workspace.
func (b *Bus) Subscribe(workspaceID string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, 64)
	b.subscribers[ch] = &subscriber{
		ch:          ch,
		workspaceID: workspaceID,
	}
	return ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *Bus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// Publish sends an event to all subscribers for the event's workspace.
// Non-blocking: if a subscriber's channel is full, the event is dropped
// and a warning is logged.
func (b *Bus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

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

// Close shuts down the event bus by closing all subscriber channels.
// SSE handler goroutines will see the channel close and exit cleanly.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// SubscriberCount returns the number of active subscribers (for testing/debugging).
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}
