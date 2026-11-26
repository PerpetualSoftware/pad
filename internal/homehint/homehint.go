// Package homehint carries home_changed refetch hints between server instances
// (TASK-4864): when a write on one instance commits, other instances' live
// ?home=true watch streams receive the hint without routing it through the
// watch bus.
//
// It is deliberately NOT the watch bus. The watch bus gives every message a
// sequence id, a slot in the shared client replay buffer and a place in each
// client subscriber's queue, so hints published there evicted real
// notifications and competed with pushes, status changes and chat. A home hint
// needs none of that: it is best-effort, never replayed on resume, and rides
// its own Redis pub/sub channel.
package homehint

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// Message carries a single home_changed refetch hint across instances.
type Message struct {
	Origin       string `json:"o"`
	WorkspaceID  string `json:"w"`
	CollectionID string `json:"c,omitempty"`
	ItemID       string `json:"i,omitempty"`
	Timestamp    int64  `json:"t"`
}

// Transport publishes home hints to every instance, this one included, and
// delivers the hints other instances publish.
type Transport interface {
	Publish(ctx context.Context, m Message) error
	// Subscribe calls handle for every hint until stop is called. handle
	// must not block: it runs on the transport's receive goroutine.
	Subscribe(handle func(Message)) (stop func())
}

// ChannelSuffix is the Redis channel's name under the installation's key
// namespace (redisns.Keys.Name).
const ChannelSuffix = "home_hints"

// RedisTransport is the cross-instance transport: plain Redis pub/sub,
// which delivers to every subscribed instance including the publisher.
type RedisTransport struct {
	client  *redis.Client
	channel string
}

// NewRedisTransport returns a transport on keys' home_hints channel. It
// uses its OWN client, cloned from client's options with context timeouts
// enabled: the shared client leaves ContextTimeoutEnabled off, so command I/O
// ignores a context deadline and a stalled Redis held a publish for the
// client's full 5s read timeout.
func NewRedisTransport(client *redis.Client, keys redisns.Keys) *RedisTransport {
	opts := *client.Options()
	opts.ContextTimeoutEnabled = true
	// The source client's push-notification processor is ITS state: reusing
	// it registers the clone's handlers on the original's registry (duplicate
	// MOVING registrations, and a panic with maintenance
	// notifications on). NewClient builds a fresh one when this is nil.
	opts.PushNotificationProcessor = nil
	return &RedisTransport{client: redis.NewClient(&opts), channel: keys.Name(ChannelSuffix)}
}

// Close releases the transport's own client. The server closes a transport
// it replaces or clears (SetHomeHintTransport), so a caller hands over
// ownership on installing one.
func (t *RedisTransport) Close() error {
	return t.client.Close()
}

// Publish sends m, bounded at a second. The server calls it from a
// background worker, never on a request path.
func (t *RedisTransport) Publish(ctx context.Context, m Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return t.client.Publish(ctx, t.channel, payload).Err()
}

// Subscribe receives hints until stop is called. go-redis re-establishes the
// subscription after a connection loss on its own; hints published while it
// was down are lost (hints are best-effort and never replayed).
func (t *RedisTransport) Subscribe(handle func(Message)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	ps := t.client.Subscribe(ctx, t.channel)
	// Wait for the subscription to be confirmed, so a hint published right
	// after Subscribe returns is not missed. Bounded: on failure the receive
	// loop below still runs, and go-redis keeps trying.
	confirmCtx, confirmCancel := context.WithTimeout(ctx, 5*time.Second)
	if _, err := ps.Receive(confirmCtx); err != nil {
		slog.Warn("home hints: subscription not yet confirmed; will keep trying", "channel", t.channel, "error", err)
	}
	confirmCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ch := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var m Message
				if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
					slog.Debug("home hints: unreadable message dropped", "error", err)
					continue
				}
				handle(m)
			}
		}
	}()
	return func() {
		cancel()
		_ = ps.Close()
		<-done
	}
}
