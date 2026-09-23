package events

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// BUG-2799 builds a first-reply probe on PubSub.ReceiveTimeout. That is only
// safe if a probe that runs out of time leaves the connection alone. If the
// timeout made go-redis reconnect, the reconnect would RE-SUBSCRIBE, and the
// second acknowledgement would be counted as a resubscription, which is the
// accounting BUG-2739 built. These two tests pin what go-redis actually does.
//
// The reason is in the library (go-redis v9.22.0). ReceiveTimeout releases the
// conn with allowTimeout = timeout > 0 (pubsub.go), and isBadConn (error.go)
// excuses a net timeout only when allowTimeout is set. A ctx deadline does NOT
// arrive as context.DeadlineExceeded: it becomes the socket read deadline, so it
// too surfaces as a net i/o timeout (measured), but under Receive's timeout of 0
// allowTimeout is false and the conn is treated as bad. So bounding the wait
// with the timeout argument keeps the subscription, and bounding it only with a
// ctx deadline replaces it. The second test is the negative control: it shows
// the instrument (the SUBSCRIBE count, plus what the next reply is) really does
// see a reconnect when one happens.

func countingSubscribeServer(t *testing.T) (*miniredis.Miniredis, *atomic.Int32) {
	t.Helper()
	mr := miniredis.RunT(t)
	var subscribes atomic.Int32
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		if strings.EqualFold(cmd, "SUBSCRIBE") {
			subscribes.Add(1)
		}
		return false
	})
	return mr, &subscribes
}

func subscribeAndConsumeAck(t *testing.T, mr *miniredis.Miniredis) (*redis.Client, *redis.PubSub) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	pubsub := client.Subscribe(context.Background(), "probe-chan")
	t.Cleanup(func() { _ = pubsub.Close() })
	msg, err := pubsub.ReceiveTimeout(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatalf("initial acknowledgement: %v", err)
	}
	if _, ok := msg.(*redis.Subscription); !ok {
		t.Fatalf("initial reply = %T, want *redis.Subscription", msg)
	}
	return client, pubsub
}

func TestReceiveTimeoutExpiryKeepsTheSubscription(t *testing.T) {
	mr, subscribes := countingSubscribeServer(t)
	client, pubsub := subscribeAndConsumeAck(t, mr)
	ctx := context.Background()

	_, err := pubsub.ReceiveTimeout(ctx, 50*time.Millisecond)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("an idle probe should run out of time as a net timeout, got %v", err)
	}

	if err := client.Publish(ctx, "probe-chan", "after-timeout").Err(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	msg, err := pubsub.ReceiveTimeout(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("receive after the timeout: %v", err)
	}
	m, ok := msg.(*redis.Message)
	if !ok {
		t.Fatalf("the reply after a timed-out probe = %T (%v), want the published *redis.Message; a *redis.Subscription here means go-redis reconnected and re-subscribed", msg, msg)
	}
	if m.Payload != "after-timeout" {
		t.Fatalf("payload = %q, want %q", m.Payload, "after-timeout")
	}
	if n := subscribes.Load(); n != 1 {
		t.Fatalf("SUBSCRIBE sent %d times, want 1: the timed-out probe made go-redis re-subscribe", n)
	}
}

func TestReceiveBoundedByContextDeadlineReSubscribes(t *testing.T) {
	mr, subscribes := countingSubscribeServer(t)
	_, pubsub := subscribeAndConsumeAck(t, mr)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := pubsub.Receive(ctx)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("an idle Receive under a 50ms ctx deadline should fail as a net timeout, got %v", err)
	}

	// The redial inside the reconnect runs under the same expired ctx and fails,
	// so the re-SUBSCRIBE is lazy: it happens on the next read, whose first reply
	// is then the new acknowledgement rather than anything published.
	msg, err := pubsub.ReceiveTimeout(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatalf("receive after the ctx-deadline expiry: %v", err)
	}
	if _, ok := msg.(*redis.Subscription); !ok {
		t.Fatalf("the reply after a ctx-deadline expiry = %T (%v), want a *redis.Subscription from the re-subscribe; without a reconnect here the positive test above cannot tell the two bounds apart", msg, msg)
	}
	if n := subscribes.Load(); n != 2 {
		t.Fatalf("SUBSCRIBE sent %d times, want 2", n)
	}
}
