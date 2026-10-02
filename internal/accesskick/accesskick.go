// Package accesskick carries access-invalidation kicks between server
// instances (TASK-3365): "user U's access or credential changed" or
// "workspace W's did", so every instance's live connections for it re-check
// now instead of on their next revalidation tick.
//
// It is deliberately NOT the watch bus. The watch bus gives every message a
// sequence id, a slot in the shared client replay buffer and a place in each
// client subscriber's queue, so kicks published there evicted real
// notifications and could open gaps for unrelated clients (codex r1). A kick
// needs none of that: it is fire-and-forget, a lost one leaves the tick as
// the backstop, and nothing ever replays it. So it rides its own Redis
// pub/sub channel, with no sequence, buffer or client in reach.
package accesskick

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/PerpetualSoftware/pad/internal/redisns"
)

// Message addresses a kick to a user, a workspace, or both.
type Message struct {
	UserID      string `json:"u,omitempty"`
	WorkspaceID string `json:"w,omitempty"`
}

// Transport publishes kicks to every instance, this one included, and
// delivers the kicks other instances publish.
type Transport interface {
	Publish(ctx context.Context, m Message) error
	// Subscribe calls handle for every kick until stop is called. handle
	// must not block: it runs on the transport's receive goroutine.
	Subscribe(handle func(Message)) (stop func())
}

// ChannelSuffix is the Redis channel's name under the installation's key
// namespace (redisns.Keys.Name).
const ChannelSuffix = "access_kicks"

// RedisTransport is the cross-instance transport: plain Redis pub/sub,
// which delivers to every subscribed instance including the publisher.
type RedisTransport struct {
	client  *redis.Client
	channel string
}

// NewRedisTransport returns a transport on keys' access_kicks channel.
func NewRedisTransport(client *redis.Client, keys redisns.Keys) *RedisTransport {
	return &RedisTransport{client: client, channel: keys.Name(ChannelSuffix)}
}

// Publish sends m. Bounded, so a Redis outage costs the caller (a request
// handler that just committed an access change) at most a second.
func (t *RedisTransport) Publish(ctx context.Context, m Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return t.client.Publish(ctx, t.channel, payload).Err()
}

// Subscribe receives kicks until stop is called. go-redis re-establishes the
// subscription after a connection loss on its own; kicks published while it
// was down are lost, and the revalidation tick covers them.
func (t *RedisTransport) Subscribe(handle func(Message)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	ps := t.client.Subscribe(ctx, t.channel)
	// Wait for the subscription to be confirmed, so a kick published right
	// after Subscribe returns is not missed. Bounded: on failure the receive
	// loop below still runs, and go-redis keeps trying.
	confirmCtx, confirmCancel := context.WithTimeout(ctx, 5*time.Second)
	if _, err := ps.Receive(confirmCtx); err != nil {
		slog.Warn("access kicks: subscription not yet confirmed; will keep trying", "channel", t.channel, "error", err)
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
					slog.Warn("access kicks: unreadable message dropped", "error", err)
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
