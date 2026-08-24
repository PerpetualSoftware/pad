package events

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisBus(t *testing.T) *RedisBus {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	return b
}

// BUG-2731 case 4: the per-workspace subscription lifecycle. When the last
// local subscriber for a workspace leaves, this instance stops receiving that
// workspace — but the replay buffer used to survive, looking complete, while
// events published on other instances went past it unrecorded.
func TestBufferIsDroppedWhenTheWorkspaceSubscriptionStops(t *testing.T) {
	b := newTestRedisBus(t)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	// Events arrive from Redis while we are covering the workspace.
	b.fanOutLocally(Event{ID: 98, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutLocally(Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})

	// Positive control for the live case: while subscribed, a resume from
	// inside the covered range IS served. Without this the test below would
	// pass on a bus that never buffers anything at all.
	if got := b.EventsSince("ws-1", 98); got == nil {
		t.Fatal("while subscribed, a cursor inside the covered range must be served")
	} else if len(got) != 1 || got[0].ID != 100 {
		t.Fatalf("expected event 100 replayed, got %+v", got)
	}

	// The last local subscriber leaves. This instance is no longer receiving
	// ws-1; ids 101..500 may be published elsewhere and we will not see them.
	b.Unsubscribe(ch)

	if got := b.EventsSince("ws-1", 100); got != nil {
		t.Fatalf("after the subscription stopped, a resume must not be served from the stale buffer; got %d events", len(got))
	}
}

// The straggler: sub.cancel() signals the receive goroutine, it does not join
// it, so a message already in flight can land after the workspace was dropped.
// Appending it would rebuild a one-entry buffer whose knownFrom vouches for
// coverage that ended when the subscription did.
func TestAStragglerAfterUnsubscribeDoesNotRebuildTheBuffer(t *testing.T) {
	b := newTestRedisBus(t)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	b.fanOutLocally(Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.Unsubscribe(ch)

	// In flight when the subscription was cancelled.
	b.fanOutLocally(Event{ID: 101, Type: ItemUpdated, WorkspaceID: "ws-1"})

	// A new client arrives much later, after ids 102..500 were published on
	// other instances, resuming from the straggler's id.
	if got := b.EventsSince("ws-1", 101); got != nil {
		t.Fatalf("a straggler must not rebuild the buffer and vouch for coverage; got %d events", len(got))
	}

	// And the negative control that keeps this honest: once the workspace is
	// genuinely subscribed again, coverage restarts and resumes from the new
	// first-seen id onward are served normally.
	ch2, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch2)
	b.fanOutLocally(Event{ID: 600, Type: ItemUpdated, WorkspaceID: "ws-1"})

	got := b.EventsSince("ws-1", 599)
	if got == nil {
		t.Fatal("after resubscribing, a cursor adjacent to the new first-seen id must be served")
	}
	if len(got) != 1 || got[0].ID != 600 {
		t.Fatalf("expected event 600 replayed, got %+v", got)
	}

	// ...while a cursor from before the outage is still a gap, because the
	// events during it are genuinely unrecoverable here.
	if got := b.EventsSince("ws-1", 101); got != nil {
		t.Fatalf("a pre-outage cursor must remain a gap after resubscription; got %d events", len(got))
	}
}

// A second local subscriber joining and leaving must not drop coverage for the
// first one — the buffer is dropped when the SUBSCRIPTION stops, not when any
// subscriber does.
func TestBufferSurvivesWhileAnyLocalSubscriberRemains(t *testing.T) {
	b := newTestRedisBus(t)

	ch1, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch1)
	ch2, _, _ := b.Subscribe(context.Background(), "ws-1")

	b.fanOutLocally(Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.Unsubscribe(ch2)

	got := b.EventsSince("ws-1", 100)
	if got == nil {
		t.Fatal("coverage must survive while a local subscriber remains; got a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected caught-up, got %d events", len(got))
	}
}

// codex round 1 F2. The straggler test above never resubscribes before the
// stray message lands, so it passes with a fan-out guard that only checks
// "does SOME subscription exist". This one closes that: the workspace is
// resubscribed FIRST, and only then does the old subscription's in-flight
// message arrive.
func TestAStragglerFromAnEndedSubscriptionCannotVouchForTheNewOne(t *testing.T) {
	b := newTestRedisBus(t)

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	oldGen := b.currentSubGen("ws-1")
	b.fanOutFromRedis(oldGen, 0, Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})

	// Everyone leaves; ids 101..500 are published elsewhere, unseen.
	b.Unsubscribe(ch)

	// A new client arrives and the workspace is subscribed again.
	ch2, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch2)
	newGen := b.currentSubGen("ws-1")
	if newGen == oldGen {
		t.Fatal("fixture drifted: resubscribing must produce a new subscription generation")
	}

	// NOW the old receive goroutine's in-flight message lands.
	b.fanOutFromRedis(oldGen, 0, Event{ID: 101, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if got := b.EventsSince("ws-1", 101); got != nil {
		t.Fatalf("a message from an ended subscription must not establish coverage for the new one; got %d events", len(got))
	}

	// Control: a message on the CURRENT subscription does establish coverage.
	b.fanOutFromRedis(newGen, 0, Event{ID: 600, Type: ItemUpdated, WorkspaceID: "ws-1"})
	got := b.EventsSince("ws-1", 600)
	if got == nil {
		t.Fatal("a message on the live subscription must establish coverage")
	}
	if len(got) != 0 {
		t.Fatalf("expected caught-up, got %d events", len(got))
	}
}
