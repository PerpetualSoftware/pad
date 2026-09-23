package events

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2732: Publish reports its outcome with the same three-way contract
// internal/watchevents' Publish has (BUG-2699): nil is accepted, ErrBusClosed
// is provably not published, and anything else is unconfirmed.

func TestMemoryBusPublishReportsClosedAfterClose(t *testing.T) {
	b := New()
	ch, _, _ := b.Subscribe(context.Background(), "ws-1")

	// Positive control: a live bus accepts, so the assertion below cannot
	// pass on a Publish that always errors.
	if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"}); err != nil {
		t.Fatalf("publish on a live bus: %v", err)
	}
	<-ch

	b.Close()
	err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("publish after Close: got %v, want ErrBusClosed", err)
	}
}

func newTestRedisBusWithServer(t *testing.T) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	return b, mr
}

func TestRedisBusPublishOutcomes(t *testing.T) {
	for _, phase2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("epoch=%v", phase2), func(t *testing.T) {
			t.Run("accepted", func(t *testing.T) {
				b, _ := newTestRedisBusWithServer(t)
				b.publishEpoch = phase2
				if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"}); err != nil {
					t.Fatalf("publish on a live bus: %v", err)
				}
			})

			t.Run("closed", func(t *testing.T) {
				b, _ := newTestRedisBusWithServer(t)
				b.publishEpoch = phase2
				b.Close()
				err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
				if !errors.Is(err, ErrBusClosed) {
					t.Fatalf("publish after Close: got %v, want ErrBusClosed", err)
				}
			})

			// Redis unreachable on a bus that is NOT closed: a failure, and
			// NOT the closed outcome, because the bus cannot prove the event
			// did not go out.
			t.Run("unconfirmed", func(t *testing.T) {
				b, mr := newTestRedisBusWithServer(t)
				b.publishEpoch = phase2
				mr.Close()
				err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
				if err == nil {
					t.Fatal("publish with Redis down reported success")
				}
				if errors.Is(err, ErrBusClosed) {
					t.Fatalf("a transport failure on a live bus must not claim closed: %v", err)
				}
				if got := PublishFailureOutcome(err); got != "unconfirmed" {
					t.Fatalf("outcome: got %q, want unconfirmed", got)
				}
			})
		})
	}
}

func TestPublishFailureOutcomeSeesThroughWrapping(t *testing.T) {
	if got := PublishFailureOutcome(fmt.Errorf("ctx: %w", ErrBusClosed)); got != "closed" {
		t.Fatalf("wrapped ErrBusClosed: got %q, want closed", got)
	}
	if got := PublishFailureOutcome(errors.New("boom")); got != "unconfirmed" {
		t.Fatalf("other error: got %q, want unconfirmed", got)
	}
}
