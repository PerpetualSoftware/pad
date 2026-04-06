package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// redisChannelPrefix is prepended to workspace IDs for Redis pub/sub channels.
	redisChannelPrefix = "pad:events:"

	// reconnectDelay is how long to wait before retrying a failed Redis subscription.
	reconnectDelay = 2 * time.Second
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
	wsCounts map[string]int         // workspace → local subscriber count
	wsSubs   map[string]*redisSub   // workspace → active Redis subscription

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
		client:      client,
		subscribers: make(map[chan Event]*subscriber),
		wsCounts:    make(map[string]int),
		wsSubs:      make(map[string]*redisSub),
		ctx:         ctx,
		cancel:      cancel,
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
func (b *RedisBus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

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

// fanOutLocally distributes an event to all local subscribers for the event's workspace.
func (b *RedisBus) fanOutLocally(event Event) {
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
