package watchevents

import (
	"context"
	"testing"
)

// A CURSOR INSIDE AN ABANDONED ID SPACE IS REFUSED UNTIL THE NEW SPACE HAS
// CLIMBED OUT OF IT (BUG-2743, which BUG-2728 is one member of).
//
// When the watch stream's id space restarts — the counter evicted or lost to a
// FLUSHDB, a Redis restored from an older snapshot, the epoch rotating with the
// counter — old and new ids OVERLAP. A client's Last-Event-ID of 3 could be
// either space's 3, and nothing in the number says which. Every arm that reacts
// to a restart used to set the coverage boundary from the FIRST NEW id it saw,
// so a cursor at or above that id was served: an old-space cursor equal to it
// was told it was caught up (BUG-2728's report), and one above it was handed
// the new space's later ids as though they followed it. Either way the client
// never learned it had missed everything the new space published below its
// cursor.
//
// The matrix is one row per ARM that moves knownFrom, each driven by the cause
// that reaches it in production. Every row asserts:
//
//   - cursors inside the overlap are REFUSED (nil — the handler's
//     sync_required), including the one equal to the first new id;
//   - a control cursor ABOVE the old space's extent is still SERVED, with the
//     ids above it — the leg that stops "refuse everything after a reset"
//     from passing.
//
// Asserted through EventsSince, which answers from local state only. The
// shared-counter check in the resume path cannot see any of this: after the
// restart the counter and this instance's high-water mark AGREE, both in the
// new space. The wiring through SubscribeAndReplaySince is the last test.
func TestACursorInsideAnAbandonedIDSpaceIsRefused(t *testing.T) {
	t.Parallel()

	// The old space reached 10 on every row. The new space's ids are chosen
	// so that it restarts BELOW that and then climbs past it, to 12.
	const oldPeak = 10

	type step struct {
		epoch string
		ids   []int64
		drop  bool // end coverage first, as a resubscription does
	}
	for _, tc := range []struct {
		name  string
		steps []step
		// cursors inside the overlap: each must be refused.
		refused []int64
	}{
		{
			// FLUSHDB (or both keys evicted): the epoch key is gone, so the
			// next publish mints a new epoch, and the counter restarts at 1.
			// The epoch arm, then the cold-start arm's +1.
			name: "epoch rotation with the counter restarted",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-two", ids: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			},
			// 1 is BUG-2728's own case: equal to the first new id.
			refused: []int64{1, 3, 5, 10},
		},
		{
			// The same, noticed after the connection dropped: highWaterID
			// survives the drop, so the epoch arm still knows the old extent.
			name: "epoch rotation seen across a coverage drop",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-two", ids: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, drop: true},
			},
			refused: []int64{1, 3, 5, 10},
		},
		{
			// The counter key alone evicted under maxmemory, or restored from
			// an older snapshot while connected: same epoch, ids go backwards.
			// The connected counter-backward arm.
			name: "counter reset while connected",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-one", ids: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			},
			refused: []int64{1, 3, 5, 10},
		},
		{
			// A Redis restarted from a stale snapshot: the restart drops the
			// connection (coverage ends), and the restored counter is lower.
			// The cold-start arm that recognises a reset.
			name: "counter reset across a coverage drop",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-one", ids: []int64{3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, drop: true},
			},
			refused: []int64{3, 5, 10},
		},
		{
			// After a reset, a GAP in the new space re-sets the boundary with
			// the ordinary knownFrom = n.ID, which lowers it back into the
			// overlap: cursor 2 was admitted and handed the new 3.
			name: "a gap after a counter reset",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-one", ids: []int64{1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			},
			refused: []int64{2, 3, 10},
		},
		{
			// A SECOND restart whose old space (the first restart's new one,
			// peaking at 5) sits below the first's peak of 10. The boundary
			// is raised, never lowered: cursor 8 could still be the ORIGINAL
			// space's 8, and a boundary overwritten down to 5 would serve it.
			name: "a second restart below the first one's peak",
			steps: []step{
				{epoch: "epoch-one", ids: []int64{8, 9, 10}},
				{epoch: "epoch-one", ids: []int64{1, 2, 3, 4, 5}},
				{epoch: "epoch-one", ids: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
			},
			refused: []int64{3, 8, 10},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newLocalOnlyBus(64)
			defer b.Close()
			for _, s := range tc.steps {
				if s.drop {
					b.dropCoverage(ResetReasonSubscriptionResumed)
				}
				for _, id := range s.ids {
					b.fanOutFromRedis(s.epoch, Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())
				}
			}

			for _, c := range tc.refused {
				if got := b.EventsSince(c); got != nil {
					t.Errorf("cursor %d is inside the abandoned id space (old peak %d) and must be refused; got %d notification(s) %v",
						c, oldPeak, len(got), ids(got))
				}
			}

			// THE CONTROL: above the old space's extent nothing is ambiguous,
			// so the resume is served — and served exactly the id above it.
			got := b.EventsSince(oldPeak + 1)
			if len(got) != 1 || got[0].ID != oldPeak+2 {
				t.Fatalf("cursor %d is above the old space and must be served [%d]; got %v",
					oldPeak+1, oldPeak+2, ids(got))
			}
		})
	}
}

// An epoch rotation that did NOT restart the counter — the epoch key evicted on
// its own — leaves the two spaces disjoint: every new id is above the old peak.
// Nothing is ambiguous, and the boundary must not refuse anything the ordinary
// epoch rule serves. The control for the row above that looks most like it.
func TestAnEpochRotationWithoutAnOverlapRefusesNothingExtra(t *testing.T) {
	t.Parallel()
	b := newLocalOnlyBus(64)
	defer b.Close()
	for _, id := range []int64{8, 9, 10} {
		b.fanOutFromRedis("epoch-one", Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())
	}
	for _, id := range []int64{11, 12, 13} {
		b.fanOutFromRedis("epoch-two", Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())
	}
	// 12 is the first cursor the epoch rule itself serves (knownFrom = 11+1).
	if got := b.EventsSince(12); len(got) != 1 || got[0].ID != 13 {
		t.Fatalf("cursor 12 is inside the new space only and must be served [13]; got %v", ids(got))
	}
}

// The refusal must reach the one caller whose answer a client depends on: the
// resume path the SSE handler uses (team CONVE-19 — the local read above
// vouches for the rule, not for its binding). A nil replay is what the handler
// turns into sync_required; an empty non-nil one is "caught up".
func TestTheResumePathRefusesACursorInsideAnAbandonedIDSpace(t *testing.T) {
	t.Parallel()
	b := newLocalOnlyBus(64)
	defer b.Close()
	for _, id := range []int64{8, 9, 10} {
		b.fanOutFromRedis("epoch-one", Notification{ID: id, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())
	}
	// The new epoch's first id, which is BUG-2728's collision value.
	b.fanOutFromRedis("epoch-two", Notification{ID: 1, Kind: KindComment, ItemRef: "TASK-1"}, b.currentGen())

	ch, missed, _, err := b.SubscribeAndReplaySince(context.Background(), 1)
	if err != nil {
		t.Fatalf("SubscribeAndReplaySince: %v", err)
	}
	defer b.Unsubscribe(ch)
	if missed != nil {
		t.Fatalf("a resume from 1 after the id space restarted under an old peak of 10 must be refused (nil replay → sync_required); got %d notification(s) — a non-nil empty slice here is the handler's \"caught up\"",
			len(missed))
	}
}

func ids(ns []Notification) []int64 {
	if ns == nil {
		return nil
	}
	out := make([]int64, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}
