package watchevents

import "testing"

// BUG-2736, this package's half. MemoryBus assigned notification IDs from a
// counter that restarted at 1 on every process start, so a `pad watch` client
// resuming with a cursor from a previous incarnation could be replayed the NEW
// space's notifications as though they followed the OLD space's.
//
// The asymmetry with RedisBus in this same package is deliberate and is
// documented there: a Redis-backed bus shares its counter across processes, so
// its id space is identified by a shared epoch key rather than by anything one
// process can compute.

func publishN(t *testing.T, b *MemoryBus, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	for _, n := range b.EventsSince(0) {
		ids = append(ids, n.ID)
	}
	return ids
}

func TestRestartCursorIsRefusedBecauseTheIDSpacesAreDisjoint(t *testing.T) {
	// Two BUS INCARNATIONS in one process — a genuine process restart is not
	// reproducible in-process, and a bus takes its base at construction
	// exactly as it does at process start, so the two are equivalent for what
	// is under test here.
	//
	// Incarnation 1: the watcher streams and leaves holding a cursor.
	first := New()
	firstIDs := publishN(t, first, 3)
	first.Close()
	cursor := firstIDs[1]

	// Incarnation 2: the process restarted.
	second := New()
	defer second.Close()
	secondIDs := publishN(t, second, 5)

	// PREMISE, asserted rather than assumed: the spaces must be disjoint, and
	// the dead cursor must sit BELOW the new space — that direction is what
	// makes the foreign-id check the thing that refuses it.
	for _, id := range secondIDs {
		if id == cursor {
			t.Fatalf("id spaces overlap: dead cursor %d was reissued (new ids %v)", cursor, secondIDs)
		}
	}
	if cursor >= secondIDs[0] {
		t.Fatalf("dead cursor %d must sit below the new space's first id %d", cursor, secondIDs[0])
	}

	if got := second.EventsSince(cursor); got != nil {
		t.Fatalf("resuming from a previous incarnation's cursor %d must be a gap, got %d notifications", cursor, len(got))
	}

	// NEGATIVE CONTROL: a cursor from THIS incarnation is still served, with
	// the right notifications — the fix must not be "answer nil always".
	live := secondIDs[1]
	got := second.EventsSince(live)
	if got == nil {
		t.Fatalf("a cursor issued by this incarnation (%d) must still be servable", live)
	}
	if len(got) != len(secondIDs)-2 {
		t.Fatalf("resuming from %d: want %d notifications, got %d", live, len(secondIDs)-2, len(got))
	}
}
