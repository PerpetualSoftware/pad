package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// redisChannelPrefix is prepended to workspace IDs for Redis pub/sub channels.
	redisChannelPrefix = "pad:events:"

	// redisSeqKey is the Redis key used for the global event sequence counter.
	// All instances share this counter so SSE event IDs are globally ordered
	// and Last-Event-ID is valid across any instance on reconnect.
	redisSeqKey = "pad:event_seq"
)

// RedisBus distributes events across multiple Pad instances via Redis pub/sub.
// Each instance subscribes to Redis channels for its locally-connected SSE clients,
// and publishes events to Redis so all instances see them.
type RedisBus struct {
	client *redis.Client

	mu          sync.RWMutex
	subscribers map[chan Event]*subscriber

	// Track which workspace channels we're subscribed to in Redis,
	// so we subscribe/unsubscribe as local SSE clients come and go.
	wsCounts map[string]int       // workspace → local subscriber count
	wsSubs   map[string]*redisSub // workspace → active Redis subscription

	// Monotonic sequence counter for event IDs (local to this instance).
	seq atomic.Int64

	// Per-workspace replay buffers for Last-Event-ID support.
	// Populated from events received via Redis pub/sub.
	replayMu      sync.RWMutex
	replayBuffers map[string]*replayBuffer
	replaySize    int

	ctx    context.Context
	cancel context.CancelFunc
}

// redisSub tracks an active Redis subscription for a workspace.
type redisSub struct {
	pubsub *redis.PubSub
	cancel context.CancelFunc
}

// NewRedisBus creates a new Redis-backed EventBus.
// The provided redis.Client should already be configured and connected.
func NewRedisBus(client *redis.Client) *RedisBus {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisBus{
		client:        client,
		subscribers:   make(map[chan Event]*subscriber),
		wsCounts:      make(map[string]int),
		wsSubs:        make(map[string]*redisSub),
		replayBuffers: make(map[string]*replayBuffer),
		replaySize:    DefaultReplayBufferSize,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Subscribe registers a local subscriber for the given workspace.
// Starts a Redis subscription for the workspace if this is the first local subscriber.
func (b *RedisBus) Subscribe(workspaceID string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, 64)
	b.subscribers[ch] = &subscriber{
		ch:          ch,
		workspaceID: workspaceID,
	}

	b.wsCounts[workspaceID]++
	if b.wsCounts[workspaceID] == 1 {
		// First local subscriber for this workspace — subscribe to Redis channel
		b.startRedisSubscription(workspaceID)
	}

	return ch
}

// SubscribeIfAllowed atomically checks limits and subscribes.
// NOTE: Limits are enforced against local (per-pod) subscriber counts only.
// In multi-replica deployments the effective cap is multiplied by the number
// of replicas.  For truly global caps, use a Redis-backed counter.
func (b *RedisBus) SubscribeIfAllowed(workspaceID string, maxGlobal, maxPerWorkspace int) (chan Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if maxGlobal > 0 && len(b.subscribers) >= maxGlobal {
		return nil, false
	}
	if maxPerWorkspace > 0 && b.wsCounts[workspaceID] >= maxPerWorkspace {
		return nil, false
	}

	ch := make(chan Event, 64)
	b.subscribers[ch] = &subscriber{
		ch:          ch,
		workspaceID: workspaceID,
	}

	b.wsCounts[workspaceID]++
	if b.wsCounts[workspaceID] == 1 {
		b.startRedisSubscription(workspaceID)
	}

	return ch, true
}

// Unsubscribe removes a local subscriber and closes its channel.
// Cancels the Redis subscription if this was the last local subscriber for the workspace.
func (b *RedisBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sub, ok := b.subscribers[ch]
	if !ok {
		return
	}

	delete(b.subscribers, ch)
	close(ch)

	wsID := sub.workspaceID
	b.wsCounts[wsID]--
	if b.wsCounts[wsID] <= 0 {
		delete(b.wsCounts, wsID)
		b.stopRedisSubscription(wsID)
	}
}

// Publish sends an event to Redis, which distributes it to all instances.
// Events are assigned a globally unique sequence ID via Redis INCR so that
// Last-Event-ID is valid across any instance on reconnect.
func (b *RedisBus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	// Assign a globally ordered sequence ID via Redis atomic counter.
	// This ensures all instances share the same ID space, so Last-Event-ID
	// from one instance is meaningful on any other instance.
	id, err := b.client.Incr(b.ctx, redisSeqKey).Result()
	if err != nil {
		// Fall back to local counter if Redis INCR fails (degraded mode).
		slog.Warn("failed to get global event ID from Redis, falling back to local", "error", err)
		id = b.seq.Add(1)
	}
	event.ID = id

	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal event for Redis", "error", err)
		return
	}

	channel := redisChannelPrefix + event.WorkspaceID
	if err := b.client.Publish(b.ctx, channel, data).Err(); err != nil {
		slog.Error("failed to publish event to Redis", "channel", channel, "error", err)
	}
}

