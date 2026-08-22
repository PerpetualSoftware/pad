package watchevents

import (
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// BUG-2699 — Publish reports acceptance.
//
// The pre-fix behaviour these pin is not "an error was swallowed": it is
// that a post-Close publish on MemoryBus did nothing AT ALL and said
// nothing about it — no error (there was no channel to return one on)
// and, unlike RedisBus, not even a log line, while still consuming a
// sequence id. The single-process deployment had the same shutdown
// window as the Redis one, with strictly less evidence.

// TestMemoryBusPublishAfterCloseReportsClosed drives the case the
// shutdown ordering in cmd/pad/cmd_server.go actually creates: the bus is
// closed before http.Server.Shutdown drains handlers, so a push already
// in its handler publishes into a closed bus.
func TestMemoryBusPublishAfterCloseReportsClosed(t *testing.T) {
	t.Parallel()
	bus := New()

	// POSITIVE CONTROL first, and it is not decoration: without it a bus
	// that refused EVERY publish would satisfy the assertion below, and
	// this test would be evidence that nothing works rather than that
	// Close is what refuses.
	if err := bus.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish on a live bus must be accepted, got %v", err)
	}

	bus.Close()

	err := bus.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("expected ErrBusClosed after Close, got %v", err)
	}
}

// TestMemoryBusPublishAfterCloseDoesNotBurnSequence asserts the OTHER
// half of the old behaviour — the part that is invisible in the return
// value. The unfixed Publish incremented seq and appended to the replay
// buffer for a notification no subscriber could ever receive, so a
// resuming client's Last-Event-ID space contained ids that never
// corresponded to a delivered notification.
//
// Written as an assertion about what the WRONG behaviour DOES (the
// counter moved) rather than about what the right one leaves looking
// unchanged (CONVE-12).
func TestMemoryBusPublishAfterCloseDoesNotBurnSequence(t *testing.T) {
	t.Parallel()
	bus := New()
	if err := bus.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("live publish: %v", err)
	}

	before := bus.EventsSince(0)
	if len(before) != 1 {
		t.Fatalf("expected 1 buffered notification before Close, got %d", len(before))
	}
	highWater := before[len(before)-1].ID

	bus.Close()
	_ = bus.Publish(Notification{Kind: KindComment, ItemRef: "TASK-2"})

	after := bus.EventsSince(0)
	if len(after) != 1 {
		t.Fatalf("post-Close publish must not append to the replay buffer: %d entries", len(after))
	}
	if after[len(after)-1].ID != highWater {
		t.Fatalf("post-Close publish burned a sequence id: %d -> %d", highWater, after[len(after)-1].ID)
	}
}

// TestRedisBusPublishAfterCloseReportsClosed pins the distinction the
// push handler acts on. Close cancels the bus context, so an unfixed
// post-Close publish fails with a CONTEXT error — indistinguishable, from
// the caller's side, from a request that lost its reply to the network.
// One of those proves nothing was published and the other does not, so
// the sentinel has to come from the closed flag, not from the error text.
//
// The client points at a closed port: if the closed-flag check were
// removed, this would still fail, but with a dial error rather than
// ErrBusClosed — which is exactly the confusion being ruled out.
func TestRedisBusPublishAfterCloseReportsClosed(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	bus := NewRedisBus(client)
	bus.Close()

	err := bus.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("expected ErrBusClosed after Close, got %v", err)
	}
}

// TestRedisBusPublishFailureIsNotClosed is the discriminating leg: a live
// bus whose Redis is unreachable must report an error that is NOT
// ErrBusClosed, because that outcome is UNCONFIRMED (go-redis retries a
// command whose reply was lost, so the script may have run). If this
// returned ErrBusClosed the push endpoint would answer 503 `unavailable`,
// which the web client treats as safe to resend — offering a duplicate
// dispatch.
func TestRedisBusPublishFailureIsNotClosed(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	bus := NewRedisBus(client)
	defer bus.Close()

	err := bus.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"})
	if err == nil {
		t.Fatal("expected an error publishing to an unreachable Redis")
	}
	if errors.Is(err, ErrBusClosed) {
		t.Fatalf("an unreachable-Redis failure must not claim the bus was closed: %v", err)
	}
}
