package events

import (
	"sync"
	"testing"
)

type recordingObserver struct {
	mu         sync.Mutex
	resumeGaps []string
	resets     []string
	loopExits  int
	drops      []string
}

func (o *recordingObserver) ResumeGap(workspaceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resumeGaps = append(o.resumeGaps, workspaceID)
}

func (o *recordingObserver) SequenceReset(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resets = append(o.resets, reason)
}

func (o *recordingObserver) ReceiveLoopExited() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loopExits++
}

func (o *recordingObserver) EventDropped(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drops = append(o.drops, reason)
}

func (o *recordingObserver) resetReasons() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.resets...)
}

func (o *recordingObserver) dropped() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.drops...)
}

func (o *recordingObserver) exits() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.loopExits
}

func (o *recordingObserver) snapshot() ([]string, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.resumeGaps...), append([]string(nil), o.resets...)
}

// BOTH WAYS OF FAILING TO SERVE A RESUME MUST REACH THE COUNTER. This is the
// specific shape that went wrong on the watch bus's own gap metric, where one
// of two conditions producing sync_required reported nothing — a metric gap
// that prose claimed did not exist. Here the two are "the workspace has no
// buffer" and "the buffer refused the span", and they are structurally
// different branches, so a suite that exercises one proves nothing about the
// other.
func TestResumeGapIsReportedForBothWaysOfNotServing(t *testing.T) {
	obs := &recordingObserver{}
	bus := New()
	bus.SetObserver(obs)

	// BOTH CURSORS ARE INSIDE THIS INCARNATION'S SPACE, which is what keeps
	// the two halves distinct. A small literal would be refused by the
	// incarnation guard before either branch below was reached (BUG-2736), so
	// this test would pass with BOTH of the branches it names deleted.

	// Half one: no buffer for this workspace at all.
	if got := bus.EventsSince("ws-cold", bus.base+4200); got != nil {
		t.Fatalf("expected a gap, got %d events", len(got))
	}

	// Half two: a buffer exists, and refuses the span. ws-other's ids push the
	// shared counter up, so a cursor among them is a real position in this
	// incarnation with room for a missed ws-warm event above it.
	other := publishN(bus, "ws-other", 49)
	bus.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-warm"})
	// PREMISE: ws-warm really does have a buffer, or the call below takes the
	// no-buffer path and this half silently duplicates half one.
	if warm := bus.EventsSince("ws-warm", 0); len(warm) != 1 {
		t.Fatalf("fixture: ws-warm must hold exactly one buffered event, got %d", len(warm))
	}
	if got := bus.EventsSince("ws-warm", other[29]); got != nil {
		t.Fatalf("expected a gap, got %d events", len(got))
	}

	gaps, _ := obs.snapshot()
	if len(gaps) != 2 {
		t.Fatalf("expected 2 resume gaps (no-buffer AND refused-span), got %d: %v", len(gaps), gaps)
	}
	if gaps[0] != "ws-cold" || gaps[1] != "ws-warm" {
		t.Fatalf("resume gaps carry the wrong workspace: %v", gaps)
	}
}

// The negative control, and the one that matters for the load posture: a
// resume this instance CAN serve must not be counted. Without it, a bus that
// reported unconditionally would satisfy the test above and make the metric
// useless for exactly the question it exists to answer.
func TestServedResumesAreNotReportedAsGaps(t *testing.T) {
	obs := &recordingObserver{}
	bus := New()
	bus.SetObserver(obs)

	ids := publishN(bus, "ws-1", 5)

	if got := bus.EventsSince("ws-1", ids[2]); got == nil {
		t.Fatal("a cursor inside the buffer must be served")
	}
	if got := bus.EventsSince("ws-1", ids[4]); got == nil {
		t.Fatal("a caught-up cursor must be served")
	}
	// A fresh client on a workspace with no events is not resuming at all.
	if got := bus.EventsSince("ws-empty", 0); got == nil {
		t.Fatal("sinceID=0 must never be a gap")
	}

	gaps, _ := obs.snapshot()
	if len(gaps) != 0 {
		t.Fatalf("served resumes must not be counted as gaps, got %v", gaps)
	}
}

// An observer may call back into the bus: every report fires with no bus lock
// held. Asserted rather than documented, because the property is invisible
// until it deadlocks a production receive loop.
func TestAnObserverMayCallBackIntoTheBus(t *testing.T) {
	bus := New()
	done := make(chan struct{})
	bus.SetObserver(callbackObserver{bus: bus, done: done})

	// Triggers a resume gap, whose report calls Publish from inside. Which of
	// the gap branches answers does not matter here — only that one does —
	// but the cursor is base-relative anyway so the reason stays legible.
	_ = bus.EventsSince("ws-1", bus.base+99)

	select {
	case <-done:
	default:
		t.Fatal("the observer's call back into the bus did not complete")
	}
}

type callbackObserver struct {
	bus  *MemoryBus
	done chan struct{}
}

func (o callbackObserver) ResumeGap(string) {
	o.bus.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-callback"})
	close(o.done)
}

func (o callbackObserver) SequenceReset(string) {}

func (o callbackObserver) EventDropped(string) {}

func (o callbackObserver) ReceiveLoopExited() {}