// EventsSince returns buffered events for a workspace with IDs greater than sinceID.
// Returns nil if sinceID has been evicted from the buffer (gap too large).
func (b *RedisBus) EventsSince(workspaceID string, sinceID int64) []Event {
	b.replayMu.RLock()
	defer b.replayMu.RUnlock()

	rb, ok := b.replayBuffers[workspaceID]
	if !ok {
		return []Event{}
	}
	return rb.since(sinceID)
}

// Close shuts down all Redis subscriptions and closes local subscriber channels.
func (b *RedisBus) Close() {
	b.cancel() // signal all subscription goroutines to stop

	b.mu.Lock()
	defer b.mu.Unlock()

	for wsID, sub := range b.wsSubs {
		sub.cancel()
		sub.pubsub.Close()
		delete(b.wsSubs, wsID)
	}

	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}

// SubscriberCount returns the number of active local subscribers.
func (b *RedisBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// WorkspaceSubscriberCount returns the number of active local subscribers for a workspace.
func (b *RedisBus) WorkspaceSubscriberCount(workspaceID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.wsCounts[workspaceID]
}

// startRedisSubscription begins listening on a Redis channel for a workspace.
// Must be called with b.mu held.
func (b *RedisBus) startRedisSubscription(workspaceID string) {
	channel := redisChannelPrefix + workspaceID
	pubsub := b.client.Subscribe(b.ctx, channel)

	subCtx, subCancel := context.WithCancel(b.ctx)
	b.wsSubs[workspaceID] = &redisSub{
		pubsub: pubsub,
		cancel: subCancel,
	}

	go b.receiveMessages(subCtx, pubsub, workspaceID)
}

// stopRedisSubscription cancels and cleans up the Redis subscription for a workspace.
// Must be called with b.mu held.
func (b *RedisBus) stopRedisSubscription(workspaceID string) {
	sub, ok := b.wsSubs[workspaceID]
	if !ok {
		return
	}
	sub.cancel()
	sub.pubsub.Close()
	delete(b.wsSubs, workspaceID)
}

// receiveMessages reads from a Redis pub/sub channel and fans out to local subscribers.
func (b *RedisBus) receiveMessages(ctx context.Context, pubsub *redis.PubSub, workspaceID string) {
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var event Event
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				slog.Error("failed to unmarshal Redis event", "channel", msg.Channel, "error", err)
				continue
			}
			b.fanOutLocally(event)
		}
	}
}

// fanOutLocally distributes an event to all local subscribers for the event's workspace
// and stores it in the replay buffer.
func (b *RedisBus) fanOutLocally(event Event) {
	// Events received via Redis pub/sub already carry a global ID assigned by
	// the publishing instance via Redis INCR. We use that ID directly so all
	// instances share the same ID space for Last-Event-ID replay.

	// Store in replay buffer for reconnect replay.
	b.replayMu.Lock()
	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = newReplayBuffer(b.replaySize)
		b.replayBuffers[event.WorkspaceID] = rb
	}
	rb.append(event)
	b.replayMu.Unlock()

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
