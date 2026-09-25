package events

import (
	"context"
	"testing"
)

// A CURSOR EQUAL TO THE DISCARDED PEAK IS REFUSED ONCE THE NEW SEQUENCE HAS
// CLIMBED PAST IT (BUG-3206).
//
// A counter-backwards reset raises the floor to the highest id the discarded
// buffers held. newReplayBufferAfterReset used to set it one short, so a cursor
// EXACTLY equal to that peak was still served — on the reasoning that nothing
// above it was buffered for such a client to be missing. That holds only when
// the new sequence starts above the peak, and a counter-backwards reset is by
// definition one where it did not: old 200, new 150, so an old-space client
// at 200 has missed the new 150..200 and was handed 201 as though it followed.
//
// The existing floor test (TestACounterGoingBackwardsRaisesTheFloor) asserts
// cursor 200 is refused only BEFORE the sequence climbs past it, where the
// "cursor newer than newest" check refuses it anyway — so it could not see
// this. Every leg here is read AFTER the climb.
func TestACursorAtTheDiscardedPeakIsRefusedAfterTheClimb(t *testing.T) {
	b := newTestRedisBus(t)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 3, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 0, Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 200 {
		t.Fatalf("fixture: the counter-backwards reset should raise the floor to 200, got %d", b.discardedHighWater)
	}
	for id := int64(151); id <= 202; id++ {
		b.fanOutFromRedis(gen, 0, Event{ID: id, Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	if got := b.EventsSince("ws-1", 200); got != nil {
		t.Fatalf("cursor 200 is the discarded peak — the old space reached it and the new one overlaps it — and must be refused; got %v", eventIDs(got))
	}
	// THE CONTROL: one above the peak is unambiguous and served exactly what
	// follows it. Without this leg, refusing everything would pass.
	got := b.EventsSince("ws-1", 201)
	if len(got) != 1 || got[0].ID != 202 {
		t.Fatalf("cursor 201 is above the discarded peak and must be served [202]; got %v", eventIDs(got))
	}
}

// The refusal reaches the resume path the SSE handler uses, not only the
// local read above (team CONVE-19). A nil replay is the handler's
// sync_required; an empty or partial one is "caught up" or a silent skip.
func TestTheActivityResumePathRefusesTheDiscardedPeak(t *testing.T) {
	b := newTestRedisBus(t)
	_, gen := liveGen(t, b, "ws-1")
	b.fanOutFromRedis(gen, 3, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 0, Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-1"})
	for id := int64(151); id <= 202; id++ {
		b.fanOutFromRedis(gen, 0, Event{ID: id, Type: ItemUpdated, WorkspaceID: "ws-1"})
	}

	ch, missed, _, _ := b.SubscribeAndReplaySince(context.Background(), "ws-1", 200, 0)
	if ch != nil {
		defer b.Unsubscribe(ch)
	}
	if missed != nil {
		t.Fatalf("a resume from the discarded peak must be refused (nil replay → sync_required); got %v", eventIDs(missed))
	}
}

// An epoch change CLEARS the floor on purpose (the loop trade documented at
// dropAllBuffers), so this fix must not reach it: after an epoch change the
// first new id's +1 rule is the whole boundary. Pinned so the constant change
// cannot leak into the branch that declined the floor.
func TestAnEpochChangeStillDeclinesTheFloorAfterBUG3206(t *testing.T) {
	b := newTestRedisBus(t)
	_, gen := liveGen(t, b, "ws-1")
	b.fanOutFromRedis(gen, 3, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 4, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 4, Event{ID: 2, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 4, Event{ID: 3, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 0 {
		t.Fatalf("fixture: an epoch change clears the floor, got %d", b.discardedHighWater)
	}
	got := b.EventsSince("ws-1", 2)
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("after an epoch change a cursor inside the new space is served as before; got %v", eventIDs(got))
	}
}

func eventIDs(es []Event) []int64 {
	if es == nil {
		return nil
	}
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}
