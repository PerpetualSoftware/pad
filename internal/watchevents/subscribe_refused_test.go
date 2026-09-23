package watchevents

import (
	"context"
	"errors"
	"testing"
)

// BUG-2800: the watch bus had no way to refuse a subscriber, so the SSE handler
// admitted streams on an instance that could deliver nothing to them. These pin
// the bus half; the handler's mapping is pinned end to end in
// internal/server/handlers_watch_events_refused_test.go.

// assertRefused checks the whole refusal contract: the right error, nothing
// registered, and a CLOSED channel, so a caller that ignores the error still
// sees its stream end instead of blocking on a channel nobody will close.
func assertRefused(t *testing.T, what string, ch chan Notification, err, want error, registered func() int) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: error = %v, want %v", what, err, want)
	}
	if n := registered(); n != 0 {
		t.Fatalf("%s: %d subscribers registered after a refusal, want 0", what, n)
	}
	select {
	case _, open := <-ch:
		if open {
			t.Fatalf("%s: the refused channel delivered a value", what)
		}
	default:
		t.Fatalf("%s: the refused channel is open; a caller ignoring the error would block on it forever", what)
	}
}

func redisSubscriberCount(b *RedisBus) func() int {
	return func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.subscribers)
	}
}

func TestARedisBusWithNoSubscriptionRefusesSubscribers(t *testing.T) {
	b := newLocalOnlyBus(16)
	defer b.Close()
	// The state a failed or rejected constructor SUBSCRIBE leaves (BUG-2764,
	// BUG-2799), and the one an idle cycle passes through between connections.
	b.mu.Lock()
	b.pubsub = nil
	b.mu.Unlock()

	ch, _, err := b.Subscribe()
	assertRefused(t, "Subscribe", ch, err, ErrNotSubscribed, redisSubscriberCount(b))

	ch, missed, _, err := b.SubscribeAndReplaySince(context.Background(), 1)
	assertRefused(t, "SubscribeAndReplaySince", ch, err, ErrNotSubscribed, redisSubscriberCount(b))
	if missed != nil {
		t.Fatalf("a refused resume returned a replay of %d notifications", len(missed))
	}
}

// TestARedisBusWithASubscriptionAdmits is the control: the refusal keys on the
// empty slot, not on anything the fixture does. Without it the test above would
// pass against a bus that refuses everyone.
func TestARedisBusWithASubscriptionAdmits(t *testing.T) {
	b := newLocalOnlyBus(16)
	defer b.Close()

	ch, _, err := b.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe on a subscribed instance: %v", err)
	}
	defer b.Unsubscribe(ch)
	ch2, _, _, err := b.SubscribeAndReplaySince(context.Background(), 0)
	if err != nil {
		t.Fatalf("SubscribeAndReplaySince on a subscribed instance: %v", err)
	}
	defer b.Unsubscribe(ch2)
	if n := redisSubscriberCount(b)(); n != 2 {
		t.Fatalf("%d subscribers registered, want 2", n)
	}
}

func TestAClosedBusRefusesSubscribersWithErrBusClosed(t *testing.T) {
	t.Run("RedisBus", func(t *testing.T) {
		b := newLocalOnlyBus(16)
		b.Close()
		ch, _, err := b.Subscribe()
		assertRefused(t, "Subscribe", ch, err, ErrBusClosed, redisSubscriberCount(b))
		ch, _, _, err = b.SubscribeAndReplaySince(context.Background(), 1)
		assertRefused(t, "SubscribeAndReplaySince", ch, err, ErrBusClosed, redisSubscriberCount(b))
	})
	t.Run("MemoryBus", func(t *testing.T) {
		b := New()
		b.Close()
		count := func() int {
			b.mu.Lock()
			defer b.mu.Unlock()
			return len(b.subscribers)
		}
		ch, _, err := b.Subscribe()
		assertRefused(t, "Subscribe", ch, err, ErrBusClosed, count)
		ch, _, _, err = b.SubscribeAndReplaySince(context.Background(), 1)
		assertRefused(t, "SubscribeAndReplaySince", ch, err, ErrBusClosed, count)
	})
}

// TestACancelledResumeIsADepartureNotARefusal pins the one path that returns a
// closed channel WITHOUT an error: a caller whose context has already ended
// (BUG-2751). There is nobody left to read a 503, so it must not be reported
// as one, and the handler must not log a refusal for ordinary disconnect churn.
func TestACancelledResumeIsADepartureNotARefusal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, bus := range map[string]Bus{"RedisBus": newLocalOnlyBus(16), "MemoryBus": New()} {
		ch, _, _, err := bus.SubscribeAndReplaySince(ctx, 1)
		if err != nil {
			t.Errorf("%s: a cancelled resume returned %v, want no error", name, err)
		}
		if _, open := <-ch; open {
			t.Errorf("%s: a cancelled resume returned an open channel", name)
		}
		bus.Close()
	}
}
