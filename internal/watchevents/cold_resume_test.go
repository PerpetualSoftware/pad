package watchevents

import (
	"sync"
	"testing"
)

// BUG-2731's defect, found in this package by a cross-artifact pass on that
// unit's branch: an empty buffer answering a resume with a non-nil empty slice
// tells the client, through both SSE handlers, that it missed nothing.
//
// THE CURSORS HERE SIT ABOVE THIS INCARNATION'S BASE, and that is load-bearing
// rather than incidental. Since BUG-2736 a cursor at or below base is refused
// one level up, by the incarnation check in EventsSince — so a test using a
// small literal like 4200 would still pass while proving nothing about the
// cold-buffer guard it is named for. Two guards, two reachable cases, tested
// separately.
//
// Both halves asserted: the gap IS produced where continuity cannot be proven,
// and is NOT produced where it can. Without the second, a bus that refused
// every resume would pass.
func TestAColdBufferCannotVouchForAResume(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)

	if got := b.EventsSince(b.base + 4200); got != nil {
		t.Fatalf("a resume against a bus that has published nothing must be a gap, got %d notifications", len(got))
	}

	// A fresh subscriber is not resuming from a position.
	got := b.EventsSince(0)
	if got == nil {
		t.Fatal("sinceID=0 must never be answered with a gap")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 notifications, got %d", len(got))
	}

	// And once the bus has published, ordinary resumes are served — the
	// control that keeps the fix from being "always refuse".
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	all := b.EventsSince(0)
	if len(all) != 1 {
		t.Fatalf("a fresh subscriber must receive the buffered notification, got %v", all)
	}
	if got := b.EventsSince(all[0].ID); got == nil {
		t.Fatal("a cursor at our newest notification must be served")
	}
}

// The incarnation guard, which is the OTHER way a resume can be unservable on
// this bus (BUG-2736). Separated from the cold-buffer test above because the
// two answer different questions and a single test covering both would go on
// passing if either guard were deleted.
func TestACursorFromAPreviousIncarnationIsAGapEvenWithAWarmBuffer(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)

	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// PREMISE: the buffer is warm, so the cold-buffer guard cannot be what
	// answers this. Without the assertion the test would pass on a bus where
	// the publish silently failed.
	all := b.EventsSince(0)
	if len(all) != 1 {
		t.Fatalf("fixture: expected one buffered notification, got %d", len(all))
	}

	// A cursor a PREVIOUS incarnation could have issued: at or below base.
	if got := b.EventsSince(b.base); got != nil {
		t.Fatalf("a cursor at this incarnation's base belongs to the dead space and must be a gap, got %d", len(got))
	}
	if got := b.EventsSince(1); got != nil {
		t.Fatalf("a cursor of 1 — what every pre-BUG-2736 incarnation issued first — must be a gap, got %d", len(got))
	}

	// Control: the first id this incarnation issued is base+1, and a cursor
	// there is served. One away from the refused value, so the boundary is
	// pinned rather than approximated.
	if got := b.EventsSince(b.base + 1); got == nil {
		t.Fatal("the first id this incarnation issued must be servable")
	}

	// AND THE WIRING, not just the component (team CONVE-19). The SSE handler
	// resumes through SubscribeAndReplaySince, not EventsSince; when this
	// guard was written inline in EventsSince the handler's path did not have
	// it, and every assertion above still passed. Both entry points, or the
	// fix is only true of the one nobody calls.
	ch, missed, _ := b.SubscribeAndReplaySince(1)
	defer b.Unsubscribe(ch)
	if ch == nil {
		t.Fatal("the subscription must still be established when the replay is refused")
	}
	if missed != nil {
		t.Fatalf("SubscribeAndReplaySince must refuse a previous incarnation's cursor, got %d notifications", len(missed))
	}
	ch2, missed2, _ := b.SubscribeAndReplaySince(b.base + 1)
	defer b.Unsubscribe(ch2)
	if missed2 == nil {
		t.Fatal("SubscribeAndReplaySince must serve a cursor this incarnation issued")
	}
}

// The same rule must reach SubscribeAndReplaySince, which is the path the SSE
// handler actually uses — EventsSince is documented as the standalone
// primitive "for tests and any future caller". Testing only the primitive
// would leave the consumer's path unproven (CONVE-19).
func TestSubscribeAndReplaySinceRefusesAColdResume(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)

	ch, missed, _ := b.SubscribeAndReplaySince(b.base + 4200)
	if ch == nil {
		t.Fatal("the subscription must still be established even when the replay is refused")
	}
	if missed != nil {
		t.Fatalf("a cold resume must be answered as a gap, got %d notifications", len(missed))
	}

	// Control: a fresh subscriber gets a usable, non-gap answer.
	ch2, missed2, _ := b.SubscribeAndReplaySince(0)
	defer b.Unsubscribe(ch2)
	if ch2 == nil {
		t.Fatal("a fresh subscription must be established")
	}
	if missed2 == nil {
		t.Fatal("sinceID=0 must not be answered as a gap")
	}
	b.Unsubscribe(ch)
}

// codex round 12. The cold-resume fix above sends clients sync_required, and
// MemoryBus reported nothing — RedisBus counts its own unservable resumes, so
// pad_watchevents_resume_gaps_total read ZERO on a single-process deployment
// for a path that genuinely resyncs clients.
//
// A counter that is structurally blind to one of its two implementations is
// worse than an absent one: zero reads as evidence of health.
func TestMemoryBusReportsItsOwnResumeGaps(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)
	obs := &countingObserver{}
	b.SetObserver(obs)

	// Both entry points, because the handler uses SubscribeAndReplaySince and
	// EventsSince is the documented standalone primitive.
	if got := b.EventsSince(b.base + 4200); got != nil {
		t.Fatalf("expected a gap, got %d", len(got))
	}
	ch, missed, _ := b.SubscribeAndReplaySince(b.base + 4200)
	if missed != nil {
		t.Fatalf("expected a gap, got %d", len(missed))
	}
	b.Unsubscribe(ch)

	if got := obs.resumeGaps(); got != 2 {
		t.Fatalf("both unservable resumes must be counted, got %d", got)
	}

	// Control: a served resume is NOT counted, or the counter measures
	// traffic rather than resyncs.
	if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	warm := b.EventsSince(0)
	if len(warm) != 1 {
		t.Fatalf("fixture: expected one buffered notification, got %d", len(warm))
	}
	if got := b.EventsSince(warm[0].ID); got == nil {
		t.Fatal("a caught-up cursor must be served")
	}
	ch2, missed2, _ := b.SubscribeAndReplaySince(0)
	defer b.Unsubscribe(ch2)
	if missed2 == nil {
		t.Fatal("a fresh subscriber must not be answered with a gap")
	}
	if got := obs.resumeGaps(); got != 2 {
		t.Fatalf("served resumes must not be counted, total moved to %d", got)
	}
}

type countingObserver struct {
	mu   sync.Mutex
	gaps int
}

func (o *countingObserver) NotificationDropped(string) {}
func (o *countingObserver) SequenceGap(int64)          {}
func (o *countingObserver) SequenceReset(string)       {}
func (o *countingObserver) ReceiveLoopExited()         {}

func (o *countingObserver) ResumeGap() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gaps++
}

func (o *countingObserver) resumeGaps() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gaps
}
