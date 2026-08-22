package watchevents

import (
	"sync"
	"testing"
)

// BUG-2731's defect, found in this package by a cross-artifact pass on that
// unit's branch. MemoryBus assigns its own ids from 1 on every process start,
// so a client resuming with a cursor from a previous incarnation is asking
// about a sequence this process never had — and an empty buffer answering with
// a non-nil empty slice tells it, through both SSE handlers, that it missed
// nothing.
//
// Both halves asserted: the gap IS produced where continuity cannot be proven,
// and is NOT produced where it can. Without the second, a bus that refused
// every resume would pass.
func TestAColdBufferCannotVouchForAResume(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)

	if got := b.EventsSince(4200); got != nil {
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
	if got := b.EventsSince(1); got == nil {
		t.Fatal("a cursor at our newest notification must be served")
	}
	if got := b.EventsSince(0); got == nil || len(got) != 1 {
		t.Fatalf("a fresh subscriber must receive the buffered notification, got %v", got)
	}
}

// The same rule must reach SubscribeAndReplaySince, which is the path the SSE
// handler actually uses — EventsSince is documented as the standalone
// primitive "for tests and any future caller". Testing only the primitive
// would leave the consumer's path unproven (CONVE-19).
func TestSubscribeAndReplaySinceRefusesAColdResume(t *testing.T) {
	b := New()
	t.Cleanup(b.Close)

	ch, missed := b.SubscribeAndReplaySince(4200)
	if ch == nil {
		t.Fatal("the subscription must still be established even when the replay is refused")
	}
	if missed != nil {
		t.Fatalf("a cold resume must be answered as a gap, got %d notifications", len(missed))
	}

	// Control: a fresh subscriber gets a usable, non-gap answer.
	ch2, missed2 := b.SubscribeAndReplaySince(0)
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
	if got := b.EventsSince(4200); got != nil {
		t.Fatalf("expected a gap, got %d", len(got))
	}
	ch, missed := b.SubscribeAndReplaySince(4200)
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
	if got := b.EventsSince(1); got == nil {
		t.Fatal("a caught-up cursor must be served")
	}
	ch2, missed2 := b.SubscribeAndReplaySince(0)
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
